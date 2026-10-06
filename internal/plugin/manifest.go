package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Manifest is the static, pre-execution description of a plugin.
//
// A manifest is validated completely before any plugin code runs. A manifest
// that half-works is worse than one that fails: the author believes a
// declaration took effect while Bruce silently ignored it. Every validation
// failure is therefore a *ManifestError carrying the plugin name, the manifest
// path and the offending field.
type Manifest struct {
	APIVersion  string               `json:"apiVersion"`
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description"`
	Entry       string               `json:"entry"`
	Tools       []ToolDeclaration    `json:"tools,omitempty"`
	Hooks       []HookDeclaration    `json:"hooks,omitempty"`
	Commands    []CommandDeclaration `json:"commands,omitempty"`
	Permissions []Permission         `json:"permissions,omitempty"`
	Concurrency Concurrency          `json:"concurrency"`
	Metadata    map[string]string    `json:"metadata,omitempty"`

	// RootDir is the absolute plugin directory; File is the manifest path;
	// Source records which search root produced it.
	//
	// These three are host bookkeeping, never manifest input: a manifest that
	// declares them is rejected as an unknown field.
	RootDir string `json:"rootDir"`
	File    string `json:"file"`
	Source  Source `json:"source"`
}

// ToolDeclaration describes one tool a plugin registers.
type ToolDeclaration struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Handler       string          `json:"handler"`
	Schema        json.RawMessage `json:"schema"`
	Permissions   []Permission    `json:"permissions,omitempty"`
	ParallelSafe  *bool           `json:"parallelSafe,omitempty"`
	TimeoutMS     int             `json:"timeoutMs,omitempty"`
	Risk          string          `json:"risk,omitempty"`
	PromptSnippet string          `json:"promptSnippet,omitempty"`
}

// HookDeclaration describes one hook a plugin registers.
//
// The event name decides the kind: an observer event is notified after the
// fact, an interceptor event may modify or block the flow.
type HookDeclaration struct {
	Event     string `json:"event"`
	Handler   string `json:"handler"`
	TimeoutMS int    `json:"timeoutMs,omitempty"`
}

// CommandDeclaration describes one slash command a plugin registers.
type CommandDeclaration struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Handler     string       `json:"handler"`
	Usage       string       `json:"usage,omitempty"`
	Permissions []Permission `json:"permissions,omitempty"`
	TimeoutMS   int          `json:"timeoutMs,omitempty"`
}

// Concurrency describes how many runtimes a plugin may occupy and whether its
// handlers may run in parallel with other tools.
type Concurrency struct {
	// MaxRuntimes is the runtime pool capacity; 0 means the host default.
	MaxRuntimes int `json:"maxRuntimes"`
	// ParallelSafe is the plugin-wide default for tool declarations that do
	// not set their own.
	ParallelSafe *bool `json:"parallelSafe,omitempty"`
}

const (
	maxPluginNameLength  = 64
	maxToolNameLength    = 64
	maxCommandNameLength = 32
	maxRuntimesLimit     = 64
)

var (
	pluginNamePattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	pluginVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	toolNamePattern      = regexp.MustCompile(`^[a-z0-9_]+$`)
	commandNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	windowsDrivePattern  = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	unknownFieldPattern  = regexp.MustCompile(`json: unknown field "([^"]+)"`)
)

// defaultToolSchema is the argument schema of a tool that declares none: an
// object accepting any property. It is always a copy, never shared, so a
// caller cannot mutate the default for every other tool.
var defaultToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// observerHookEvents are events a hook may only observe. Sorted.
var observerHookEvents = []string{
	"message.created",
	"session.ended",
	"session.started",
	"tool.completed",
	"tool.started",
}

// interceptorHookEvents are events a hook may intercept, modify or block. Sorted.
var interceptorHookEvents = []string{
	"chat.before",
	"tool.after",
	"tool.before",
}

// validToolRisks are the risk classifications a tool declaration may carry.
// Sorted.
var validToolRisks = []string{"high", "low", "medium", "safe"}

