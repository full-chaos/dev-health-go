package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// Structured outputs refuse these bounds, so the model can only learn them
// from the description; a bound it never sees is an answer the caller's own
// check rejects after the call was billed.
func TestAnthropicSchemaWritesEachDroppedBoundIntoTheDescription(t *testing.T) {
	cases := []struct {
		keyword string
		node    map[string]any
		want    string
	}{
		{"minLength", map[string]any{"type": "string", "minLength": 3}, "at least 3 characters"},
		{"maxLength", map[string]any{"type": "string", "maxLength": 120}, "at most 120 characters"},
		{"minimum", map[string]any{"type": "number", "minimum": 0}, "at least 0"},
		{"maximum", map[string]any{"type": "number", "maximum": 0.5}, "at most 0.5"},
		{"exclusiveMinimum", map[string]any{"type": "number", "exclusiveMinimum": -1}, "greater than -1"},
		{"exclusiveMaximum", map[string]any{"type": "number", "exclusiveMaximum": 100}, "less than 100"},
		{"multipleOf", map[string]any{"type": "integer", "multipleOf": 5}, "a multiple of 5"},
		{"minItems", map[string]any{"type": "array", "minItems": 2, "items": map[string]any{"type": "string"}}, "at least 2 items"},
		{"maxItems", map[string]any{"type": "array", "maxItems": 10, "items": map[string]any{"type": "string"}}, "at most 10 items"},
		{"uniqueItems", map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}}, "unique items"},
		{"minContains", map[string]any{"type": "array", "minContains": 2, "items": map[string]any{"type": "string"}}, "at least 2 matching items"},
		{"maxContains", map[string]any{"type": "array", "maxContains": 4, "items": map[string]any{"type": "string"}}, "at most 4 matching items"},
		{"minProperties", map[string]any{"type": "object", "additionalProperties": false, "minProperties": 1}, "at least 1 properties"},
		{"maxProperties", map[string]any{"type": "object", "additionalProperties": false, "maxProperties": 3}, "at most 3 properties"},
	}
	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := anthropicSchema(map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"field"},
				"properties": map[string]any{"field": tc.node},
			})
			field := got["properties"].(map[string]any)["field"].(map[string]any)
			if _, sent := field[tc.keyword]; sent {
				t.Fatalf("%s was sent: %v", tc.keyword, field)
			}
			description, _ := field["description"].(string)
			if !strings.Contains(description, tc.want) {
				t.Fatalf("description = %q, want it to name %q", description, tc.want)
			}
			if fits, why := anthropicSchemaFit(got); !fits {
				t.Fatalf("the sent schema does not fit structured outputs: %s", why)
			}
		})
	}
}

func TestAnthropicSchemaKeepsTheCallersDescriptionAndAddsTheBounds(t *testing.T) {
	got := anthropicSchema(map[string]any{
		"type": "string", "description": "A short driver.", "minLength": 1, "maxLength": 120,
	})
	description, _ := got["description"].(string)
	if !strings.HasPrefix(description, "A short driver.") {
		t.Fatalf("description = %q, want the caller's text first", description)
	}
	for _, want := range []string{"at least 1 characters", "at most 120 characters"} {
		if !strings.Contains(description, want) {
			t.Fatalf("description = %q, want %q", description, want)
		}
	}
	if again := anthropicSchema(map[string]any{
		"type": "string", "description": "A short driver.", "minLength": 1, "maxLength": 120,
	}); again["description"] != description {
		t.Fatalf("description is not stable: %q then %q", description, again["description"])
	}
}

func TestAnthropicSchemaLeavesANodeWithNoDroppedBoundAsItIs(t *testing.T) {
	got := anthropicSchema(map[string]any{
		"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
	})
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "description") {
		t.Fatalf("a description was added with no bound dropped: %s", encoded)
	}
}

// The recorded CHAOS-10007 shape: a confidence driver list whose items carry
// maxLength 120 reaches claude-haiku-5-5 with the limit in the item's
// description.
func TestAnthropicSchemaCarriesTheDriverLengthToTheModel(t *testing.T) {
	got := anthropicSchema(map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"drivers"},
		"properties": map[string]any{
			"drivers": map[string]any{
				"type": "array", "maxItems": 10,
				"items": map[string]any{"type": "string", "minLength": 1, "maxLength": 120},
			},
		},
	})
	drivers := got["properties"].(map[string]any)["drivers"].(map[string]any)
	if description, _ := drivers["description"].(string); !strings.Contains(description, "at most 10 items") {
		t.Fatalf("drivers description = %q", description)
	}
	item := drivers["items"].(map[string]any)
	if description, _ := item["description"].(string); !strings.Contains(description, "at most 120 characters") {
		t.Fatalf("driver item description = %q", description)
	}
}

func TestAnthropicToolCallSchemaGetsNoBoundDescription(t *testing.T) {
	schema := map[string]any{"type": "string", "maxLength": 120}
	got := copySchema(schema)
	if got["maxLength"] != float64(120) {
		t.Fatalf("the tool schema lost maxLength: %v", got)
	}
	if _, added := got["description"]; added {
		t.Fatalf("the tool schema got a description: %v", got)
	}
}
