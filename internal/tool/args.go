package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Argument objects use the standard JSON data model.
//
// Historically Bruce tools took a flat map[string]string and ParseArguments
// stringified every non-string value. That silently destroyed structure and
// type: {"options":{"depth":3}} arrived as the *string* `{"depth":3}`. The
// plugin system must hand nested objects, arrays, booleans, numbers and null
// to JavaScript without loss, so the canonical representation is now
// map[string]any holding exactly the values encoding/json produces.
//
// Existing callers that only ever pass strings keep working: a string stays a
// string, and the typed accessors below read both a real string and a JSON
// scalar that arrived as one.
type Args = map[string]any

// ParseArguments decodes a JSON object into the canonical argument model.
// Numbers decode as json.Number so that an integer stays an integer instead
// of becoming a float64 with a fractional part.
func ParseArguments(raw string) (Args, error) {
	if strings.TrimSpace(raw) == "" {
		return Args{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var generic map[string]any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	if generic == nil {
		return Args{}, nil
	}
	return generic, nil
}

// EncodeArguments renders an argument object as compact JSON. It is the
// inverse of ParseArguments and never flattens or stringifies structure.
func EncodeArguments(args Args) (string, error) {
	if len(args) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// StringArg reads a string argument. A JSON scalar that arrived as a string
// (for example "3" for an integer field) is returned as is so that callers
// can parse it themselves; a missing key is the empty string.
func StringArg(args Args, name string) string {
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

// OptionalIntArg reads an optional integer argument. A missing, null or empty
// value yields nil. A JSON number is accepted directly; a string is accepted
// only when it holds a plain integer, which keeps the previous behavior for
// callers that passed "2" as a string.
func OptionalIntArg(args Args, name string) (*int, error) {
	value, ok := args[name]
	if !ok || value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case json.Number:
		n, err := strconv.Atoi(typed.String())
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer: %s", name, typed.String())
		}
		return &n, nil
	case float64:
		if typed != float64(int(typed)) {
			return nil, fmt.Errorf("%s must be an integer: %v", name, typed)
		}
		n := int(typed)
		return &n, nil
	case int:
		return &typed, nil
	case int64:
		n := int(typed)
		return &n, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(typed), `"`))
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer: %s", name, typed)
		}
		return &n, nil
	default:
		return nil, fmt.Errorf("%s must be an integer", name)
	}
}

// ObjectArg reads a nested JSON object argument. A missing or null value
// yields nil.
func ObjectArg(args Args, name string) (Args, error) {
	value, ok := args[name]
	if !ok || value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	return object, nil
}

// ArrayArg reads a nested JSON array argument. A missing or null value yields
// nil.
func ArrayArg(args Args, name string) ([]any, error) {
	value, ok := args[name]
	if !ok || value == nil {
		return nil, nil
	}
	array, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON array", name)
	}
	return array, nil
}

// BoolArg reads an optional boolean argument. A missing value yields nil.
func BoolArg(args Args, name string) (*bool, error) {
	value, ok := args[name]
	if !ok || value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case bool:
		return &typed, nil
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		if err != nil {
			return nil, fmt.Errorf("%s must be a boolean: %s", name, typed)
		}
		return &parsed, nil
	default:
		return nil, fmt.Errorf("%s must be a boolean", name)
	}
}

// CloneArgs returns a deep copy of an argument object, so a hook or a tool
// cannot mutate the caller's data through a shared nested map or slice.
func CloneArgs(args Args) Args {
	if args == nil {
		return nil
	}
	return cloneValue(args).(map[string]any)
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}

// ValidateArgumentsShape reports whether an argument object can be
// represented as JSON at all. It guards the plugin boundary: a Go value that
// json.Marshal cannot render (a channel, a function) must be rejected before
// it reaches a JavaScript runtime.
func ValidateArgumentsShape(args Args) error {
	if args == nil {
		return nil
	}
	_, err := json.Marshal(args)
	if err != nil {
		return errors.New("tool arguments are not JSON-serializable: " + err.Error())
	}
	return nil
}
