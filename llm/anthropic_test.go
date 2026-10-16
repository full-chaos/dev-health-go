package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Response bodies here are hand-written in the documented Messages API shape
// (https://platform.claude.com/docs/en/api/messages); no live answer was
// recorded and no test reaches the network.

func anthropicText(text, stopReason string) map[string]any {
	return map[string]any{
		"id": "msg_01", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": stopReason,
		"usage":       map[string]any{"input_tokens": 11, "output_tokens": 7},
	}
}

func newTestAnthropic(t *testing.T, steps ...step) (*Anthropic, *capture) {
	t.Helper()
	server, c := scriptedServer(t, steps...)
	provider, err := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL + "/", Model: "claude-haiku-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider, c
}

// boundedFormat carries the keywords Anthropic rejects, on a property whose
// name is itself a keyword.
func boundedFormat() *JSONFormat {
	return &JSONFormat{Name: "categorization", Schema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"maximum", "quotes"},
		"properties": map[string]any{
			"maximum": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"quotes": map[string]any{"type": "array", "minItems": 1, "maxItems": 10,
				"items": map[string]any{"type": "string", "minLength": 1, "maxLength": 280}},
			"tags": map[string]any{"type": "array", "minItems": 2, "items": map[string]any{"type": "string"}},
		},
	}}
}

func TestAnthropicSendsTheMessagesRequest(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"ok": true}`, "end_turn")})
	format := boundedFormat()
	original, _ := json.Marshal(format.Schema)

	result, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "the prompt", Format: format, MaxOutputTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"ok":true}` || result.Model != "claude-haiku-5-5" || result.StopReason != "end_turn" {
		t.Errorf("result = %+v", result)
	}
	if c.paths[0] != "/v1/messages" {
		t.Errorf("path = %q", c.paths[0])
	}
	header := c.headers[0]
	if header.Get("X-Api-Key") != testKey || header.Get("Anthropic-Version") != "2023-06-01" || header.Get("Authorization") != "" {
		t.Errorf("headers: x-api-key set=%v version=%q authorization=%q", header.Get("X-Api-Key") == testKey,
			header.Get("Anthropic-Version"), header.Get("Authorization"))
	}
	want := map[string]any{
		"model": "claude-haiku-5-5", "max_tokens": float64(4096), "system": "sys",
		"messages": []any{map[string]any{"role": "user", "content": "the prompt"}},
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"maximum", "quotes"},
			"properties": map[string]any{
				"maximum": map[string]any{"type": "number"},
				"quotes":  map[string]any{"type": "array", "minItems": float64(1), "items": map[string]any{"type": "string"}},
				"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		}}},
	}
	if got := c.request(0); !reflect.DeepEqual(got, want) {
		t.Errorf("body = %#v\nwant %#v", got, want)
	}
	if after, _ := json.Marshal(format.Schema); string(after) != string(original) {
		t.Error("Complete modified the caller's schema")
	}
}

func TestAnthropicSchemaFreeRequestReturnsTheTextAsWritten(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText("not json at all", "end_turn")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "explain", MaxOutputTokens: 6000})
	if err != nil || result.Text != "not json at all" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if _, present := c.request(0)["output_config"]; present {
		t.Error("a schema-free request carries output_config")
	}
	if c.request(0)["max_tokens"] != float64(6000) {
		t.Errorf("max_tokens = %v, want the request floor 6000", c.request(0)["max_tokens"])
	}
}

func TestAnthropicJoinsTextBlocksAndCountsCachedTokens(t *testing.T) {
	provider, _ := newTestAnthropic(t, step{status: 200, body: `{"content":[{"type":"thinking","thinking":"","signature":"sig"},` +
		`{"type":"text","text":"{\"a\":"},{"type":"text","text":"1}"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":9}}`})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"a":1}` {
		t.Errorf("Text = %q, want the two text blocks joined, the thinking block skipped", result.Text)
	}
	if *result.Usage.InputTokens != 150 || *result.Usage.CachedInputTokens != 30 || *result.Usage.OutputTokens != 9 {
		t.Errorf("usage = %d/%d/%d, want 150 in (100+20+30), 30 cached, 9 out",
			*result.Usage.InputTokens, *result.Usage.CachedInputTokens, *result.Usage.OutputTokens)
	}
}

func TestAnthropicRetriesWithoutOutputConfigAfterA400(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"output_config: not supported"}}`},
		step{status: 200, body: anthropicText("```json\n{\"ok\": 1}\n```", "end_turn")},
	)
	result, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatal(err)
	}
	if c.calls() != 2 {
		t.Fatalf("calls = %d", c.calls())
	}
	if _, present := c.request(0)["output_config"]; !present {
		t.Error("the first attempt carries no output_config")
	}
	if _, present := c.request(1)["output_config"]; present || c.request(1)["system"] != "sys" {
		t.Error("the retry kept output_config or lost the system message")
	}
	if result.Text != "" {
		t.Errorf("Text = %q, want empty: a fenced answer is not JSON, the caller repairs", result.Text)
	}
}

func TestAnthropicDoublesTheCapWhenCut(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 200, body: anthropicText(`{"cut": `, "max_tokens")},
		step{status: 200, body: anthropicText(`{"cut": false}`, "end_turn")},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != `{"cut":false}` {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if c.request(1)["max_tokens"] != float64(8192) {
		t.Errorf("retry max_tokens = %v, want 8192", c.request(1)["max_tokens"])
	}
}

func TestAnthropicRefusalIsAnEmptyCompletion(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200,
		body: `{"content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":null},"usage":{"input_tokens":5,"output_tokens":0}}`})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != "" || result.StopReason != "refusal" || c.calls() != 1 {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
}

func TestAnthropicClassifiesItsErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass ErrorClass
		wantCalls int
	}{
		{"401 is final", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, ClassAuth, 1},
		{"429 is retried once", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, ClassRateLimit, 2},
		{"529 overloaded is retried once", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ClassServer, 2},
		{"413 is an invalid request", 413, `{"type":"error","error":{"type":"request_too_large","message":"too big"}}`, ClassInvalidRequest, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, c := newTestAnthropic(t, step{status: tc.status, body: tc.body, header: http.Header{"Retry-After": {"0"}}})
			_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
			class, ok := ClassOf(err)
			if !ok || class != tc.wantClass || c.calls() != tc.wantCalls {
				t.Errorf("class = %q ok=%v calls=%d, want %q %d (err %v)", class, ok, c.calls(), tc.wantClass, tc.wantCalls, err)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Error("error carries the key")
			}
		})
	}
}

func TestAnthropicUnderABudget(t *testing.T) {
	server, c := scriptedServer(t, step{status: 503, body: `{}`}, step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")})
	budget := &recordingBudget{}
	provider, _ := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: budget})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if c.calls() != 2 || len(budget.attempts) != 2 || budget.attempts[0].IdempotencyKey == budget.attempts[1].IdempotencyKey {
		t.Fatalf("calls=%d attempts=%v", c.calls(), budget.attempts)
	}
	if a := budget.attempts[0]; a.Provider != KindAnthropic || a.Model != "claude-haiku-5-5" || a.MaxOutputTokens != 4096 {
		t.Errorf("attempt = %+v", a)
	}
	refusing := &recordingBudget{refuse: errors.New("pricing unavailable")}
	refused, _ := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: refusing})
	before := c.calls()
	if _, err := refused.Complete(context.Background(), Request{Prompt: "p"}); !IsBudgetRefusal(err) || c.calls() != before {
		t.Fatalf("err = %v, sent %d more", err, c.calls()-before)
	}
}

func TestNewAnthropicDefaults(t *testing.T) {
	if _, err := NewAnthropic(Config{}); err == nil {
		t.Fatal("built with no key")
	}
	provider, err := New(Config{Kind: KindAnthropic, APIKey: NewSecret(testKey)})
	if err != nil {
		t.Fatal(err)
	}
	a := provider.(*Anthropic)
	if a.cfg.BaseURL != "https://api.anthropic.com" || a.Model() != "claude-haiku-5-5" || a.cfg.MaxOutputTokens != 4096 {
		t.Errorf("defaults: %+v", a.view())
	}
	assertNoKey(t, "Anthropic", *a)
}

func TestAnthropicCapAboveTheRetryCeilingIsKept(t *testing.T) {
	server, c := scriptedServer(t,
		step{status: 200, body: anthropicText(`{"cut": `, "max_tokens")},
		step{status: 200, body: anthropicText(`{"cut": false}`, "end_turn")},
	)
	provider, _ := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, MaxOutputTokens: 20000})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if got := c.request(1)["max_tokens"]; got != float64(20000) {
		t.Errorf("retry max_tokens = %v, want 20000 kept", got)
	}
}

func TestAnthropicRefusalWithTextIsStillEmpty(t *testing.T) {
	for _, format := range []*JSONFormat{nil, testFormat} {
		provider, c := newTestAnthropic(t, step{status: 200, body: map[string]any{
			"content":     []any{map[string]any{"type": "text", "text": `{"a": 1}`}},
			"stop_reason": "refusal", "stop_details": map[string]any{"type": "refusal", "category": "cyber"},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 3},
		}})
		result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: format})
		if err != nil || result.Text != "" || result.StopReason != "refusal" || c.calls() != 1 {
			t.Errorf("format=%v: result = %+v err = %v calls = %d, want an empty refusal from one call", format != nil, result, err, c.calls())
		}
		if result.Usage.InputTokens == nil || *result.Usage.InputTokens != 5 {
			t.Errorf("a refusal's usage is still reported: %+v", result.Usage)
		}
	}
}

func TestAnthropicEmptyCutAnswerIsRetriedWithADoubledCap(t *testing.T) {
	provider, c := newTestAnthropic(t,
		step{status: 200, body: map[string]any{"content": []any{}, "stop_reason": "max_tokens", "usage": map[string]any{"input_tokens": 5, "output_tokens": 4096}}},
		step{status: 200, body: anthropicText(`{"a": 1}`, "end_turn")},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != `{"a":1}` || c.calls() != 2 {
		t.Fatalf("result = %+v err = %v calls = %d, want the doubled retry to answer", result, err, c.calls())
	}
}

func TestAnthropicSchemaKeepsDataAndDropsArrayAndObjectBounds(t *testing.T) {
	got := anthropicSchema(map[string]any{
		"type": "object", "additionalProperties": false, "minProperties": 1, "maxProperties": 3,
		"properties": map[string]any{
			"tags":  map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}},
			"limit": map[string]any{"type": "number", "default": map[string]any{"maximum": 1}, "enum": []any{map[string]any{"minimum": 2}}},
		},
	})
	encoded, _ := json.Marshal(got)
	for _, dropped := range []string{"uniqueItems", "minProperties", "maxProperties"} {
		if strings.Contains(string(encoded), dropped) {
			t.Errorf("%s was sent: %s", dropped, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"default":{"maximum":1}`) || !strings.Contains(string(encoded), `"enum":[{"minimum":2}]`) {
		t.Errorf("a default or enum value was changed: %s", encoded)
	}
}