// ObserverHookEvents returns the observer hook events this build accepts.
func ObserverHookEvents() []string {
	return append([]string(nil), observerHookEvents...)
}

// InterceptorHookEvents returns the interceptor hook events this build accepts.
func InterceptorHookEvents() []string {
	return append([]string(nil), interceptorHookEvents...)
}

// IsHookEvent reports whether event is a hook event this build accepts.
func IsHookEvent(event string) bool {
	return IsObserverHookEvent(event) || IsInterceptorHookEvent(event)
}

// IsObserverHookEvent reports whether event is an observer hook event.
func IsObserverHookEvent(event string) bool {
	return containsString(observerHookEvents, event)
}

// IsInterceptorHookEvent reports whether event is an interceptor hook event.
func IsInterceptorHookEvent(event string) bool {
	return containsString(interceptorHookEvents, event)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ParseManifest validates raw manifest bytes. Every failure is a *ManifestError.
//
// path locates the manifest on disk. It is used for diagnostics and to resolve
// entry: both supported layouts place the entry relative to the directory that
// holds the manifest file, which is the plugin root for <name>.json and the
// plugin directory for <name>/plugin.json.
func ParseManifest(data []byte, path string) (*Manifest, error) {
	raw, err := decodeManifest(data, path)
	if err != nil {
		return nil, err
	}
	rootDir := filepath.Dir(path)
	manifest := &Manifest{
		APIVersion:  raw.APIVersion,
		Version:     raw.Version,
		Description: raw.Description,
		Entry:       raw.Entry,
		Metadata:    raw.Metadata,
		RootDir:     rootDir,
		File:        path,
	}
	if raw.Concurrency != nil {
		manifest.Concurrency = Concurrency{
			MaxRuntimes:  raw.Concurrency.MaxRuntimes,
			ParallelSafe: raw.Concurrency.ParallelSafe,
		}
	}
	validator := &manifestValidator{
		path:    path,
		plugin:  strings.TrimSpace(raw.Name),
		rootDir: rootDir,
	}
	if err := validator.validate(manifest, raw); err != nil {
		return nil, err
	}
	return manifest, nil
}

// LoadManifest reads and parses one manifest file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ManifestError{Path: path, Message: "failed to read manifest: " + err.Error()}
	}
	return ParseManifest(data, path)
}

// manifestFile is the wire form of a manifest.
//
// It is deliberately separate from Manifest: host bookkeeping fields (rootDir,
// file, source) must not be settable from JSON, and DisallowUnknownFields can
// then reject any field this schema does not define.
type manifestFile struct {
	APIVersion  string                   `json:"apiVersion"`
	Name        string                   `json:"name"`
	Version     string                   `json:"version"`
	Description string                   `json:"description"`
	Entry       string                   `json:"entry"`
	Tools       []toolDeclarationFile    `json:"tools"`
	Hooks       []hookDeclarationFile    `json:"hooks"`
	Commands    []commandDeclarationFile `json:"commands"`
	Permissions []string                 `json:"permissions"`
	Concurrency *concurrencyFile         `json:"concurrency"`
	Metadata    map[string]string        `json:"metadata"`
}

type toolDeclarationFile struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Handler       string          `json:"handler"`
	Schema        json.RawMessage `json:"schema"`
	Permissions   []string        `json:"permissions"`
	ParallelSafe  *bool           `json:"parallelSafe"`
	TimeoutMS     *int            `json:"timeoutMs"`
	Risk          string          `json:"risk"`
	PromptSnippet string          `json:"promptSnippet"`
}

type hookDeclarationFile struct {
	Event     string `json:"event"`
	Handler   string `json:"handler"`
	TimeoutMS *int   `json:"timeoutMs"`
}

type commandDeclarationFile struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Handler     string   `json:"handler"`
	Usage       string   `json:"usage"`
	Permissions []string `json:"permissions"`
	TimeoutMS   *int     `json:"timeoutMs"`
}

type concurrencyFile struct {
	MaxRuntimes  int   `json:"maxRuntimes"`
	ParallelSafe *bool `json:"parallelSafe"`
}

