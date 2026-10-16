package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Caller-relied response checks of the Chat Completions client (local, gemini,
// qwen). The ops categorize and explain callers consume Result's text (and the
// JSON in it), Usage (input, output; nil = not reported), the stop reason, and
// the error class. For each recorded answer in
// testdata/compatible_responses.golden.json, the parsed Result must give those
// callers what they got from the previous clients, the deleted local.py,
// gemini.py and qwen.py (their parsed outcomes were recorded once with the
// answers). The answers are documented-shape bodies, not live recordings.
// Request shape is not compared: this client follows the current APIs.

const compatibleResponsesGoldenDigest = "92325ccb55dc34c546d959784d208fd0a843a3d7876e16815e86887cad3d8371"

// compatibleImprovements are the cases this client answers better than the
// old ones, on purpose. There are none on the response side.
var compatibleImprovements = map[string]string{}

func TestCompatibleCallerReliedResponses(t *testing.T) {
	_, cases := loadRecordedCases(t, "testdata/compatible_responses.golden.json", compatibleResponsesGoldenDigest)
	if len(cases) < 17 {
		t.Fatalf("%d cases: the golden is not the recording", len(cases))
	}
	kinds := map[string]int{}
	for _, oc := range cases {
		kinds[oc.Kind]++
		server, _ := serveRecordedAnswers(t, oc.Answers)
		first := plainJSON(t, oc.Requests[0]["body"]).(map[string]any)
		request := Request{System: "s", Prompt: oc.Prompt}
		if format, ok := first["response_format"].(map[string]any); ok && format["type"] == "json_schema" {
			spec := format["json_schema"].(map[string]any)
			request.Format = &JSONFormat{Name: spec["name"].(string), Schema: spec["schema"].(map[string]any)}
		}
		provider, err := NewCompatible(Config{Kind: Kind(oc.Kind), APIKey: NewSecret("oracle-placeholder"),
			BaseURL: server.URL + "/v1", Model: first["model"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := provider.Complete(context.Background(), request)

		equal, detail := true, ""
		fail := func(format string, args ...any) {
			if equal {
				equal, detail = false, fmt.Sprintf(format, args...)
			}
		}
		class, _ := ClassOf(err)
		var typed *Error
		if asError(err, &typed) && string(typed.Provider) != oc.Kind {
			fail("error labelled %q, want %q", typed.Provider, oc.Kind)
		}
		want := oc.Outcome["error_class"].(string)
		switch {
		case string(class) != want:
			fail("error class go=%q recorded=%q (err %v)", class, want, err)
		case want == "":
			if !sameText(result.Text, oc.Outcome["text"].(string)) {
				fail("text go=%q recorded=%q", result.Text, oc.Outcome["text"])
			}
			if result.Model != oc.Outcome["model"] {
				fail("model go=%q recorded=%q", result.Model, oc.Outcome["model"])
			}
			if !sameTokens(result.Usage.InputTokens, oc.Outcome["input_tokens"]) || !sameTokens(result.Usage.OutputTokens, oc.Outcome["output_tokens"]) {
				fail("tokens go=%v/%v recorded=%v/%v", deref(result.Usage.InputTokens), deref(result.Usage.OutputTokens), oc.Outcome["input_tokens"], oc.Outcome["output_tokens"])
			}
			var body struct {
				Choices []struct {
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			_ = json.Unmarshal([]byte(oc.Answers[len(oc.Answers)-1]["body"].(string)), &body)
			if len(body.Choices) > 0 && result.StopReason != body.Choices[0].FinishReason {
				fail("stop reason go=%q answer=%q", result.StopReason, body.Choices[0].FinishReason)
			}
		}
		divergenceCheck(t, compatibleImprovements, oc.Name, equal, detail)
	}
	for _, kind := range []string{"local", "gemini", "qwen"} {
		if kinds[kind] == 0 {
			t.Errorf("the golden has no %s case", kind)
		}
	}
}

// TestCompatibleCallerReliedErrorClasses maps the documented errors of the
// OpenAI-compatible endpoints (Gemini: https://ai.google.dev/gemini-api/docs/troubleshooting;
// DashScope: https://www.alibabacloud.com/help/en/model-studio/error-code)
// to the class the callers' retry and fallback decisions read. The bodies are
// documented examples, not live recordings. A 400 is asked once more with a
// text format (a server may reject the JSON format), so it takes two calls.
func TestCompatibleCallerReliedErrorClasses(t *testing.T) {
	cases := []struct {
		kind      Kind
		status    int
		body      string
		wantClass ErrorClass
		wantCalls int
	}{
		{KindGemini, 400, `[{"error":{"code":400,"message":"Invalid JSON payload","status":"INVALID_ARGUMENT"}}]`, ClassInvalidRequest, 2},
		{KindGemini, 403, `[{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}]`, ClassAuth, 1},
		{KindGemini, 404, `[{"error":{"code":404,"message":"models/gemini-nope is not found","status":"NOT_FOUND"}}]`, ClassModelNotFound, 1},
		{KindGemini, 429, `[{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}]`, ClassRateLimit, 2},
		{KindGemini, 500, `[{"error":{"code":500,"message":"An internal error has occurred","status":"INTERNAL"}}]`, ClassServer, 2},
		{KindGemini, 503, `[{"error":{"code":503,"message":"The model is overloaded","status":"UNAVAILABLE"}}]`, ClassServer, 2},
		{KindGemini, 504, `[{"error":{"code":504,"message":"Deadline exceeded","status":"DEADLINE_EXCEEDED"}}]`, ClassServer, 2},
		{KindQwen, 400, `{"error":{"code":"invalid_parameter_error","message":"Range of input length should be [1, 129024]","type":"invalid_request_error"}}`, ClassInvalidRequest, 2},
		{KindQwen, 401, `{"error":{"code":"invalid_api_key","message":"Incorrect API key provided.","type":"invalid_request_error"}}`, ClassAuth, 1},
		{KindQwen, 429, `{"error":{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded","type":"requests"}}`, ClassRateLimit, 2},
		{KindQwen, 500, `{"error":{"code":"InternalError","message":"An internal error has occured","type":"internal_error"}}`, ClassServer, 2},
		{KindLocal, 400, `{"error":{"message":"context length exceeded: maximum context length is 8192 tokens","type":"invalid_request_error"}}`, ClassContextLength, 2},
		{KindLocal, 408, `{"error":{"message":"request timeout"}}`, ClassTimeout, 2},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d", tc.kind, tc.status), func(t *testing.T) {
			server, c := scriptedServer(t, step{status: tc.status, body: tc.body, header: map[string][]string{"Retry-After": {"0"}}})
			provider, _ := NewCompatible(Config{Kind: tc.kind, APIKey: NewSecret(testKey), BaseURL: server.URL})
			_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
			class, ok := ClassOf(err)
			if !ok || class != tc.wantClass || c.calls() != tc.wantCalls {
				t.Errorf("class = %q calls = %d, want %q %d (err %v)", class, c.calls(), tc.wantClass, tc.wantCalls, err)
			}
		})
	}
}

func TestGeminiLiveSmoke(t *testing.T) { runLiveSmoke(t, KindGemini) }
func TestQwenLiveSmoke(t *testing.T)   { runLiveSmoke(t, KindQwen) }
