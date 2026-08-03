package mcp

import (
	"encoding/json"
	"strings"
)

const schemaDescriptionLimit = 1000

// SanitizeSchema cleans a JSON Schema for safe tool registration.
func SanitizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
	}
	if schema == nil {
		schema = map[string]any{}
	}
	ensureObjectSchema(schema)
	sanitizeObject(schema)
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	if _, ok := schema["required"]; !ok {
		schema["required"] = []any{}
	}
	out, _ := json.Marshal(schema)
	return out
}

func ensureObjectSchema(schema map[string]any) {
	typeName, _ := schema["type"].(string)
	if typeName == "" {
		schema["type"] = "object"
		return
	}
	if typeName != "object" {
		// Deep-copy the original content to avoid reference cycles.
		inner := deepCopyMap(schema)
		for key := range schema {
			delete(schema, key)
		}
		schema["type"] = "object"
		schema["properties"] = map[string]any{"value": inner}
	}
}

func deepCopyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		switch typed := value.(type) {
		case map[string]any:
			dst[key] = deepCopyMap(typed)
		case []any:
			items := make([]any, len(typed))
			for i, item := range typed {
				if object, ok := item.(map[string]any); ok {
					items[i] = deepCopyMap(object)
				} else {
					items[i] = item
				}
			}
			dst[key] = items
		default:
			dst[key] = typed
		}
	}
	return dst
}

func sanitizeObject(schema map[string]any) {
	delete(schema, "$schema")
	delete(schema, "$id")
	delete(schema, "$defs")
	delete(schema, "definitions")
	delete(schema, "$ref")
	foldUnion(schema, "anyOf")
	foldUnion(schema, "oneOf")
	truncateSchemaDescription(schema)

	for _, value := range schema {
		switch typed := value.(type) {
		case map[string]any:
			sanitizeObject(typed)
		case []any:
			for _, item := range typed {
				if object, ok := item.(map[string]any); ok {
					sanitizeObject(object)
				}
			}
		}
	}
}

func foldUnion(schema map[string]any, field string) {
	union, ok := schema[field].([]any)
	if !ok || len(union) == 0 {
		return
	}
	var description strings.Builder
	if existing, _ := schema["description"].(string); existing != "" {
		description.WriteString(existing)
		description.WriteByte('\n')
	}
	description.WriteString(field + " options: ")
	for _, option := range union {
		object, _ := option.(map[string]any)
		if typeName, _ := object["type"].(string); typeName != "" {
			description.WriteString(typeName)
		}
		if optionDescription, _ := object["description"].(string); optionDescription != "" {
			description.WriteString("(" + optionDescription + ")")
		}
		description.WriteString("; ")
	}
	schema["description"] = description.String()
	delete(schema, field)
}

func truncateSchemaDescription(schema map[string]any) {
	description, ok := schema["description"].(string)
	if !ok || len(description) <= schemaDescriptionLimit {
		return
	}
	schema["description"] = description[:schemaDescriptionLimit] + "..."
}