// decodeManifest parses the manifest object strictly.
//
// Unknown fields are rejected rather than ignored: a misspelled "timeoutMs" or
// "parallelSafe" would otherwise leave the author believing a declaration took
// effect while the host used the default.
func decodeManifest(data []byte, path string) (*manifestFile, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, &ManifestError{Path: path, Message: "manifest is empty"}
	}
	if trimmed[0] != '{' {
		return nil, &ManifestError{Path: path, Message: "manifest must be a JSON object"}
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var raw manifestFile
	if err := decoder.Decode(&raw); err != nil {
		return nil, decodeFailure(data, path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, &ManifestError{
			Plugin:  attributePluginName(data),
			Path:    path,
			Message: "unexpected content after the manifest object",
		}
	}
	if err := checkFieldNames(trimmed, path, attributePluginName(data)); err != nil {
		return nil, err
	}
	return &raw, nil
}

// fieldShape describes the exact field names one level of the manifest schema
// defines. items is set when the value is an array of objects, freeForm when
// the object's keys are author-chosen (metadata).
type fieldShape struct {
	fields   map[string]*fieldShape
	items    *fieldShape
	freeForm bool
}

// scalarShape is a leaf: a string, number, boolean or array of scalars.
func scalarShape() *fieldShape { return &fieldShape{} }

var manifestFieldShape = &fieldShape{fields: map[string]*fieldShape{
	"apiVersion":  scalarShape(),
	"name":        scalarShape(),
	"version":     scalarShape(),
	"description": scalarShape(),
	"entry":       scalarShape(),
	"permissions": scalarShape(),
	"metadata":    {freeForm: true},
	"concurrency": {fields: map[string]*fieldShape{
		"maxRuntimes":  scalarShape(),
		"parallelSafe": scalarShape(),
	}},
	"tools": {items: &fieldShape{fields: map[string]*fieldShape{
		"name":          scalarShape(),
		"description":   scalarShape(),
		"handler":       scalarShape(),
		"schema":        scalarShape(),
		"permissions":   scalarShape(),
		"parallelSafe":  scalarShape(),
		"timeoutMs":     scalarShape(),
		"risk":          scalarShape(),
		"promptSnippet": scalarShape(),
	}}},
	"hooks": {items: &fieldShape{fields: map[string]*fieldShape{
		"event":     scalarShape(),
		"handler":   scalarShape(),
		"timeoutMs": scalarShape(),
	}}},
	"commands": {items: &fieldShape{fields: map[string]*fieldShape{
		"name":        scalarShape(),
		"description": scalarShape(),
		"handler":     scalarShape(),
		"usage":       scalarShape(),
		"permissions": scalarShape(),
		"timeoutMs":   scalarShape(),
	}}},
}}

// checkFieldNames rejects a field whose spelling differs from the schema only
// by case.
//
// encoding/json matches object keys to struct tags case-insensitively as a
// fallback, so "timeoutMS" and "parallelsafe" decode successfully and
// DisallowUnknownFields never sees them. That is exactly the silent-typo class
// this package promises to reject: the author writes "timeoutMS", the host
// reads zero, and the declaration appears to be ignored for no stated reason.
func checkFieldNames(data []byte, path, plugin string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil // decodeFailure already reported this manifest.
	}
	return checkShape(generic, manifestFieldShape, "", path, plugin)
}

