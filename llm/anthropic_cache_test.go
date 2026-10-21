package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func anthropicUsageBody(usage string) string {
	return `{"content":[{"type":"text","text":"{\"a\":1}"}],"stop_reason":"end_turn","usage":` + usage + `}`
}

func TestAnthropicCacheOffSendsAPlainSystemString(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")})
	if _, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if got := c.request(0)["system"]; got != "sys" {
		t.Errorf("system = %#v, want the plain string", got)
	}
	if bytes.Contains(c.bodies[0], []byte("cache_control")) {
		t.Error("a request with no Cache policy carries cache_control")
	}
}

func TestAnthropicCacheSystemMarksOneSystemBlock(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")})
	if _, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "p", Format: testFormat, Cache: CacheSystem}); err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"type": "text", "text": "sys", "cache_control": map[string]any{"type": "ephemeral"}}}
	if got := c.request(0)["system"]; !reflect.DeepEqual(got, want) {
		t.Errorf("system = %#v, want %#v", got, want)
	}
	if bytes.Count(c.bodies[0], []byte("cache_control")) != 1 {
		t.Errorf("want one cache marker, body = %s", c.bodies[0])
	}
}

func TestAnthropicCacheWithNoSystemSendsNoMarker(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat, Cache: CacheSystem}); err != nil {
		t.Fatal(err)
	}
	if _, present := c.request(0)["system"]; present || bytes.Contains(c.bodies[0], []byte("cache_control")) {
		t.Errorf("body = %s", c.bodies[0])
	}
}

func TestAnthropicCacheBodiesAreByteStable(t *testing.T) {
	forcedTool := Request{System: "sys", Prompt: "p", Format: boundedFormat(), Cache: CacheSystem}
	structured := Request{System: "sys", Prompt: "p", Format: testFormat, Cache: CacheSystem}
	for name, request := range map[string]Request{"tool-or-structured-bounded": forcedTool, "structured": structured} {
		t.Run(name, func(t *testing.T) {
			provider, c := newTestAnthropic(t,
				step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")},
				step{status: 200, body: anthropicText(`{"a":1}`, "end_turn")})
			for range 2 {
				if _, err := provider.Complete(context.Background(), request); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(c.bodies[0], c.bodies[1]) {
				t.Errorf("two builds differ:\n%s\n%s", c.bodies[0], c.bodies[1])
			}
		})
	}
}

func TestAnthropicForcedToolKeepsToolsAndCacheMarker(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: `{"content":[{"type":"tool_use","name":"x","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`})
	format := &JSONFormat{Name: "x", Schema: map[string]any{
		"type": "object", "required": []any{"a"},
		"properties": map[string]any{"a": map[string]any{"type": "integer", "minimum": 0}},
		"anyOf":      []any{map[string]any{"required": []any{"a"}}},
	}}
	_, err := provider.Complete(context.Background(), Request{System: "sys", Prompt: "p", Format: format, Cache: CacheSystem})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(c.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["system"]; !ok {
		t.Error("no system in the body")
	}
	if _, ok := body["tools"]; !ok && body["output_config"] == nil {
		t.Errorf("neither tools nor output_config in %s", c.bodies[0])
	}
	if bytes.Count(c.bodies[0], []byte("cache_control")) != 1 {
		t.Errorf("want one cache marker, body = %s", c.bodies[0])
	}
}

