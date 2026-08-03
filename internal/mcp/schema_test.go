package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeSchemaEnsuresObject(t *testing.T) {
	// Non-object schema should be wrapped
	raw := json.RawMessage(`{"type":"string"}`)
	out := SanitizeSchema(raw)
	var schema map[string]any
	if err := json.Unmarshal(out, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" {
		t.Fatalf("expected type=object, got %v", schema["type"])
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil || properties["value"] == nil {
		t.Fatalf("expected properties.value, got %v", schema["properties"])
	}
}

func TestSanitizeSchemaRemovesExtensionKeys(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","$schema":"http://x","$ref":"#/defs/Foo","properties":{"x":{"type":"string"}}}`)
	out := SanitizeSchema(raw)
	var schema map[string]any
	if err := json.Unmarshal(out, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["$schema"]; ok {
		t.Fatal("expected $schema to be removed")
	}
	if _, ok := schema["$ref"]; ok {
		t.Fatal("expected $ref to be removed")
	}
}

func TestSanitizeSchemaFoldsUnion(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","anyOf":[{"type":"string","description":"a string"},{"type":"number"}],"description":"check"}`)
	out := SanitizeSchema(raw)
	var schema map[string]any
	if err := json.Unmarshal(out, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["anyOf"]; ok {
		t.Fatal("expected anyOf to be removed")
	}
	description, _ := schema["description"].(string)
	if !strings.Contains(description, "anyOf options") {
		t.Fatalf("expected description to contain union options, got %q", description)
	}
}

func TestSanitizeSchemaEnsuresRequired(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	out := SanitizeSchema(raw)
	var schema map[string]any
	if err := json.Unmarshal(out, &schema); err != nil {
		t.Fatal(err)
	}
	required, _ := schema["required"].([]any)
	if required == nil {
		t.Fatal("expected required to be an array")
	}
}
