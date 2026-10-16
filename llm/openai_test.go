package llm

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func openAIOK(text string) map[string]any {
	return map[string]any{"output_text": text, "usage": map[string]any{
		"input_tokens": 11, "output_tokens": 7, "input_tokens_details": map[string]any{"cached_tokens": 3},
	}}
}

func newTestOpenAI(t *testing.T, steps ...step) (*OpenAI, *capture) {
	t.Helper()
	server, c := scriptedServer(t, steps...)
	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL + "/", Model: "gpt-5-nano"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider, c
}

var testFormat = &JSONFormat{Name: "categorization", Schema: map[string]any{
	"type": "object", "additionalProperties": false, "required": []any{"a"},
	"properties": map[string]any{"a": map[string]any{"type": "number"}},
}}

func TestOpenAISendsTheResponsesRequest(t *testing.T) {
	provider, c := newTestOpenAI(t, step{status: 200, body: openAIOK(`{"a": 1}`)})
	result, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "the prompt", Format: testFormat, MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"a":1}` || result.Model != "gpt-5-nano" || result.StopReason != "completed" {
		t.Errorf("result = %+v", result)
	}
	if *result.Usage.InputTokens != 11 || *result.Usage.OutputTokens != 7 || *result.Usage.CachedInputTokens != 3 {
		t.Errorf("usage = %+v", result.Usage)
	}
	if c.paths[0] != "/responses" {
		t.Errorf("path = %q (a trailing slash on the base URL is trimmed)", c.paths[0])
	}
	if got := c.headers[0].Get("Authorization"); got != "Bearer "+testKey {
		t.Error("Authorization does not carry the configured key")
	}
	body := c.request(0)
	want := map[string]any{
		"model": "gpt-5-nano", "instructions": "sys", "input": "the prompt",
		"text": map[string]any{"verbosity": "low", "format": map[string]any{
			"type": "json_schema", "name": "categorization", "strict": true, "schema": map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"a"},
				"properties": map[string]any{"a": map[string]any{"type": "number"}},
			},
		}},
		"reasoning":         map[string]any{"effort": "low"},
		"max_output_tokens": float64(4096),
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %#v\nwant %#v", body, want)
	}
}

func TestOpenAIOutputFloors(t *testing.T) {
	cases := []struct {
		name         string
		configFloor  int
		requestFloor int
		want         float64
	}{
		{"provider floor 2048", 0, 0, 2048},
		{"request floor raises it", 0, 4096, 4096},
		{"config floor raises it", 6000, 2048, 6000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, c := scriptedServer(t, step{status: 200, body: openAIOK(`{}`)})
			provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, MaxOutputTokens: tc.configFloor})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Complete(context.Background(), Request{Prompt: "p", MaxOutputTokens: tc.requestFloor}); err != nil {
				t.Fatal(err)
			}
			if got := c.request(0)["max_output_tokens"]; got != tc.want {
				t.Errorf("max_output_tokens = %v, want %v", got, tc.want)
			}
			if got := c.request(0)["model"]; got != "gpt-5-mini" {
				t.Errorf("model = %v, want the default gpt-5-mini", got)
			}
		})
	}
}

func TestOpenAISchemaFreeRequestAsksForAJSONObject(t *testing.T) {
	provider, c := newTestOpenAI(t, step{status: 200, body: map[string]any{
		"output": []any{map[string]any{"content": []any{
			map[string]any{"type": "output_text", "text": `{"b":`},
			map[string]any{"type": "refusal", "text": "ignored"},
			map[string]any{"type": "text", "text": `2}`},
		}}},
	}})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"b":2}` {
		t.Errorf("Text = %q, want the output_text/text chunks joined", result.Text)
	}
	if result.Usage.InputTokens != nil {
		t.Error("an unreported usage count must stay nil, never 0")
	}
	format := c.request(0)["text"].(map[string]any)["format"]
	if !reflect.DeepEqual(format, map[string]any{"type": "json_object"}) {
		t.Errorf("format = %v, want json_object", format)
	}
}

func TestOpenAIDoublesTheCapWhenCut(t *testing.T) {
	provider, c := newTestOpenAI(t,
		step{status: 200, body: map[string]any{"output_text": `{"a":`, "incomplete_details": map[string]any{"reason": "max_output_tokens"}}},
		step{status: 200, body: openAIOK(`{"a":2}`)},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != `{"a":2}` {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if c.request(1)["max_output_tokens"] != float64(4096) {
		t.Errorf("retry cap = %v, want 4096", c.request(1)["max_output_tokens"])
	}
}

func TestOpenAIInvalidJSONIsAnEmptyCompletion(t *testing.T) {
	provider, c := newTestOpenAI(t, step{status: 200, body: openAIOK("not json")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != "" || c.calls() != 1 {
		t.Fatalf("result = %+v err = %v calls = %d, want an empty completion from one call", result, err, c.calls())
	}
}

func TestOpenAIRetriesOnceAndClassifies(t *testing.T) {
	cases := []struct {
		name      string
		steps     []step
		wantClass ErrorClass
		wantCalls int
	}{
		{"401 is final", []step{{status: 401, body: `{"error":{"code":"invalid_api_key"}}`}}, ClassAuth, 1},
		{"400 is final", []step{{status: 400, body: `{"error":{"type":"invalid_request_error"}}`}}, ClassInvalidRequest, 1},
		{"429 twice", []step{{status: 429, body: `{}`, header: http.Header{"Retry-After": {"0"}}}}, ClassRateLimit, 2},
		{"503 twice", []step{{status: 503, body: `{}`}}, ClassServer, 2},
		{"garbage 200 is generic and final", []step{{status: 200, body: `{not json`}}, ClassGeneric, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, c := newTestOpenAI(t, tc.steps...)
			_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
			class, ok := ClassOf(err)
			if !ok || class != tc.wantClass {
				t.Errorf("class = %q ok=%v, want %q (err %v)", class, ok, tc.wantClass, err)
			}
			if c.calls() != tc.wantCalls {
				t.Errorf("calls = %d, want %d", c.calls(), tc.wantCalls)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Error("error text carries the key")
			}
		})
	}
}

func TestOpenAINeverFollowsARedirect(t *testing.T) {
	target, targetCalls := scriptedServer(t, step{status: 200, body: openAIOK(`{}`)})
	redirect, _ := scriptedServer(t, step{status: 307, header: http.Header{"Location": {target.URL + "/responses"}}})
	// A caller-supplied client that WOULD follow redirects is still refused.
	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: redirect.URL, HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p"}); err == nil {
		t.Fatal("a redirect answered as success")
	}
	if targetCalls.calls() != 0 {
		t.Fatal("the redirect was followed: the key reached another host")
	}
}

func TestNewOpenAIRequiresAKey(t *testing.T) {
	if _, err := NewOpenAI(Config{}); err == nil {
		t.Fatal("built with no key")
	}
	provider, err := New(Config{Kind: KindOpenAI, APIKey: NewSecret(testKey)})
	if err != nil || provider.Kind() != KindOpenAI || provider.Model() != DefaultModel(KindOpenAI) {
		t.Fatalf("New(openai) = %v, %v", provider, err)
	}
	if _, err := New(Config{Kind: "nope", APIKey: NewSecret(testKey)}); err == nil {
		t.Fatal("an unknown kind built a provider")
	}
}