func TestAnthropicUsageSplitsCacheCreationAndRead(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		usage                  string
		input, created, read   *int
		wantInput, wantCreated int
		wantRead               int
		createdNil, readNil    bool
	}{
		{name: "write", usage: `{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":5}`, wantInput: 110, wantCreated: 100, wantRead: 0},
		{name: "read", usage: `{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":100,"output_tokens":5}`, wantInput: 110, wantCreated: 0, wantRead: 100},
		{name: "absent", usage: `{"input_tokens":10,"output_tokens":5}`, wantInput: 10, createdNil: true, readNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := newTestAnthropic(t, step{status: 200, body: anthropicUsageBody(tc.usage)})
			result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat})
			if err != nil {
				t.Fatal(err)
			}
			u := result.Usage
			if *u.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", *u.InputTokens, tc.wantInput)
			}
			check := func(label string, got *int, want int, wantNil bool) {
				switch {
				case wantNil && got != nil:
					t.Errorf("%s = %d, want nil", label, *got)
				case !wantNil && (got == nil || *got != want):
					t.Errorf("%s = %v, want %d", label, got, want)
				}
			}
			check("CacheCreationInputTokens", u.CacheCreationInputTokens, tc.wantCreated, tc.createdNil)
			check("CachedInputTokens", u.CachedInputTokens, tc.wantRead, tc.readNil)
		})
	}
}

func TestOpenAIClientsIgnoreCacheAndReportNoCreation(t *testing.T) {
	plain, plainC := newTestOpenAI(t, step{status: 200, body: openAIOK(`{"a":1}`)})
	marked, markedC := newTestOpenAI(t, step{status: 200, body: openAIOK(`{"a":1}`)})
	if _, err := plain.Complete(context.Background(), Request{System: "s", Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	result, err := marked.Complete(context.Background(), Request{System: "s", Prompt: "p", Format: testFormat, Cache: CacheSystem})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plainC.bodies[0], markedC.bodies[0]) {
		t.Errorf("Cache changed the OpenAI body:\n%s\n%s", plainC.bodies[0], markedC.bodies[0])
	}
	if result.Usage.CachedInputTokens == nil || *result.Usage.CachedInputTokens != 3 || result.Usage.CacheCreationInputTokens != nil {
		t.Errorf("usage = %+v, want cached 3 and no creation count", result.Usage)
	}
}

func TestCompatibleClientsIgnoreCache(t *testing.T) {
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		t.Run(string(kind), func(t *testing.T) {
			body := map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": `{"a":1}`}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "prompt_tokens_details": map[string]any{"cached_tokens": 3}},
			}
			plain, plainC := newTestCompatible(t, kind, step{status: 200, body: body})
			marked, markedC := newTestCompatible(t, kind, step{status: 200, body: body})
			if _, err := plain.Complete(context.Background(), Request{System: "s", Prompt: "p", Format: testFormat}); err != nil {
				t.Fatal(err)
			}
			result, err := marked.Complete(context.Background(), Request{System: "s", Prompt: "p", Format: testFormat, Cache: CacheSystem})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(plainC.bodies[0], markedC.bodies[0]) {
				t.Errorf("Cache changed the body:\n%s\n%s", plainC.bodies[0], markedC.bodies[0])
			}
			if result.Usage.CachedInputTokens == nil || *result.Usage.CachedInputTokens != 3 || result.Usage.CacheCreationInputTokens != nil {
				t.Errorf("usage = %+v", result.Usage)
			}
		})
	}
}

func TestAnthropicBudgetSeesCacheMarkerAndCreationCount(t *testing.T) {
	server, _ := scriptedServer(t,
		step{status: 200, body: anthropicUsageBody(`{"input_tokens":10,"cache_creation_input_tokens":100,"output_tokens":5}`)},
		step{status: 200, body: anthropicUsageBody(`{"input_tokens":10,"cache_creation_input_tokens":100,"output_tokens":5}`)})
	budget := &recordingBudget{}
	provider, _ := NewAnthropic(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: budget})
	if _, err := provider.Complete(context.Background(), Request{System: "s", Prompt: "p", Format: testFormat, Cache: CacheSystem}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{System: "s", Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if !budget.attempts[0].CacheWrite || budget.attempts[1].CacheWrite {
		t.Errorf("CacheWrite = %v/%v, want true/false", budget.attempts[0].CacheWrite, budget.attempts[1].CacheWrite)
	}
	if got := budget.usages[0].CacheCreationInputTokens; got == nil || *got != 100 {
		t.Errorf("budget usage creation = %v, want 100", got)
	}
}
