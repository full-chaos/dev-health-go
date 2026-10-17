package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// assertNoKey fails when any printed form of subject shows testKey: every fmt
// verb, slog (text and JSON handlers, as a value and as a field) and
// encoding/json. A Secret field, a Config or a provider printed whole by a
// caller must never show the key.
// reporter is the part of *testing.T assertNoKey uses, so a test can prove
// the check fails without failing itself.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func assertNoKey(t reporter, name string, subject any) {
	t.Helper()
	var forms []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		forms = append(forms, verb+"="+fmt.Sprintf(verb, subject))
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("m", "subject", subject)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("m", "subject", subject)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("m", slog.Any("subject", subject))
	forms = append(forms, "slog="+logs.String())
	if encoded, err := json.Marshal(subject); err == nil {
		forms = append(forms, "json="+string(encoded))
	}
	for _, form := range forms {
		if strings.Contains(form, testKey) || strings.Contains(form, fmt.Sprintf("%x", testKey)) {
			t.Errorf("%s leaks the key: %s", name, form)
		}
	}
}

func TestSecretAndConfigNeverPrintTheKey(t *testing.T) {
	secret := NewSecret(testKey)
	if secret.Reveal() != testKey || !secret.Configured() {
		t.Fatal("Reveal lost the value")
	}
	cfg := Config{Kind: KindOpenAI, APIKey: secret, BaseURL: "https://api.example", HTTPClient: &http.Client{}, Budget: &recordingBudget{}}
	provider, err := NewOpenAI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, subject := range map[string]any{
		"Secret": secret, "*Secret": &secret, "Config": cfg, "*Config": &cfg,
		"*OpenAI": provider, "OpenAI": *provider,
		"[]Secret": []Secret{secret}, "map": map[string]Secret{"k": secret},
	} {
		assertNoKey(t, name, subject)
	}
	if encoded, err := json.Marshal(cfg); err != nil {
		t.Errorf("json.Marshal(Config) failed: %v (a caller logging its config must not crash)", err)
	} else if !strings.Contains(string(encoded), redacted) {
		t.Errorf("json.Marshal(Config) = %s, want the redaction marker in place of the key", encoded)
	}
}

// A caller keeps a provider or a Config in an unexported field of its own
// type. fmt cannot call a method through that field, so it prints the value by
// reflection, down to the Secret: the key must not be reachable that way.
func TestAProviderInAnUnexportedFieldNeverPrintsTheKey(t *testing.T) {
	cfg := Config{APIKey: NewSecret(testKey), BaseURL: "https://api.example", Model: "m"}
	openAI, err := NewOpenAI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := NewAnthropic(cfg)
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]any{
		"holder{Config}":     struct{ c Config }{cfg},
		"holder{Secret}":     struct{ s Secret }{cfg.APIKey},
		"holder{*OpenAI}":    struct{ p Provider }{openAI},
		"holder{OpenAI}":     struct{ p OpenAI }{*openAI},
		"holder{*Anthropic}": struct{ p Provider }{anthropic},
		"holder{Anthropic}":  struct{ p Anthropic }{*anthropic},
	}
	for _, kind := range []Kind{KindLocal, KindGemini, KindQwen} {
		compatibleCfg := cfg
		compatibleCfg.Kind = kind
		compatible, err := NewCompatible(compatibleCfg)
		if err != nil {
			t.Fatal(err)
		}
		subjects["holder{*Compatible}/"+string(kind)] = struct{ p Provider }{compatible}
		subjects["holder{Compatible}/"+string(kind)] = struct{ p Compatible }{*compatible}
	}
	for name, subject := range subjects {
		assertNoKey(t, name, subject)
		assertNoKey(t, "&"+name, &subject)
	}
}

