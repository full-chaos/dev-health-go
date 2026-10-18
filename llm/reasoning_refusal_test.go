package llm

import (
	"context"
	"reflect"
	"testing"
)

func chatRefusal(refusal string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": nil, "refusal": refusal}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	}
}

func openAIRefusal() map[string]any {
	return map[string]any{
		"output": []any{map[string]any{"content": []any{map[string]any{"type": "refusal", "refusal": "no"}}}},
		"usage":  map[string]any{"input_tokens": 11, "output_tokens": 7, "input_tokens_details": map[string]any{"cached_tokens": 3}},
	}
}

func TestCompatibleSendsReasoningEffortOnlyWhenSet(t *testing.T) {
	provider, c := newTestCompatible(t, KindLocal,
		step{status: 200, body: chatOK(`{"a": 1}`)}, step{status: 200, body: chatOK(`{"a": 1}`)})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat, ReasoningEffort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if got := c.request(0)["reasoning_effort"]; got != "xhigh" {
		t.Errorf("reasoning_effort = %v, want xhigh", got)
	}
	if _, present := c.request(1)["reasoning_effort"]; present {
		t.Error("an empty ReasoningEffort must not put reasoning_effort in the body")
	}
}

func TestCompatibleRefusalIsAResultWithUsage(t *testing.T) {
	provider, c := newTestCompatible(t, KindLocal, step{status: 200, body: chatRefusal("I cannot help")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatalf("a refusal is not an error: %v", err)
	}
	if !result.Refused || result.Text != "" || result.Unparsed != "" || c.calls() != 1 {
		t.Fatalf("result = %+v calls = %d", result, c.calls())
	}
	if result.Model != "m-local" || result.StopReason != "stop" || result.FormatPath != FormatPathSchema ||
		result.Usage.InputTokens == nil || *result.Usage.InputTokens != 11 || result.Usage.OutputTokens == nil || *result.Usage.OutputTokens != 7 {
		t.Errorf("a refused result keeps its receipt fields: %+v", result)
	}
}

func TestCompatibleEmptyRefusalIsNotRefused(t *testing.T) {
	provider, _ := newTestCompatible(t, KindLocal, step{status: 200, body: chatOK(`{"a": 1}`)})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || result.Refused {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestOpenAIReasoningEffortOverridesTheLowDefault(t *testing.T) {
	provider, c := newTestOpenAI(t,
		step{status: 200, body: openAIOK(`{"a": 1}`)}, step{status: 200, body: openAIOK(`{"a": 1}`)})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat, ReasoningEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if got := c.request(0)["reasoning"]; !reflect.DeepEqual(got, map[string]any{"effort": "high"}) {
		t.Errorf("reasoning = %v, want effort high", got)
	}
	if got := c.request(1)["reasoning"]; !reflect.DeepEqual(got, map[string]any{"effort": "low"}) {
		t.Errorf("default reasoning = %v, want effort low", got)
	}
}

func TestOpenAIRefusalIsAResultWithUsage(t *testing.T) {
	provider, c := newTestOpenAI(t, step{status: 200, body: openAIRefusal()})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil {
		t.Fatalf("a refusal is not an error: %v", err)
	}
	if !result.Refused || result.Text != "" || c.calls() != 1 {
		t.Fatalf("result = %+v calls = %d", result, c.calls())
	}
	if result.Model != "gpt-5-nano" || result.StopReason != "completed" || result.FormatPath != FormatPathSchema ||
		result.Usage.InputTokens == nil || *result.Usage.InputTokens != 11 || result.Usage.CachedInputTokens == nil || *result.Usage.CachedInputTokens != 3 {
		t.Errorf("a refused result keeps its receipt fields: %+v", result)
	}
}

func TestCompleteStrictDoesNotRepairARefusal(t *testing.T) {
	provider := &scriptedProvider{results: []Result{{Refused: true}, {Text: `{"a":1}`}}}
	repairs := 0
	_, outcome, calls, err := CompleteStrict(context.Background(), provider, Request{Prompt: "p"},
		func(string) error { return nil },
		func(string, error) Request { repairs++; return Request{Prompt: "repair"} })
	if err != nil || outcome != OutcomeInvalid || len(calls) != 1 || repairs != 0 || len(provider.requests) != 1 {
		t.Fatalf("outcome=%q calls=%d repairs=%d sent=%d err=%v", outcome, len(calls), repairs, len(provider.requests), err)
	}
	if !calls[0].Refused {
		t.Error("the refused result must be the one reported")
	}
}

func TestOpenAIRefusalAtTheOutputLimitIsNotRetried(t *testing.T) {
	body := openAIRefusal()
	body["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	provider, c := newTestOpenAI(t, step{status: 200, body: body}, step{status: 200, body: openAIOK(`{"a": 1}`)})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
	if err != nil || !result.Refused || result.Text != "" || c.calls() != 1 {
		t.Fatalf("result = %+v err = %v calls = %d, want refused after one attempt", result, err, c.calls())
	}
}
