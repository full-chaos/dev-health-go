package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func roundTrip(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const schemaRefused = `{"type":"error","error":{"type":"invalid_request_error","message":"output_config.format.schema: For 'object' type, 'additionalProperties: object' is not supported."}}`

func anthropicToolUse(name, input, stopReason string) map[string]any {
	return map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "Here is the answer."},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": name, "input": rawJSON(input)},
		},
		"stop_reason": stopReason,
		"usage":       map[string]any{"input_tokens": 20, "output_tokens": 9},
	}
}

type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// mapFormat has a map field: structured outputs refuse an
// additionalProperties schema, so it never fits.
var mapFormat = &JSONFormat{Name: "categorization", Schema: map[string]any{
	"type": "object", "additionalProperties": false, "required": []any{"a"},
	"properties": map[string]any{
		"a":      map[string]any{"type": "number"},
		"labels": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	},
}}

func TestAnthropicSendsAToolCallWhenTheSchemaDoesNotFit(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicToolUse("categorization", `{"a": 1}`, "tool_use")})
	result, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "p", Format: mapFormat})
	if err != nil {
		t.Fatal(err)
	}
	if c.calls() != 1 || result.Text != `{"a":1}` || result.FormatPath != FormatPathTool || !strings.HasPrefix(result.FormatReason, "additional_properties_not_false") {
		t.Fatalf("calls = %d, result = %+v", c.calls(), result)
	}
	sent := c.request(0)
	if _, present := sent["output_config"]; present || sent["system"] != "sys" {
		t.Fatalf("the tool call carries output_config or lost the system message: %v", sent)
	}
	tool := sent["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "categorization" || !reflect.DeepEqual(tool["input_schema"], roundTrip(t, mapFormat.Schema)) {
		t.Fatalf("tool = %v, want the Format's schema as given", tool)
	}
	if description, _ := tool["description"].(string); !strings.Contains(description, "never move a top-level property inside an object property") {
		t.Fatalf("description = %q, want the placement rule", description)
	}
	if !reflect.DeepEqual(sent["tool_choice"], map[string]any{"type": "tool", "name": "categorization"}) {
		t.Fatalf("tool_choice = %v", sent["tool_choice"])
	}
}

func TestAnthropicUsesStructuredOutputsWhenTheSchemaFits(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"a": 1}`, "end_turn")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || c.calls() != 1 || result.FormatPath != FormatPathSchema || result.FormatReason != "" {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
	if _, present := c.request(0)["output_config"]; !present {
		t.Fatal("a fitting schema was not sent as output_config")
	}
	if _, present := c.request(0)["tools"]; present {
		t.Fatal("a fitting schema also sent a tool")
	}
}

func TestAnthropicARefusedSchemaIsOneCallAndAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format *JSONFormat
	}{{"structured outputs", testFormat}, {"tool call", mapFormat}} {
		t.Run(tc.name, func(t *testing.T) {
			provider, c := newTestAnthropic(t, step{status: 400, body: schemaRefused}, step{status: 200, body: anthropicText(`{"a": 1}`, "end_turn")})
			_, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: tc.format})
			if class, ok := ClassOf(err); !ok || class != ClassInvalidRequest || c.calls() != 1 {
				t.Fatalf("err = %v calls = %d, want one call and class %q", err, c.calls(), ClassInvalidRequest)
			}
		})
	}
}

func TestAnthropicModelsWithoutForcedToolsGetToolChoiceAuto(t *testing.T) {
	server, c := scriptedServer(t, step{status: 200, body: anthropicToolUse("categorization", `{"a": 1}`, "tool_use")})
	provider, err := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Model: "claude-sonnet-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: mapFormat}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.request(0)["tool_choice"], map[string]any{"type": "auto"}) {
		t.Fatalf("tool_choice = %v, want auto for a model the docs list as refusing forced tools", c.request(0)["tool_choice"])
	}
}

func TestAnthropicASchemaFreeRequestNeverSendsATool(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`},
	)
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if class, ok := ClassOf(err); !ok || class != ClassInvalidRequest || c.calls() != 1 {
		t.Fatalf("err = %v calls = %d, want one final invalid_request", err, c.calls())
	}
}

func TestAnthropicToolNameFallsBackToOutput(t *testing.T) {
	for name, want := range map[string]string{"categorization": "categorization", "": "output", "has space": "output", "a.b": "output"} {
		if got := anthropicToolName(&JSONFormat{Name: name}); got != want {
			t.Errorf("anthropicToolName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAnthropicToolCallKeepsTheKeywordsStructuredOutputsDrop(t *testing.T) {
	format := &JSONFormat{Name: "bounded", Schema: roundTrip(t, boundedFormat().Schema).(map[string]any)}
	format.Schema["properties"].(map[string]any)["labels"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicToolUse("bounded", `{"maximum": 1}`, "tool_use")})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: format}); err != nil {
		t.Fatal(err)
	}
	tool := c.request(0)["tools"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(tool["input_schema"], roundTrip(t, format.Schema)) {
		t.Fatalf("input_schema = %v, want the Format's schema with its bounds", tool["input_schema"])
	}
}

func TestAnthropicTransientRetryIsStillOne(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 503, body: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := provider.Complete(ctx, Request{Prompt: "p", Format: mapFormat})
	if class, ok := ClassOf(err); !ok || class != ClassServer || c.calls() != 2 {
		t.Fatalf("err = %v calls = %d, want a server error after 2 calls", err, c.calls())
	}
	if _, present := c.request(1)["tools"]; !present {
		t.Fatal("the transient retry changed the mode")
	}
}

func TestFormatPathNamesTheSchemaPathForEveryKind(t *testing.T) {
	for _, kind := range Kinds() {
		wire := providerWires[kind]
		server, _ := scriptedServer(t, step{status: 200, body: wire.answer(`{"a": 1}`, 1, 1)})
		provider, err := New(Config{Kind: kind, APIKey: NewSecret(testKey), BaseURL: server.URL, Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		withFormat, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
		if err != nil || withFormat.FormatPath != FormatPathSchema {
			t.Errorf("%s: FormatPath = %q err = %v, want %q", kind, withFormat.FormatPath, err, FormatPathSchema)
		}
		free, err := provider.Complete(context.Background(), Request{Prompt: "p"})
		if err != nil || free.FormatPath != "" {
			t.Errorf("%s: schema-free FormatPath = %q err = %v, want empty", kind, free.FormatPath, err)
		}
	}
}

func TestCompatibleARefusedFormatIsOneCallAndAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format *JSONFormat
	}{{"json_schema", testFormat}, {"json_object", nil}} {
		for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
			t.Run(tc.name+"/"+string(kind), func(t *testing.T) {
				budget := &recordingBudget{}
				server, c := scriptedServer(t,
					step{status: 400, body: `{"error":{"message":"response_format json_schema is not supported"}}`},
					step{status: 200, body: chatOK(`{"a":2}`)},
				)
				provider, err := NewCompatible(Config{Kind: kind, APIKey: NewSecret(testKey), BaseURL: server.URL, Model: "m", Budget: budget})
				if err != nil {
					t.Fatal(err)
				}
				_, err = provider.Complete(context.Background(), Request{Prompt: "p", Format: tc.format})
				if class, ok := ClassOf(err); !ok || class != ClassInvalidRequest || c.calls() != 1 {
					t.Fatalf("err = %v calls = %d, want one call and class %q", err, c.calls(), ClassInvalidRequest)
				}
				if len(budget.attempts) != 1 {
					t.Fatalf("budget attempts = %d, want the one billed call recorded", len(budget.attempts))
				}
			})
		}
	}
}