func checkShape(value any, shape *fieldShape, prefix, path, plugin string) error {
	switch typed := value.(type) {
	case map[string]any:
		if shape.freeForm {
			return nil
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		// Map iteration is random; sort so the reported typo is stable.
		sort.Strings(keys)
		for _, key := range keys {
			child, declared := shape.fields[key]
			if !declared {
				if canonical, ok := caseInsensitiveMatch(shape.fields, key); ok {
					return &ManifestError{
						Plugin: plugin,
						Path:   path,
						Field:  joinField(prefix, key),
						Message: fmt.Sprintf(
							"unknown field %q: did you mean %q? manifest field names are case-sensitive",
							key, canonical),
					}
				}
				continue // a genuinely unknown field was already rejected.
			}
			if err := checkShape(typed[key], child, joinField(prefix, key), path, plugin); err != nil {
				return err
			}
		}
	case []any:
		// A leaf shape (a string list, a raw JSON Schema) has no element shape
		// to check, so an array under it is left alone.
		if shape.items == nil {
			return nil
		}
		for index, item := range typed {
			if err := checkShape(item, shape.items, fmt.Sprintf("%s[%d]", prefix, index), path, plugin); err != nil {
				return err
			}
		}
	}
	return nil
}

// caseInsensitiveMatch finds the declared field a misspelled key folded onto.
func caseInsensitiveMatch(fields map[string]*fieldShape, key string) (string, bool) {
	folded := strings.ToLower(key)
	matches := make([]string, 0, 1)
	for declared := range fields {
		if declared != key && strings.ToLower(declared) == folded {
			matches = append(matches, declared)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[0], true
}

func joinField(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// decodeFailure turns an encoding/json error into a stable, actionable
// *ManifestError. The plugin name is recovered leniently so that even a
// malformed manifest is attributed to the plugin it claims to be.
func decodeFailure(data []byte, path string, err error) error {
	plugin := attributePluginName(data)
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return &ManifestError{
			Plugin:  plugin,
			Path:    path,
			Message: fmt.Sprintf("malformed JSON at byte %d: %s", syntaxErr.Offset, syntaxErr.Error()),
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return &ManifestError{Plugin: plugin, Path: path, Message: "malformed JSON: the object is not complete"}
	}
	if match := unknownFieldPattern.FindStringSubmatch(err.Error()); match != nil {
		return &ManifestError{
			Plugin:  plugin,
			Path:    path,
			Field:   match[1],
			Message: "unknown field: the manifest schema does not define it; unknown fields are rejected so that a typo cannot be silently ignored",
		}
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return &ManifestError{
			Plugin:  plugin,
			Path:    path,
			Field:   typeErr.Field,
			Message: "must be " + jsonTypeDescription(typeErr.Type),
		}
	}
	return &ManifestError{Plugin: plugin, Path: path, Message: "malformed JSON: " + err.Error()}
}

// attributePluginName recovers the declared name from a manifest that failed
// strict decoding, so the diagnostic still says which plugin is broken.
//
// The name is read by scanning tokens rather than by unmarshalling the whole
// object: a syntax error further down the file (a broken nested schema, say)
// must not cost the diagnostic its plugin identity. The scan stops at the
// first error and reports whatever it found up to that point.
func attributePluginName(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return ""
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return ""
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return ""
		}
		name, ok := key.(string)
		if !ok {
			return ""
		}
		if name != "name" {
			// Skip this key's value, which may itself be a nested object.
			if err := skipValue(decoder); err != nil {
				return ""
			}
			continue
		}
		value, err := decoder.Token()
		if err != nil {
			return ""
		}
		text, ok := value.(string)
		if !ok {
			return ""
		}
		return strings.TrimSpace(text)
	}
	return ""
}

// skipValue consumes exactly one JSON value from the decoder.
func skipValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil // a scalar is one token
	}
	switch delimiter {
	case '{', '[':
	default:
		return nil
	}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func jsonTypeDescription(kind reflect.Type) string {
	switch kind.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a JSON array"
	case reflect.Map, reflect.Struct:
		return "a JSON object"
	case reflect.Interface:
		return "a JSON value"
	default:
		return kind.String()
	}
}

// manifestValidator carries the identity needed to build diagnostics.
type manifestValidator struct {
	path    string
	plugin  string
	rootDir string
}

func (v *manifestValidator) fail(field, message string) error {
	return &ManifestError{Plugin: v.plugin, Path: v.path, Field: field, Message: message}
}

