package llm

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// providerWire is one Kind's wire protocol as a test server needs it: the
// path it posts to, and how a completion with usage is answered.
type providerWire struct {
	path   string
	answer func(text string, input, output int) any
}

var providerWires = map[Kind]providerWire{
	KindOpenAI: {path: "/responses", answer: func(text string, input, output int) any {
		return map[string]any{"output_text": text, "usage": map[string]any{
			"input_tokens": input, "output_tokens": output, "input_tokens_details": map[string]any{"cached_tokens": 0},
		}}
	}},
}

// TestEveryKindHonoursTheSameContract runs every Kind New can build through
// the same cases, so the surface the callers rely on cannot drift per
// provider. The list is Kinds() itself: a Kind with no wire entry fails.
func TestEveryKindHonoursTheSameContract(t *testing.T) {
	cases := []struct {
		name      string
		steps     []step
		wantText  string
		wantClass ErrorClass
		wantCalls int
		wantUsage bool
	}{
		{name: "valid JSON under a schema comes back compact", steps: []step{{status: 200, body: "a"}}, wantText: `{"a":1}`, wantCalls: 1, wantUsage: true},
		{name: "invalid JSON under a schema is an empty completion", steps: []step{{status: 200, body: "bad"}}, wantText: "", wantCalls: 1},
		{name: "a transient 503 is retried once", steps: []step{{status: 503}, {status: 200, body: "a"}}, wantText: `{"a":1}`, wantCalls: 2, wantUsage: true},
		{name: "a 401 is final and classified auth", steps: []step{{status: 401}}, wantClass: ClassAuth, wantCalls: 1},
	}
	texts := map[string]string{"a": `{"a": 1}`, "bad": "not json"}

	for _, kind := range Kinds() {
		wire, ok := providerWires[kind]
		if !ok {
			t.Errorf("kind %q has no wire entry in providerWires: add it so the contract covers it", kind)
			continue
		}
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				steps := make([]step, len(tc.steps))
				for i, s := range tc.steps {
					steps[i] = step{status: s.status, body: `{"error":{"message":"failure"}}`}
					if s.status == 200 {
						steps[i].body = wire.answer(texts[s.body.(string)], 13, 5)
					}
				}
				server, c := scriptedServer(t, steps...)
				recorder := tracetest.NewSpanRecorder()
				previous := otel.GetTracerProvider()
				otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
				t.Cleanup(func() { otel.SetTracerProvider(previous) })

				provider, err := New(Config{Kind: kind, APIKey: NewSecret(testKey), BaseURL: server.URL, Model: "contract-" + string(kind)})
				if err != nil {
					t.Fatal(err)
				}
				assertNoKey(t, string(kind), provider)
				result, err := provider.Complete(WithRole(context.Background(), RoleServed), Request{Prompt: "p", Format: testFormat})

				if c.calls() != tc.wantCalls {
					t.Errorf("calls = %d, want %d", c.calls(), tc.wantCalls)
				}
				for i := 0; i < c.calls(); i++ {
					if c.paths[i] != wire.path || c.request(i)["model"] != provider.Model() {
						t.Errorf("request %d: path %q model %v, want %q %q", i, c.paths[i], c.request(i)["model"], wire.path, provider.Model())
					}
				}
				spans := recorder.Ended()
				if len(spans) != tc.wantCalls {
					t.Fatalf("spans = %d, want one per HTTP attempt (%d)", len(spans), tc.wantCalls)
				}
				for i, span := range spans {
					attrs := map[attribute.Key]attribute.Value{}
					for _, kv := range span.Attributes() {
						attrs[kv.Key] = kv.Value
					}
					if span.Name() != "dev_health.llm.request" || attrs["llm.provider"].AsString() != string(kind) ||
						attrs["llm.model"].AsString() != provider.Model() || attrs["llm.attempt"].AsInt64() != int64(i+1) ||
						attrs["llm.role"].AsString() != "served" || attrs["http.response.status_code"].AsInt64() == 0 {
						t.Errorf("span %d = %s %v", i, span.Name(), span.Attributes())
					}
				}

				if tc.wantClass != "" {
					class, ok := ClassOf(err)
					if !ok || class != tc.wantClass {
						t.Fatalf("err = %v, want class %q", err, tc.wantClass)
					}
					if last := spans[len(spans)-1]; attributeOf(last, "error.class") != "auth" {
						t.Errorf("error.class = %q, want auth", attributeOf(last, "error.class"))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.Text != tc.wantText || result.Model != provider.Model() {
					t.Errorf("result = %+v", result)
				}
				if tc.wantUsage && (result.Usage.InputTokens == nil || *result.Usage.InputTokens != 13 || *result.Usage.OutputTokens != 5) {
					t.Errorf("usage = %+v, want 13/5", result.Usage)
				}
			})
		}
	}
}

func attributeOf(span sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
