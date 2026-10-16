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

func TestAnthropicFallsBackToAForcedToolCallWhenTheSchemaIsRefused(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 400, body: schemaRefused},
		step{status: 200, body: anthropicToolUse("categorization", `{"a": 1}`, "tool_use")},
	)
	result, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatal(err)
	}
	if c.calls() != 2 || result.Text != `{"a":1}` || result.Unparsed != "" || *result.Usage.OutputTokens != 9 || result.FormatPath != FormatPathTool {
		t.Fatalf("calls = %d, result = %+v", c.calls(), result)
	}
	retry := c.request(1)
	if _, present := retry["output_config"]; present || retry["system"] != "sys" {
		t.Fatalf("the tool call kept output_config or lost the system message: %v", retry)
	}
	tools, _ := retry["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want one", retry["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "categorization" {
		t.Fatalf("tool = %v, want name categorization", tool)
	}
	if description, _ := tool["description"].(string); !strings.Contains(description, "never move a top-level property inside an object property") {
		t.Fatalf("description = %q, want the placement rule (live: without it, a model nested top-level fields)", description)
	}
	if !reflect.DeepEqual(tool["input_schema"], roundTrip(t, testFormat.Schema)) {
		t.Fatalf("input_schema = %v, want the Format's schema as given", tool["input_schema"])
	}
	if !reflect.DeepEqual(retry["tool_choice"], map[string]any{"type": "tool", "name": "categorization"}) {
		t.Fatalf("tool_choice = %v", retry["tool_choice"])
	}
}

func TestAnthropicFallsBackToPlainTextWhenTheToolCallIsRefusedToo(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 400, body: schemaRefused},
		step{status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"tools.0.input_schema: invalid"}}`},
		step{status: 200, body: anthropicText(`{"a": 3}`, "end_turn")},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != `{"a":3}` || c.calls() != 3 || result.FormatPath != FormatPathText {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
	last := c.request(2)
	for _, field := range []string{"output_config", "tools", "tool_choice"} {
		if _, present := last[field]; present {
			t.Errorf("the plain-text call still carries %s", field)
		}
	}
}

func TestAnthropicFormatFallbacksDoNotSpendTheTransientRetry(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 400, body: schemaRefused},
		step{status: 503, body: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`},
		step{status: 200, body: anthropicToolUse("categorization", `{"a": 2}`, "tool_use")},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != `{"a":2}` || c.calls() != 3 {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
	if _, present := c.request(2)["tools"]; !present {
		t.Fatal("the transient retry dropped back from the tool call")
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
	provider, c := newTestAnthropic(t,
		step{status: 400, body: schemaRefused},
		step{status: 200, body: anthropicToolUse("bounded", `{"maximum": 1}`, "tool_use")},
	)
	format := boundedFormat()
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: format}); err != nil {
		t.Fatal(err)
	}
	tool := c.request(1)["tools"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(tool["input_schema"], roundTrip(t, format.Schema)) {
		t.Fatalf("input_schema = %v, want the Format's schema with its bounds", tool["input_schema"])
	}
	if reflect.DeepEqual(c.request(0)["output_config"].(map[string]any)["format"].(map[string]any)["schema"], roundTrip(t, format.Schema)) {
		t.Fatal("the bounded schema was not stripped for structured outputs; this test proves nothing")
	}
}

func TestAnthropicTransientRetryStaysOneAfterAFormatFallback(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 400, body: schemaRefused},
		step{status: 503, body: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := provider.Complete(ctx, Request{Prompt: "p", Format: testFormat})
	if class, ok := ClassOf(err); !ok || class != ClassServer || c.calls() != 3 {
		t.Fatalf("err = %v calls = %d, want a server error after 3 calls (schema, tool, one retry)", err, c.calls())
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

func TestCompatibleTextFallbackReportsTheTextPath(t *testing.T) {
	provider, _ := newTestCompatible(t, KindGemini,
		step{status: 400, body: `{"error":{"message":"response_format json_schema is not supported"}}`},
		step{status: 200, body: chatOK(`{"a":2}`)},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.FormatPath != FormatPathText {
		t.Fatalf("FormatPath = %q err = %v, want %q", result.FormatPath, err, FormatPathText)
	}
}