func (v *manifestValidator) validate(manifest *Manifest, raw *manifestFile) error {
	if err := v.apiVersion(raw); err != nil {
		return err
	}
	if err := v.identity(manifest, raw); err != nil {
		return err
	}
	if err := v.entry(manifest); err != nil {
		return err
	}
	if err := v.tools(manifest, raw.Tools); err != nil {
		return err
	}
	if err := v.hooks(manifest, raw.Hooks); err != nil {
		return err
	}
	if err := v.commands(manifest, raw.Commands); err != nil {
		return err
	}
	if err := v.permissions(raw.Permissions, "permissions", &manifest.Permissions); err != nil {
		return err
	}
	return v.concurrency(manifest)
}

func (v *manifestValidator) apiVersion(raw *manifestFile) error {
	if strings.TrimSpace(raw.APIVersion) == "" {
		return v.fail("apiVersion", fmt.Sprintf("is required and must be %q", APIVersion))
	}
	if raw.APIVersion != APIVersion {
		return v.fail("apiVersion", fmt.Sprintf("must be %q, got %q", APIVersion, raw.APIVersion))
	}
	return nil
}

func (v *manifestValidator) identity(manifest *Manifest, raw *manifestFile) error {
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return v.fail("name", "is required")
	}
	if len(name) > maxPluginNameLength {
		return v.fail("name", fmt.Sprintf("must be at most %d characters, got %d", maxPluginNameLength, len(name)))
	}
	if !pluginNamePattern.MatchString(name) {
		return v.fail("name", `must match ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	}
	manifest.Name = name
	// From here on the plugin is identified, so later diagnostics carry a name
	// the author recognises.
	v.plugin = name

	version := strings.TrimSpace(raw.Version)
	if version == "" {
		return v.fail("version", "is required")
	}
	if !pluginVersionPattern.MatchString(version) {
		return v.fail("version", `must match ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	}
	manifest.Version = version

	if strings.TrimSpace(raw.Description) == "" {
		return v.fail("description", "must not be empty")
	}
	manifest.Description = raw.Description
	return nil
}

// entry validates the JavaScript entry point.
//
// Containment is checked textually here; a symlink that leaves the plugin
// directory is rejected later by the import resolver, which resolves links
// before deciding which root a module belongs to.
func (v *manifestValidator) entry(manifest *Manifest) error {
	entry := strings.TrimSpace(manifest.Entry)
	if entry == "" {
		return v.fail("entry", "is required")
	}
	if isAbsoluteEntry(entry) {
		return v.fail("entry", fmt.Sprintf("must be a relative path, got %q", entry))
	}
	if entryEscapesRoot(entry) {
		return v.fail("entry", fmt.Sprintf(`must not contain ".." path segments, got %q`, entry))
	}
	target := filepath.Join(v.rootDir, filepath.FromSlash(entry))
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v.fail("entry", fmt.Sprintf("%q does not exist (resolved to %s)", entry, target))
		}
		return v.fail("entry", fmt.Sprintf("%q could not be inspected: %s", entry, err))
	}
	if !info.Mode().IsRegular() {
		return v.fail("entry", fmt.Sprintf("%q must be a regular file, got %s", entry, info.Mode().Type()))
	}
	manifest.Entry = entry
	return nil
}

