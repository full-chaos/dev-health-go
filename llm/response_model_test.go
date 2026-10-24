package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The fake transport answers every request with one fixed body; no listener
// and no network are used.
type bodyTransport struct{ body string }

func (b bodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(b.body)),
	}, nil
}

func TestResultResponseModelPerSendPath(t *testing.T) {
	const configured = "configured-model"
	anthropicBody := func(model string) string {
		return `{"type":"message",` + model + `"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	}
	openAIBody := func(model string) string {
		return `{` + model + `"output_text":"hi","usage":{"input_tokens":1,"output_tokens":1}}`
	}
	chatBody := func(model string) string {
		return `{` + model + `"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	}
	paths := []struct {
		name string
		kind Kind
		body func(string) string
	}{
		{"anthropic messages", KindAnthropic, anthropicBody},
		{"openai responses", KindOpenAI, openAIBody},
		{"chat completions local", KindLocal, chatBody},
		{"chat completions gemini", KindGemini, chatBody},
		{"chat completions qwen", KindQwen, chatBody},
	}
	cases := []struct {
		name  string
		field string
		want  string
	}{
		{"differs from configured", `"model":"served-model-9",`, "served-model-9"},
		{"equals configured", `"model":"` + configured + `",`, configured},
		{"absent", ``, ""},
		{"empty", `"model":"",`, ""},
		{"number", `"model":42,`, ""},
		{"null", `"model":null,`, ""},
		{"object", `"model":{"a":1},`, ""},
	}
	for _, path := range paths {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				provider, err := New(Config{
					Kind: path.kind, APIKey: NewSecret(testKey), BaseURL: "http://llm.invalid/v1", Model: configured,
					HTTPClient: &http.Client{Transport: bodyTransport{body: path.body(tc.field)}},
				})
				if err != nil {
					t.Fatal(err)
				}
				result, err := provider.Complete(context.Background(), Request{System: "s", Prompt: "p"})
				if err != nil {
					t.Fatal(err)
				}
				if result.ResponseModel != tc.want || result.Model != configured {
					t.Errorf("ResponseModel=%q Model=%q, want response %q, configured %q", result.ResponseModel, result.Model, tc.want, configured)
				}
			})
		}
	}
}

func TestResponseModelOnRefusal(t *testing.T) {
	body := `{"model":"served-model-9","choices":[{"message":{"role":"assistant","content":"","refusal":"no"},"finish_reason":"stop"}]}`
	provider, err := New(Config{Kind: KindLocal, BaseURL: "http://llm.invalid/v1", Model: "m", HTTPClient: &http.Client{Transport: bodyTransport{body: body}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Complete(context.Background(), Request{System: "s", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Refused || result.ResponseModel != "served-model-9" || result.Model != "m" {
		t.Errorf("result = %+v", result)
	}
}
