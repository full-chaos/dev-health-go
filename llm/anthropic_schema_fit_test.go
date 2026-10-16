package llm

import (
	"fmt"
	"strings"
	"testing"
)

func object(properties map[string]any, required ...string) map[string]any {
	names := []any{}
	for _, name := range required {
		names = append(names, name)
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": names}
}

func manyOptional(n int, union bool) map[string]any {
	properties := map[string]any{}
	for i := 0; i < n; i++ {
		var property map[string]any = map[string]any{"type": "string"}
		if union {
			property = map[string]any{"type": []any{"string", "null"}}
		}
		properties[fmt.Sprintf("p%02d", i)] = property
	}
	return object(properties)
}

func TestAnthropicSchemaFitChecksEachDocumentedLimit(t *testing.T) {
	str := map[string]any{"type": "string"}
	cases := []struct {
		name   string
		schema map[string]any
		reason string
	}{
		{"a plain object fits", object(map[string]any{"a": str}, "a"), ""},
		{"24 optional fit", manyOptional(24, false), ""},
		{"25 optional do not", manyOptional(25, false), "optional_parameters"},
		{"16 unions fit when required", func() map[string]any {
			s := manyOptional(16, true)
			names := []any{}
			for name := range s["properties"].(map[string]any) {
				names = append(names, name)
			}
			s["required"] = names
			return s
		}(), ""},
		{"17 unions do not", manyOptional(17, true), "union_parameters"},
		{"a map field", object(map[string]any{"m": map[string]any{"type": "object", "additionalProperties": str}}), "additional_properties_not_false"},
		{"an object with no additionalProperties", map[string]any{"type": "object", "properties": map[string]any{"a": str}}, "additional_properties_not_false"},
		{"an untyped property", object(map[string]any{"x": map[string]any{"description": "anything"}}), "property_without_type"},
		{"if/then", object(map[string]any{"a": map[string]any{"type": "string", "if": map[string]any{}, "then": map[string]any{}}}), "unsupported_keyword"},
		{"a nested not", object(map[string]any{"a": map[string]any{"type": "object", "additionalProperties": false, "allOf": []any{map[string]any{"not": map[string]any{}}}}}), "unsupported_keyword not"},
		{"an external ref", object(map[string]any{"a": map[string]any{"$ref": "http://example.invalid/s.json"}}), "external_ref"},
		{"a complex enum", object(map[string]any{"a": map[string]any{"enum": []any{map[string]any{"k": 1}}}}), "complex_enum"},
		{"an unknown string format", object(map[string]any{"a": map[string]any{"type": "string", "format": "binary"}}), "string_format"},
		{"a lookahead pattern", object(map[string]any{"a": map[string]any{"type": "string", "pattern": "^(?=x)"}}), "pattern"},
		{"items are checked", object(map[string]any{"a": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"b": str}}}}), "additional_properties_not_false"},
		{"a supported format and pattern", object(map[string]any{"a": map[string]any{"type": "string", "format": "date-time", "pattern": "^[a-z]+$"}}, "a"), ""},
	}
	for _, tc := range cases {
		fits, reason := anthropicSchemaFit(tc.schema)
		if tc.reason == "" && (!fits || reason != "") {
			t.Errorf("%s: fits=%v reason=%q, want a fit", tc.name, fits, reason)
		}
		if tc.reason != "" && (fits || !strings.HasPrefix(reason, tc.reason)) {
			t.Errorf("%s: fits=%v reason=%q, want %q", tc.name, fits, reason, tc.reason)
		}
	}
}

func TestAnthropicForcesToolsExceptOnTheDocumentedModels(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-haiku-5-5": true, "claude-opus-5": true, "claude-sonnet-5": true,
		"claude-opus-5-5": false, "claude-sonnet-5-5": false, "claude-fable-5-1": false, "claude-mythos-5-1": false,
	} {
		if got := anthropicForcesTools(model); got != want {
			t.Errorf("anthropicForcesTools(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestAnthropicSchemaFitReportsTheSameFirstLimitEveryTime(t *testing.T) {
	str := map[string]any{"type": "string"}
	schema := object(map[string]any{
		"z_map": map[string]any{"type": "object", "additionalProperties": str},
		"a_untyped": map[string]any{"description": "x"},
		"m_if": map[string]any{"type": "string", "if": map[string]any{}},
	})
	_, first := anthropicSchemaFit(schema)
	for i := 0; i < 50; i++ {
		if _, again := anthropicSchemaFit(schema); again != first {
			t.Fatalf("run %d reported %q, the first run %q", i, again, first)
		}
	}
	if first != "property_without_type: $.a_untyped" {
		t.Fatalf("first = %q, want the alphabetically first property's limit", first)
	}
}