func (v *manifestValidator) tools(manifest *Manifest, declarations []toolDeclarationFile) error {
	if len(declarations) == 0 {
		manifest.Tools = nil
		return nil
	}
	seen := map[string]int{}
	tools := make([]ToolDeclaration, 0, len(declarations))
	for index, declaration := range declarations {
		field := fmt.Sprintf("tools[%d]", index)
		name := strings.TrimSpace(declaration.Name)
		if name == "" {
			return v.fail(field+".name", "is required")
		}
		if len(name) > maxToolNameLength {
			return v.fail(field+".name", fmt.Sprintf("must be at most %d characters, got %d", maxToolNameLength, len(name)))
		}
		if !toolNamePattern.MatchString(name) {
			return v.fail(field+".name", `must match ^[a-z0-9_]+$`)
		}
		if IsBuiltinToolName(name) {
			return v.fail(field+".name", fmt.Sprintf("conflicts with the built-in tool %q", name))
		}
		if previous, ok := seen[name]; ok {
			return v.fail(field+".name", fmt.Sprintf("duplicate tool name %q, already declared by tools[%d]", name, previous))
		}
		seen[name] = index

		handler := strings.TrimSpace(declaration.Handler)
		if handler == "" {
			return v.fail(field+".handler", "is required")
		}
		schema, err := v.toolSchema(declaration.Schema, field+".schema")
		if err != nil {
			return err
		}
		var permissions []Permission
		if err := v.permissions(declaration.Permissions, field+".permissions", &permissions); err != nil {
			return err
		}
		if err := v.timeout(declaration.TimeoutMS, field+".timeoutMs"); err != nil {
			return err
		}
		risk := strings.TrimSpace(declaration.Risk)
		if risk != "" && !containsString(validToolRisks, risk) {
			return v.fail(field+".risk", fmt.Sprintf("must be one of %s, got %q", strings.Join(validToolRisks, ", "), risk))
		}
		tools = append(tools, ToolDeclaration{
			Name:          name,
			Description:   declaration.Description,
			Handler:       handler,
			Schema:        schema,
			Permissions:   permissions,
			ParallelSafe:  declaration.ParallelSafe,
			TimeoutMS:     timeoutValue(declaration.TimeoutMS),
			Risk:          risk,
			PromptSnippet: declaration.PromptSnippet,
		})
	}
	manifest.Tools = tools
	return nil
}

// toolSchema validates a tool's argument schema and fills the default when the
// declaration omits one.
func (v *manifestValidator) toolSchema(raw json.RawMessage, field string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return append(json.RawMessage(nil), defaultToolSchema...), nil
	}
	var parsed any
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return nil, v.fail(field, "is not valid JSON: "+err.Error())
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, v.fail(field, fmt.Sprintf("must be a JSON Schema object, got %s", jsonKind(parsed)))
	}
	typeValue, ok := object["type"]
	if !ok {
		return nil, v.fail(field, `must declare "type": "object"`)
	}
	if typeName, ok := typeValue.(string); !ok || typeName != "object" {
		return nil, v.fail(field, fmt.Sprintf(`must declare "type": "object", got %v`, typeValue))
	}
	if properties, ok := object["properties"]; ok {
		if _, isObject := properties.(map[string]any); !isObject {
			return nil, v.fail(field, fmt.Sprintf(`"properties" must be a JSON object, got %s`, jsonKind(properties)))
		}
	}
	return append(json.RawMessage(nil), trimmed...), nil
}

func (v *manifestValidator) hooks(manifest *Manifest, declarations []hookDeclarationFile) error {
	if len(declarations) == 0 {
		manifest.Hooks = nil
		return nil
	}
	seen := map[string]int{}
	hooks := make([]HookDeclaration, 0, len(declarations))
	for index, declaration := range declarations {
		field := fmt.Sprintf("hooks[%d]", index)
		event := strings.TrimSpace(declaration.Event)
		if event == "" {
			return v.fail(field+".event", "is required")
		}
		if !IsHookEvent(event) {
			return v.fail(field+".event", fmt.Sprintf(
				"unknown hook event %q; observer events are %s; interceptor events are %s",
				event, strings.Join(observerHookEvents, ", "), strings.Join(interceptorHookEvents, ", "),
			))
		}
		handler := strings.TrimSpace(declaration.Handler)
		if handler == "" {
			return v.fail(field+".handler", "is required")
		}
		if err := v.timeout(declaration.TimeoutMS, field+".timeoutMs"); err != nil {
			return err
		}
		key := event + "\x00" + handler
		if previous, ok := seen[key]; ok {
			return v.fail(field, fmt.Sprintf("duplicate hook %q with handler %q, already declared by hooks[%d]", event, handler, previous))
		}
		seen[key] = index
		hooks = append(hooks, HookDeclaration{
			Event:     event,
			Handler:   handler,
			TimeoutMS: timeoutValue(declaration.TimeoutMS),
		})
	}
	manifest.Hooks = hooks
	return nil
}

