package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"bruce-go/internal/jsengine"
	"bruce-go/internal/tool"
)

// storageRequest is the JSON shape every storage host function accepts.
//
// One object argument rather than positional parameters, so the host boundary
// is a single uniform JSON value and a scope or key can be omitted without
// changing the arity.
type storageRequest struct {
	Scope string          `json:"scope"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func decodeStorageRequest(args json.RawMessage) (storageRequest, error) {
	var request storageRequest
	decoder := json.NewDecoder(strings.NewReader(string(args)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return storageRequest{}, fmt.Errorf("storage arguments must be an object with scope, key and value: %w", err)
	}
	if strings.TrimSpace(request.Scope) == "" {
		return storageRequest{}, errors.New("storage requires a scope")
	}
	return request, nil
}

// storageScope converts a JavaScript scope name into the Go scope.
func storageScope(raw string) (StorageScope, error) {
	scope := StorageScope(strings.TrimSpace(raw))
	switch scope {
	case ScopeInvocation, ScopeSession, ScopePlugin, ScopeWorkspace, ScopeGlobal:
		return scope, nil
	default:
		return "", fmt.Errorf("unknown storage scope %q", raw)
	}
}

// storageHandle returns the namespaced storage for one plugin.
//
// Namespacing happens here, once, so no plugin can address another plugin's
// data: the handle it receives is already scoped to its own name and there is
// no argument that widens it.
func (m *Manager) storageHandle(pluginName string) Storage {
	return m.storage.ForPlugin(pluginName)
}

func (m *Manager) storageGet(pluginName string) jsengine.HostFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		request, err := decodeStorageRequest(args)
		if err != nil {
			return nil, err
		}
		scope, err := storageScope(request.Scope)
		if err != nil {
			return nil, err
		}
		value, found, err := m.storageHandle(pluginName).Get(ctx, scope, request.Key)
		if err != nil {
			return nil, err
		}
		if !found {
			return json.RawMessage(`{"found":false}`), nil
		}
		encoded, err := json.Marshal(map[string]any{"found": true, "value": value})
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}
}

func (m *Manager) storageSet(pluginName string) jsengine.HostFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		request, err := decodeStorageRequest(args)
		if err != nil {
			return nil, err
		}
		scope, err := storageScope(request.Scope)
		if err != nil {
			return nil, err
		}
		if len(request.Value) == 0 {
			return nil, errors.New("storage.set requires a value")
		}
		if err := m.storageHandle(pluginName).Set(ctx, scope, request.Key, request.Value); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
}

func (m *Manager) storageDelete(pluginName string) jsengine.HostFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		request, err := decodeStorageRequest(args)
		if err != nil {
			return nil, err
		}
		scope, err := storageScope(request.Scope)
		if err != nil {
			return nil, err
		}
		if err := m.storageHandle(pluginName).Delete(ctx, scope, request.Key); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
}

func (m *Manager) storageKeys(pluginName string) jsengine.HostFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		request, err := decodeStorageRequest(args)
		if err != nil {
			return nil, err
		}
		scope, err := storageScope(request.Scope)
		if err != nil {
			return nil, err
		}
		keys, err := m.storageHandle(pluginName).Keys(ctx, scope)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(map[string]any{"keys": keys})
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}
}

// eventsEmit publishes a plugin observation event.
//
// Emitting is a capability, not a right: without the events permission the
// call is refused, so a plugin cannot inject events into Bruce's bus.
func (m *Manager) eventsEmit(pluginName string, grant Grant) jsengine.HostFunc {
	return func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
		if !grant.Allows(PermissionEvents) {
			denial := &Denial{Plugin: pluginName, Permission: PermissionEvents, Reason: "the plugin did not request the events permission, or the host policy did not grant it"}
			m.emit(EventDenied, map[string]any{
				"plugin": pluginName, "permission": string(PermissionEvents), "capability": "events.emit",
			})
			return nil, denial
		}
		var payload struct {
			Name    string          `json:"name"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(args, &payload); err != nil {
			return nil, fmt.Errorf("events.emit requires an object with name and payload: %w", err)
		}
		name := strings.TrimSpace(payload.Name)
		if name == "" {
			return nil, errors.New("events.emit requires a non-empty name")
		}
		// The event name is namespaced by plugin, so a plugin cannot forge
		// another plugin's or a built-in event.
		m.emit("plugin.event", map[string]any{
			"plugin": pluginName, "name": name, "payload": string(payload.Payload),
		})
		return json.RawMessage(`{"ok":true}`), nil
	}
}

