package llm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func chatOK(text string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	}
}

func newTestCompatible(t *testing.T, kind Kind, steps ...step) (*Compatible, *capture) {
	t.Helper()
	server, c := scriptedServer(t, steps...)
	provider, err := NewCompatible(Config{Kind: kind, APIKey: NewSecret(testKey), BaseURL: server.URL + "/v1/", Model: "m-" + string(kind)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider, c
}

func TestCompatibleSendsTheChatCompletionsRequest(t *testing.T) {
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		t.Run(string(kind), func(t *testing.T) {
			provider, c := newTestCompatible(t, kind, step{status: 200, body: chatOK(`{"a": 1}`)})
			result, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "the prompt", Format: testFormat, MaxOutputTokens: 2048})
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != `{"a":1}` || result.Model != "m-"+string(kind) || result.StopReason != "stop" ||
				*result.Usage.InputTokens != 11 || *result.Usage.OutputTokens != 7 || result.Usage.CachedInputTokens != nil {
				t.Errorf("result = %+v", result)
			}
			if c.paths[0] != "/v1/chat/completions" || c.headers[0].Get("Authorization") != "Bearer "+testKey {
				t.Errorf("path %q, authorization carries the key: %v", c.paths[0], c.headers[0].Get("Authorization") == "Bearer "+testKey)
			}
			want := map[string]any{
				"model": "m-" + string(kind),
				"messages": []any{
					map[string]any{"role": "system", "content": "sys"},
					map[string]any{"role": "user", "content": "the prompt"},
				},
				"max_completion_tokens": float64(4096),
				"temperature":           0.3,
				"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
					"name": "categorization", "strict": true, "schema": map[string]any{
						"type": "object", "additionalProperties": false, "required": []any{"a"},
						"properties": map[string]any{"a": map[string]any{"type": "number"}},
					},
				}},
			}
			if got := c.request(0); !reflect.DeepEqual(got, want) {
				t.Errorf("body = %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestCompatibleFallsBackToTextAfterA400(t *testing.T) {
	provider, c := newTestCompatible(t, KindGemini,
		step{status: 400, body: `{"error":{"message":"response_format json_schema is not supported"}}`},
		step{status: 200, body: chatOK(`{"a":2}`)},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != `{"a":2}` || c.calls() != 2 {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
	if format := c.request(1)["response_format"]; !reflect.DeepEqual(format, map[string]any{"type": "text"}) {
		t.Errorf("retry response_format = %v, want text", format)
	}
}

func TestCompatibleSchemaFreeRequestReturnsTheTextAsWritten(t *testing.T) {
	provider, c := newTestCompatible(t, KindQwen, step{status: 200, body: chatOK("plain words")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if err != nil || result.Text != "plain words" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if format := c.request(0)["response_format"]; !reflect.DeepEqual(format, map[string]any{"type": "json_object"}) {
		t.Errorf("schema-free response_format = %v, want json_object", format)
	}
}

func TestCompatibleSchemaFreeFallsBackToTextAfterA400(t *testing.T) {
	provider, c := newTestCompatible(t, KindLocal,
		step{status: 400, body: `{"error":{"message":"response_format json_object is not supported"}}`},
		step{status: 200, body: chatOK("prose answer")},
	)
	result, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if err != nil || result.Text != "prose answer" || c.calls() != 2 {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
	if format := c.request(1)["response_format"]; !reflect.DeepEqual(format, map[string]any{"type": "text"}) {
		t.Errorf("retry response_format = %v, want text", format)
	}
}

func TestCompatibleInvalidJSONUnderASchemaIsEmpty(t *testing.T) {
	provider, c := newTestCompatible(t, KindLocal, step{status: 200, body: chatOK("not json")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Text != "" || c.calls() != 1 {
		t.Fatalf("result = %+v err = %v calls = %d", result, err, c.calls())
	}
}

func TestCompatibleNoChoicesIsAGenericFailure(t *testing.T) {
	provider, _ := newTestCompatible(t, KindGemini, step{status: 200, body: map[string]any{"choices": []any{}}})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Class != ClassGeneric || typed.Provider != KindGemini {
		t.Fatalf("err = %v, want a generic gemini failure", err)
	}
}

func TestCompatibleClassifiesUnderItsOwnKind(t *testing.T) {
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		provider, c := newTestCompatible(t, kind, step{status: 401, body: `{}`})
		_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
		var typed *Error
		if !errors.As(err, &typed) || typed.Class != ClassAuth || typed.Provider != kind || c.calls() != 1 {
			t.Errorf("%s: err = %v calls = %d", kind, err, c.calls())
		}
	}
}

func TestCompatibleDefaults(t *testing.T) {
	cases := []struct {
		kind      Kind
		wantBase  string
		wantModel string
	}{
		{KindLocal, "http://localhost:11434/v1", "llama3.2"},
		{KindGemini, "https://generativelanguage.googleapis.com/v1beta/openai", "gemini-3.8-flash"},
		{KindQwen, "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-plus"},
	}
	for _, tc := range cases {
		provider, err := New(Config{Kind: tc.kind, APIKey: NewSecret(testKey)})
		if err != nil {
			t.Fatal(err)
		}
		compatible := provider.(*Compatible)
		if compatible.cfg.BaseURL != tc.wantBase || compatible.Model() != tc.wantModel || compatible.Kind() != tc.kind {
			t.Errorf("%s defaults: %+v", tc.kind, compatible.view())
		}
		assertNoKey(t, string(tc.kind), *compatible)
	}
	local, err := New(Config{Kind: KindLocal})
	if err != nil || local.(*Compatible).cfg.APIKey.Reveal() != "not-needed" {
		t.Fatalf("local without a key: %v", err)
	}
	for _, kind := range []Kind{KindGemini, KindQwen} {
		if _, err := New(Config{Kind: kind}); err == nil {
			t.Errorf("%s built with no key", kind)
		}
	}
	if _, err := NewCompatible(Config{Kind: KindOpenAI, APIKey: NewSecret(testKey)}); err == nil {
		t.Error("NewCompatible built an openai client")
	}
}

func TestCompatibleNoChoiceStillReportsItsUsage(t *testing.T) {
	budget := &recordingBudget{}
	server, _ := scriptedServer(t, step{status: 200, body: map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7}}})
	provider, _ := NewCompatible(Config{Kind: KindGemini, APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: budget})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p"}); err == nil {
		t.Fatal("a reply with no choice answered as success")
	}
	if len(budget.usages) != 1 || budget.usages[0].InputTokens == nil || *budget.usages[0].InputTokens != 11 {
		t.Fatalf("budget settled with %+v, want the reported 11 input tokens", budget.usages)
	}
}