func (v *manifestValidator) commands(manifest *Manifest, declarations []commandDeclarationFile) error {
	if len(declarations) == 0 {
		manifest.Commands = nil
		return nil
	}
	seen := map[string]int{}
	commands := make([]CommandDeclaration, 0, len(declarations))
	for index, declaration := range declarations {
		field := fmt.Sprintf("commands[%d]", index)
		name := strings.TrimSpace(declaration.Name)
		if name == "" {
			return v.fail(field+".name", "is required")
		}
		if len(name) > maxCommandNameLength {
			return v.fail(field+".name", fmt.Sprintf("must be at most %d characters, got %d", maxCommandNameLength, len(name)))
		}
		if !commandNamePattern.MatchString(name) {
			return v.fail(field+".name", `must match ^[a-z0-9][a-z0-9-]*$`)
		}
		if IsBuiltinCommandName(name) {
			return v.fail(field+".name", fmt.Sprintf("conflicts with the built-in command %q", name))
		}
		if previous, ok := seen[name]; ok {
			return v.fail(field+".name", fmt.Sprintf("duplicate command name %q, already declared by commands[%d]", name, previous))
		}
		seen[name] = index

		handler := strings.TrimSpace(declaration.Handler)
		if handler == "" {
			return v.fail(field+".handler", "is required")
		}
		var permissions []Permission
		if err := v.permissions(declaration.Permissions, field+".permissions", &permissions); err != nil {
			return err
		}
		if err := v.timeout(declaration.TimeoutMS, field+".timeoutMs"); err != nil {
			return err
		}
		commands = append(commands, CommandDeclaration{
			Name:        name,
			Description: declaration.Description,
			Handler:     handler,
			Usage:       declaration.Usage,
			Permissions: permissions,
			TimeoutMS:   timeoutValue(declaration.TimeoutMS),
		})
	}
	manifest.Commands = commands
	return nil
}

// permissions validates one permission list and stores the canonical values.
func (v *manifestValidator) permissions(raw []string, field string, out *[]Permission) error {
	if len(raw) == 0 {
		*out = nil
		return nil
	}
	seen := map[Permission]bool{}
	parsed := make([]Permission, 0, len(raw))
	for index, item := range raw {
		permission, err := ParsePermission(item)
		if err != nil {
			return v.fail(fmt.Sprintf("%s[%d]", field, index), fmt.Sprintf("unknown permission %q; must be one of %s", item, permissionList()))
		}
		if seen[permission] {
			return v.fail(fmt.Sprintf("%s[%d]", field, index), fmt.Sprintf("duplicate permission %q", permission))
		}
		seen[permission] = true
		parsed = append(parsed, permission)
	}
	*out = parsed
	return nil
}

func (v *manifestValidator) timeout(value *int, field string) error {
	if value != nil && *value < 0 {
		return v.fail(field, fmt.Sprintf("must not be negative, got %d", *value))
	}
	return nil
}

func (v *manifestValidator) concurrency(manifest *Manifest) error {
	maxRuntimes := manifest.Concurrency.MaxRuntimes
	if maxRuntimes < 0 || maxRuntimes > maxRuntimesLimit {
		return v.fail("concurrency.maxRuntimes", fmt.Sprintf("must be between 0 and %d, got %d", maxRuntimesLimit, maxRuntimes))
	}
	return nil
}

func timeoutValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func permissionList() string {
	names := make([]string, 0, len(Permissions()))
	for _, permission := range Permissions() {
		names = append(names, string(permission))
	}
	return strings.Join(names, ", ")
}

func isAbsoluteEntry(entry string) bool {
	return filepath.IsAbs(entry) ||
		strings.HasPrefix(entry, "/") ||
		strings.HasPrefix(entry, `\`) ||
		windowsDrivePattern.MatchString(entry)
}

func entryEscapesRoot(entry string) bool {
	for _, segment := range strings.FieldsFunc(entry, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return true
		}
	}
	return false
}

func jsonKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64, json.Number:
		return "a number"
	default:
		return fmt.Sprintf("%T", value)
	}
}