// validateAgainstSchema returns a validator for a tool's declared JSON Schema.
//
// The manifest is the source of truth for a tool's input, and the model is
// asked to satisfy it, but a model can still emit an argument object that does
// not match. Checking the declared schema before the call means a plugin never
// has to defend itself against malformed input, and a hook that rewrites the
// arguments is checked against the same schema as the original call.
//
// The supported keywords are the ones a tool schema actually uses. Anything
// else is ignored rather than rejected, so a richer schema still loads; the
// point is to catch a missing or wrongly-typed argument, not to be a complete
// JSON Schema implementation.
func validateAgainstSchema(schema json.RawMessage) func(tool.Args) error {
	spec, err := parseSchema(schema)
	if err != nil || spec == nil {
		return nil
	}
	return func(args tool.Args) error {
		return spec.validate(args, "")
	}
}

type schemaSpec struct {
	Type       string                 `json:"type"`
	Required   []string               `json:"required"`
	Properties map[string]*schemaSpec `json:"properties"`
	Items      *schemaSpec            `json:"items"`
	Enum       []any                  `json:"enum"`
	Additional *bool                  `json:"additionalProperties"`
	MinLength  *int                   `json:"minLength"`
	Minimum    *float64               `json:"minimum"`
	Maximum    *float64               `json:"maximum"`
}

func parseSchema(schema json.RawMessage) (*schemaSpec, error) {
	trimmed := strings.TrimSpace(string(schema))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var spec schemaSpec
	if err := json.Unmarshal(schema, &spec); err != nil {
		return nil, err
	}
	if spec.Type != "object" && spec.Type != "" {
		// Only object schemas describe tool arguments; anything else carries no
		// constraint the executor can enforce.
		return nil, nil
	}
	return &spec, nil
}

func (s *schemaSpec) validate(value any, path string) error {
	if s == nil {
		return nil
	}
	if len(s.Enum) > 0 && !enumContains(s.Enum, value) {
		return fmt.Errorf("%s must be one of %s", fieldPath(path), enumText(s.Enum))
	}
	switch s.Type {
	case "", "any":
		return nil
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", fieldPath(path))
		}
		for _, name := range s.Required {
			item, present := object[name]
			if !present || item == nil {
				return fmt.Errorf("%s is required", fieldPath(joinPath(path, name)))
			}
		}
		if s.Additional != nil && !*s.Additional {
			for name := range object {
				if _, declared := s.Properties[name]; !declared {
					return fmt.Errorf("%s is not an accepted argument", fieldPath(joinPath(path, name)))
				}
			}
		}
		for name, property := range s.Properties {
			item, present := object[name]
			if !present || item == nil {
				continue
			}
			if err := property.validate(item, joinPath(path, name)); err != nil {
				return err
			}
		}
		return nil
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", fieldPath(path))
		}
		for i, item := range array {
			if err := s.Items.validate(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", fieldPath(path))
		}
		if s.MinLength != nil && len(text) < *s.MinLength {
			return fmt.Errorf("%s must be at least %d characters", fieldPath(path), *s.MinLength)
		}
		return nil
	case "number", "integer":
		number, err := numericValue(value)
		if err != nil {
			return fmt.Errorf("%s must be a %s", fieldPath(path), s.Type)
		}
		if s.Type == "integer" && number != float64(int64(number)) {
			return fmt.Errorf("%s must be an integer", fieldPath(path))
		}
		if s.Minimum != nil && number < *s.Minimum {
			return fmt.Errorf("%s must be at least %v", fieldPath(path), *s.Minimum)
		}
		if s.Maximum != nil && number > *s.Maximum {
			return fmt.Errorf("%s must be at most %v", fieldPath(path), *s.Maximum)
		}
		return nil
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", fieldPath(path))
		}
		return nil
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", fieldPath(path))
		}
		return nil
	default:
		return nil
	}
}

// numericValue accepts the representations the argument model can hold: a
// json.Number from ParseArguments, or a float64 from a caller that built the
// map by hand.
func numericValue(value any) (float64, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.Float64()
	case float64:
		return typed, nil
	case float32:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	default:
		return 0, errors.New("not a number")
	}
}

func enumContains(values []any, value any) bool {
	for _, candidate := range values {
		if fmt.Sprint(candidate) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

func enumText(values []any) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		parts = append(parts, string(encoded))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func fieldPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "tool arguments"
	}
	return "tool argument " + path
}