// TestPlantedLeakIsCaught proves assertNoKey can fail: a struct holding the key
// as a plain string must be reported. Without this, a checker that never
// matched anything would pass every test above.
func TestPlantedLeakIsCaught(t *testing.T) {
	planted := struct{ APIKey string }{APIKey: testKey}
	probe := &recordingReporter{}
	assertNoKey(probe, "planted", planted)
	if probe.failures == 0 {
		t.Fatal("assertNoKey did not catch a struct that prints the key")
	}
	clean := &recordingReporter{}
	assertNoKey(clean, "secret", NewSecret(testKey))
	if clean.failures != 0 {
		t.Fatalf("assertNoKey reported a Secret %d times", clean.failures)
	}
}

type recordingReporter struct{ failures int }

func (r *recordingReporter) Helper()               {}
func (r *recordingReporter) Errorf(string, ...any) { r.failures++ }

// recordingBudget admits every attempt, records what it was shown, and
// settles from the reported usage.
type recordingBudget struct {
	attempts []Attempt
	usages   []Usage
	refuse   error
	settle   error
}

func (b *recordingBudget) Run(ctx context.Context, attempt Attempt, invoke func(context.Context) (Usage, error)) error {
	b.attempts = append(b.attempts, attempt)
	if b.refuse != nil {
		return b.refuse
	}
	usage, err := invoke(ctx)
	b.usages = append(b.usages, usage)
	if err != nil {
		return err
	}
	return b.settle
}

func TestBudgetSeesEveryAttemptAndNeverTheKey(t *testing.T) {
	server, c := scriptedServer(t, step{status: 503, body: `{}`}, step{status: 200, body: openAIOK(`{"a":1}`)})
	budget := &recordingBudget{}
	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Model: "gpt-5-mini", Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if c.calls() != 2 || len(budget.attempts) != 2 {
		t.Fatalf("calls=%d attempts=%d, want one admission per HTTP attempt", c.calls(), len(budget.attempts))
	}
	first, second := budget.attempts[0], budget.attempts[1]
	if first.IdempotencyKey == second.IdempotencyKey || !strings.HasSuffix(first.IdempotencyKey, ":attempt:1") || !strings.HasSuffix(second.IdempotencyKey, ":attempt:2") {
		t.Errorf("keys = %q, %q", first.IdempotencyKey, second.IdempotencyKey)
	}
	payload, _ := json.Marshal(provider.responsesBody(Request{Prompt: "p", Format: testFormat}, 2048))
	if first.Provider != KindOpenAI || first.Model != "gpt-5-mini" || first.BaseURL != server.URL || first.MaxOutputTokens != 2048 || first.MaxInputBytes != int64(len(payload)) {
		t.Errorf("attempt = %+v", first)
	}
	if usage := budget.usages[1]; usage.InputTokens == nil || *usage.InputTokens != 11 || *usage.CachedInputTokens != 3 {
		t.Errorf("settled usage = %+v", usage)
	}
	for i, attempt := range budget.attempts {
		assertNoKey(t, fmt.Sprintf("Attempt[%d]", i), attempt)
	}
}

func TestBudgetRefusalIsFinalAndNotSent(t *testing.T) {
	refusal := errors.New("budget exhausted")
	server, c := scriptedServer(t, step{status: 200, body: openAIOK(`{}`)})
	budget := &recordingBudget{refuse: refusal}
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: budget})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, refusal) || !IsBudgetRefusal(err) {
		t.Fatalf("err = %v, want the budget's own refusal", err)
	}
	if _, isProvider := ClassOf(err); isProvider {
		t.Error("a budget refusal was reported as a provider failure")
	}
	if c.calls() != 0 || len(budget.attempts) != 1 {
		t.Fatalf("calls=%d admissions=%d, want 0 and 1", c.calls(), len(budget.attempts))
	}
}

func TestBudgetSettleFailureIsFinal(t *testing.T) {
	settle := errors.New("settle failed")
	server, c := scriptedServer(t, step{status: 200, body: openAIOK(`{}`)})
	budget := &recordingBudget{settle: settle}
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: budget})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if !IsBudgetRefusal(err) || !errors.Is(err, settle) || c.calls() != 1 {
		t.Fatalf("err = %v calls = %d, want the settle failure, final after one sent call", err, c.calls())
	}
}
