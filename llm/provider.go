package llm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Kind names a provider API. The values are the provider names the Python
// providers and the ops/acr settings already use.
type Kind string

const (
	KindOpenAI    Kind = "openai"
	KindAnthropic Kind = "anthropic"
)

// defaultModels is the model each Kind uses when Config.Model is empty. Every
// entry was checked against the provider's public model list (see the pull
// request that set it); a caller that resolves a model itself passes it.
var defaultModels = map[Kind]string{
	// https://developers.openai.com/api/docs/models/all, 2026-10-16: listed,
	// not deprecated. base.py's DEFAULT_MODEL_BY_PROVIDER["openai"].
	KindOpenAI: "gpt-5-mini",
	// https://platform.claude.com/docs/en/models/overview, 2026-10-16:
	// current, retirement not sooner than 2027-10-07. base.py's
	// claude-3-haiku-20240307 was retired on 2026-04-19; this is the same
	// low-cost tier.
	KindAnthropic: "claude-haiku-5-5",
}

// DefaultModel is the model kind uses when none is named, "" for an unknown
// kind.
func DefaultModel(kind Kind) string { return defaultModels[kind] }

// Kinds is every Kind New can build, sorted.
func Kinds() []Kind { return []Kind{KindAnthropic, KindOpenAI} }

// JSONFormat asks for strict JSON matching Schema. Name is the schema's name
// on the wire where the API has one.
type JSONFormat struct {
	Name   string
	Schema map[string]any
}

// Request is one completion. A nil Format asks for a JSON object with no
// schema (OpenAI's json_object), the Python providers' schema-free branch.
type Request struct {
	System string
	Prompt string
	Format *JSONFormat
	// MaxOutputTokens is a floor combined with the provider's own floor by
	// max(): it never lowers the provider's minimum.
	MaxOutputTokens int
}

// Usage is what the provider reported. A nil count was not reported; it is
// unknown, never zero.
type Usage struct {
	InputTokens       *int
	OutputTokens      *int
	CachedInputTokens *int
}

// Result is one completion. Under a Format, Text is compact JSON or "" (the
// reply was not JSON: repair or fall back).
type Result struct {
	Text  string
	Model string
	// StopReason is the provider's own completion status ("completed",
	// "max_output_tokens", ...), for logs.
	StopReason string
	Usage      Usage
}

// Provider is one configured client of one provider API.
type Provider interface {
	Complete(ctx context.Context, request Request) (Result, error)
	// Model is the model every request carries, known before any call.
	Model() string
	Kind() Kind
	// Close releases idle connections.
	Close() error
}

// Config builds a Provider. Credentials come in here and nowhere else: the
// package reads no environment variable and stores no key.
type Config struct {
	Kind   Kind
	APIKey Secret
	// BaseURL is the API root ("https://api.openai.com/v1"); "" is the
	// provider's public endpoint.
	BaseURL string
	// Model; "" is DefaultModel(Kind).
	Model string
	// MaxOutputTokens raises the provider's own output floor.
	MaxOutputTokens int
	// HTTPClient sends every request. nil is a hardened client: no redirect,
	// no proxy, 60 s timeout. This package does not check the addresses it
	// dials: a caller dialling a customer-supplied BaseURL must pass a client
	// whose transport refuses internal addresses at dial time (ops: the
	// SSRF-guarded transport). Whatever client is passed, redirects are
	// refused and a net/http Transport's proxy is dropped; a custom
	// RoundTripper is trusted and used as it is.
	HTTPClient *http.Client `json:"-"`
	// Budget, when set, admits and settles every HTTP attempt.
	Budget Budget `json:"-"`
}

// New builds the Provider for cfg.Kind.
func New(cfg Config) (Provider, error) {
	switch cfg.Kind {
	case KindOpenAI:
		return NewOpenAI(cfg)
	case KindAnthropic:
		return NewAnthropic(cfg)
	}
	return nil, fmt.Errorf("llm: unknown provider kind %q", cfg.Kind)
}

func (c Config) withDefaults(defaultBaseURL string) Config {
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = defaultBaseURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.Model == "" {
		c.Model = DefaultModel(c.Kind)
	}
	return c
}
