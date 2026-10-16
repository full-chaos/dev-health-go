package llm

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opt-in live smoke tests: one real request per provider, with a key the
// caller supplies by environment variable NAME. They never run in default CI:
// LLM_LIVE_SMOKE=1 enables them, and an enabled smoke with no key FAILS rather
// than skips, so a smoke that did not happen never reads as one that passed.
// No key value is printed: the key travels as a Secret and the result line
// carries the model, the token counts and the error class only.
//
//	LLM_LIVE_SMOKE=1 OPENAI_API_KEY=... go test ./llm -run LiveSmoke -v
//
// LLM_LIVE_SMOKE_MODEL overrides the model of every smoke; the provider's
// DefaultModel is used otherwise.

// liveSmokeKeys names the environment variable holding each provider's key.
var liveSmokeKeys = map[Kind]string{
	KindOpenAI:    "OPENAI_API_KEY",
	KindAnthropic: "ANTHROPIC_API_KEY",
	KindGemini:    "GEMINI_API_KEY",
	KindQwen:      "DASHSCOPE_API_KEY",
}

func runLiveSmoke(t smokeT, kind Kind) {
	t.Helper()
	if os.Getenv("LLM_LIVE_SMOKE") != "1" {
		t.Skip("live smoke is opt-in: set LLM_LIVE_SMOKE=1 and " + liveSmokeKeys[kind])
		return
	}
	keyName := liveSmokeKeys[kind]
	key := os.Getenv(keyName)
	if key == "" {
		t.Fatalf("LLM_LIVE_SMOKE=1 but %s is unset: the %s smoke cannot run", keyName, kind)
		return
	}
	provider, err := New(Config{Kind: kind, APIKey: NewSecret(key), Model: os.Getenv("LLM_LIVE_SMOKE_MODEL")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = provider.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	format := &JSONFormat{Name: "smoke", Schema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"ok"},
		"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
	}}
	result, err := provider.Complete(ctx, Request{
		System:          "You are a JSON generator. Return a single JSON object only.",
		Prompt:          `Return the JSON object {"ok": true}.`,
		Format:          format,
		MaxOutputTokens: 1024,
	})
	if err != nil {
		class, _ := ClassOf(err)
		t.Fatalf("%s live smoke failed: class=%q err=%v", kind, class, err)
	}
	if result.Text != `{"ok":true}` {
		t.Errorf("%s live smoke text = %q, want the compact JSON {\"ok\":true}", kind, result.Text)
	}
	if result.Usage.InputTokens == nil || result.Usage.OutputTokens == nil {
		t.Errorf("%s live smoke reported no usage: %+v", kind, result.Usage)
	}
	t.Logf("%s live smoke: model=%s stop=%s input=%v output=%v cached=%v text=%s", kind, result.Model, result.StopReason,
		deref(result.Usage.InputTokens), deref(result.Usage.OutputTokens), deref(result.Usage.CachedInputTokens), result.Text)
}

func TestOpenAILiveSmoke(t *testing.T) { runLiveSmoke(t, KindOpenAI) }

// TestLiveSmokeFailsWhenEnabledWithoutAKey proves the smoke cannot pass
// vacuously: enabled with no key, it must fail.
func TestLiveSmokeFailsWhenEnabledWithoutAKey(t *testing.T) {
	t.Setenv("LLM_LIVE_SMOKE", "1")
	for kind, name := range liveSmokeKeys {
		t.Setenv(name, "")
		probe := &smokeProbe{}
		func() {
			defer func() { _ = recover() }()
			runLiveSmoke(probe, kind)
		}()
		if !probe.failed {
			t.Errorf("%s: enabled with no key, the smoke did not fail", kind)
		}
	}
}

// smokeProbe records a Fatalf without stopping the outer test.
type smokeProbe struct{ failed bool }

type smokeFatal struct{}

func (p *smokeProbe) Helper()               {}
func (p *smokeProbe) Skip(...any)           { panic(smokeFatal{}) }
func (p *smokeProbe) Fatalf(string, ...any) { p.failed = true; panic(smokeFatal{}) }
func (p *smokeProbe) Fatal(...any)          { p.failed = true; panic(smokeFatal{}) }
func (p *smokeProbe) Errorf(string, ...any) { p.failed = true }
func (p *smokeProbe) Logf(string, ...any)   {}

// smokeT is the part of *testing.T runLiveSmoke uses.
type smokeT interface {
	Helper()
	Skip(args ...any)
	Fatalf(format string, args ...any)
	Fatal(args ...any)
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
}
