package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// The structured-outputs API answers 400 to an enum under a type list
// ("Enum value 'high' does not match declared type '['string', 'null']'")
// and accepts the enum alone: the schema is sent as the enum alone, with the
// same accepted values.
func TestAnthropicSendsAnEnumUnderATypeListAsTheEnumAlone(t *testing.T) {
	for _, c := range []struct {
		name     string
		property map[string]any
		want     map[string]any
	}{
		{"nullable string enum", map[string]any{"type": []string{"string", "null"}, "enum": []any{"high", "low", nil}}, map[string]any{"enum": []any{"high", "low", nil}}},
		{"no null in the enum", map[string]any{"type": []any{"string", "null"}, "enum": []any{"high"}}, map[string]any{"enum": []any{"high"}}},
		{"values the types refuse are dropped", map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", 1, true, nil}}, map[string]any{"enum": []any{"a", nil}}},
		{"integer keeps whole numbers", map[string]any{"type": []any{"integer", "null"}, "enum": []any{1, 1.5, nil}}, map[string]any{"enum": []any{float64(1), nil}}},
		{"number and boolean", map[string]any{"type": []any{"number", "boolean"}, "enum": []any{1.5, false, "x"}}, map[string]any{"enum": []any{1.5, false}}},
		{"a single type is kept", map[string]any{"type": "string", "enum": []any{"high"}}, map[string]any{"type": "string", "enum": []any{"high"}}},
		{"a type list without an enum is kept", map[string]any{"type": []any{"string", "null"}}, map[string]any{"type": []any{"string", "null"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			schema := object(map[string]any{"band": c.property}, "band")
			before, _ := json.Marshal(schema)
			sent := anthropicSchema(schema)
			band := sent["properties"].(map[string]any)["band"]
			if !reflect.DeepEqual(roundTrip(t, band), c.want) {
				t.Fatalf("band = %#v, want %#v", band, c.want)
			}
			if fits, why := anthropicSchemaFit(sent); !fits {
				t.Fatalf("does not fit: %s", why)
			}
			if after, _ := json.Marshal(schema); string(after) != string(before) {
				t.Fatalf("the caller's schema was modified: %#v", schema)
			}
		})
	}
}

// A nested nullable enum (the ops investment-mix explanation's
// evidence_quality_band) is sent in structured mode, rewritten, in one call.
func TestAnthropicStructuredRequestCarriesTheRewrittenEnum(t *testing.T) {
	provider, capture := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"evidence":{"band":null}}`, "end_turn")})
	schema := object(map[string]any{
		"evidence": object(map[string]any{
			"band": map[string]any{"type": []string{"string", "null"}, "enum": []any{"high", "moderate", nil}},
		}, "band"),
	}, "evidence")
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: &JSONFormat{Name: "mix", Schema: schema}})
	if err != nil || result.FormatPath != FormatPathSchema || capture.calls() != 1 {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, capture.calls())
	}
	config, _ := capture.request(0)["output_config"].(map[string]any)
	format, _ := config["format"].(map[string]any)
	sent, _ := format["schema"].(map[string]any)
	band := sent["properties"].(map[string]any)["evidence"].(map[string]any)["properties"].(map[string]any)["band"]
	if want := map[string]any{"enum": []any{"high", "moderate", nil}}; !reflect.DeepEqual(band, want) {
		t.Fatalf("sent band = %#v, want %#v", band, want)
	}
}
