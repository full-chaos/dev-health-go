package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Caller-relied response checks of the Anthropic client. The ops categorize
// and explain callers consume Result's text (and the JSON in it), Usage
// (input, output; nil = not reported, for budget and spend accounting), the
// stop reason, and the error class (retry and fallback decisions). For each
// recorded Messages API answer in testdata/anthropic_responses.golden.json,
// the parsed Result must give those callers what they got from the previous
// Anthropic client, the deleted anthropic.py (its parsed outcomes were
// recorded once with the answers). The answers are documented-shape bodies
// (https://platform.claude.com/docs/en/api/messages), not live recordings: no
// Anthropic key exists where they were made.
//
// Request shape is not compared: this client builds its request from the
// current API (structured outputs included), not from the old one.
//
// Where this client now answers better than the old one, the case is named
// below with the reason and must still differ; a better answer that stops
// differing means the case broke.

const anthropicResponsesGoldenDigest = "db6fdf0b019531ce7b237f64e0f657a31f2b04b7d995c254dcd2176c5e46ac36"

var anthropicImprovements = map[string]string{
	"divergence_cache_tokens": "input tokens count cache reads and writes too, the same meaning as the OpenAI " +
		"client's input tokens; the old client under-reported cached prompts",
	"divergence_two_text_blocks": "every text block is joined; the old client kept the first block only",
	"divergence_thinking_first":  "thinking blocks are skipped; the old client failed on a thinking block at content[0]",
}

func TestAnthropicCallerReliedResponses(t *testing.T) {
	_, cases := loadRecordedCases(t, "testdata/anthropic_responses.golden.json", anthropicResponsesGoldenDigest)
	if len(cases) < 15 {
		t.Fatalf("%d cases: the golden is not the recording", len(cases))
	}
	seen := map[string]bool{}
	for _, oc := range cases {
		seen[oc.Name] = true
		server, _ := serveRecordedAnswers(t, oc.Answers)
		model := oc.Requests[0]["body"].(map[string]any)["model"].(string)
		provider, err := NewAnthropic(Config{APIKey: NewSecret("oracle-placeholder"), BaseURL: server.URL, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		result, err := provider.Complete(context.Background(), Request{Prompt: oc.Prompt})

		equal, detail := true, ""
		fail := func(format string, args ...any) {
			if equal {
				equal, detail = false, fmt.Sprintf(format, args...)
			}
		}
		class, _ := ClassOf(err)
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
			// The stop reason is the answer's own, carried as it is.
			var body struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal([]byte(oc.Answers[len(oc.Answers)-1]["body"].(string)), &body)
			if result.StopReason != body.StopReason {
				fail("stop reason go=%q answer=%q", result.StopReason, body.StopReason)
			}
		}
		divergenceCheck(t, anthropicImprovements, oc.Name, equal, detail)
	}
	for name := range anthropicImprovements {
		if !seen[name] {
			t.Errorf("%s: named improvement has no case", name)
		}
	}
}

// TestAnthropicCallerReliedErrorClasses maps each documented Messages API
// error (https://platform.claude.com/docs/en/api/errors) to the class the
// callers' retry and fallback decisions read. The bodies are the documented
// examples, not live recordings.
func TestAnthropicCallerReliedErrorClasses(t *testing.T) {
	cases := []struct {
		status    int
		kind      string
		message   string
		wantClass ErrorClass
		wantCalls int
	}{
		{400, "invalid_request_error", "messages: field required", ClassInvalidRequest, 1},
		{400, "invalid_request_error", "prompt is too long: 250000 tokens > 200000 maximum", ClassContextLength, 1},
		{401, "authentication_error", "invalid x-api-key", ClassAuth, 1},
		{402, "billing_error", "Your credit balance is too low", ClassAuth, 1},
		{403, "permission_error", "Your API key does not have permission to use the specified resource.", ClassAuth, 1},
		{404, "not_found_error", "model: claude-nope", ClassModelNotFound, 1},
		{408, "timeout_error", "Request timed out", ClassTimeout, 2},
		{413, "request_too_large", "Request exceeds the maximum allowed number of bytes.", ClassInvalidRequest, 1},
		{429, "rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit", ClassRateLimit, 2},
		{500, "api_error", "An unexpected error has occurred internal to Anthropic's systems.", ClassServer, 2},
		{504, "timeout_error", "Gateway timeout", ClassTimeout, 2},
		{529, "overloaded_error", "Anthropic's API is temporarily overloaded.", ClassServer, 2},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d_%s", tc.status, tc.kind), func(t *testing.T) {
			body := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q},"request_id":"req_011CSHoEeqs5C35K2UUqR7Fy"}`, tc.kind, tc.message)
			server, c := scriptedServer(t, step{status: tc.status, body: body, header: map[string][]string{"Retry-After": {"0"}}})
			provider, _ := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL})
			_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
			class, ok := ClassOf(err)
			if !ok || class != tc.wantClass || c.calls() != tc.wantCalls {
				t.Errorf("class = %q calls = %d, want %q %d (err %v)", class, c.calls(), tc.wantClass, tc.wantCalls, err)
			}
		})
	}
}

func TestAnthropicLiveSmoke(t *testing.T) { runLiveSmoke(t, KindAnthropic) }
