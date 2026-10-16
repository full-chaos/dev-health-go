package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultAnthropicBaseURL has the Anthropic SDK's meaning
	// (anthropic.py passed base_url to AsyncAnthropic): the host root without
	// the /v1 segment, which this client appends.
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	// anthropicAPIVersion is the Messages API version header the SDK sends.
	anthropicAPIVersion = "2023-06-01"

	// anthropicMinOutputTokens is anthropic.py's max_tokens=4096.
	anthropicMinOutputTokens = 4096
	anthropicMaxOutputTokens = 16000
	anthropicMaxRetries      = 1
)

// Anthropic is llm/providers/anthropic.py's AnthropicProvider over the
// Messages API: one call per attempt, call_with_retry(max_retries=1), and
// classify_provider_error(provider="anthropic").
//
// It goes past the Python provider in four places, each to give the strict
// JSON contract the enforcement the OpenAI client has:
//
//   - A Request with a Format is sent with output_config.format (json_schema),
//     Anthropic's constrained decoding. The schema keywords the API rejects
//     are removed from a copy first (anthropicSchema); the caller's own
//     validation still enforces them, as the Anthropic SDKs do client side.
//   - A 400 on a request with output_config (a model or gateway without
//     structured outputs) is retried once without it; the answer still passes
//     the JSON gate.
//   - The text is every "text" content block joined. anthropic.py read
//     content[0].text, which is a thinking block on a current model.
//   - A max_tokens stop with no JSON under a Format is retried once with the
//     cap doubled (at most 16000). A refusal stop is an empty completion,
//     whatever text it carries.
//
// Under a Format the text is compact JSON or "" (the caller repairs). With no
// Format the text is returned as the model wrote it, as anthropic.py did.
type Anthropic struct {
	cfg    Config
	client *http.Client
}

var _ Provider = (*Anthropic)(nil)

// NewAnthropic builds an Anthropic provider. An empty BaseURL is the public
// API, an empty Model is DefaultModel(KindAnthropic). An API key is required.
func NewAnthropic(cfg Config) (*Anthropic, error) {
	cfg.Kind = KindAnthropic
	if !cfg.APIKey.Configured() {
		return nil, fmt.Errorf("llm: provider %q is not configured: missing an api key", KindAnthropic)
	}
	cfg = cfg.withDefaults(defaultAnthropicBaseURL)
	cfg.MaxOutputTokens = max(cfg.MaxOutputTokens, anthropicMinOutputTokens)
	return &Anthropic{cfg: cfg, client: clientFor(cfg)}, nil
}

func (p *Anthropic) Model() string { return p.cfg.Model }
func (p *Anthropic) Kind() Kind    { return KindAnthropic }

// Close releases the client's idle connections.
func (p *Anthropic) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

func (p Anthropic) view() providerView {
	return providerView{Kind: KindAnthropic, Model: p.cfg.Model, BaseURL: p.cfg.BaseURL}
}

// Format, LogValue and MarshalJSON print the provider without its key.
func (p Anthropic) Format(state fmt.State, verb rune) { p.view().format(state, verb) }
func (p Anthropic) LogValue() slog.Value              { return p.view().logValue() }
func (p Anthropic) MarshalJSON() ([]byte, error)      { return p.view().marshalJSON() }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicOutputConfig struct {
	Format *anthropicOutputFormat `json:"format,omitempty"`
}

type anthropicOutputFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      *struct {
		InputTokens              *int `json:"input_tokens"`
		OutputTokens             *int `json:"output_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

type anthropicAnswer struct {
	text       string
	stopReason string
	usage      Usage
}

// Complete is anthropic.py's AnthropicProvider.complete.
func (p *Anthropic) Complete(ctx context.Context, request Request) (Result, error) {
	maxTokens := max(p.cfg.MaxOutputTokens, request.MaxOutputTokens)
	structured := request.Format != nil
	requestKey := "shared:" + randomID()
	var lastErr *Error
	for attempt := 0; attempt <= anthropicMaxRetries; attempt++ {
		body := p.messagesBody(request, maxTokens, structured)
		answer, err := p.send(ctx, requestKey, attempt+1, body)
		if err != nil {
			if IsBudgetRefusal(err) || ctx.Err() != nil {
				return Result{}, err
			}
			if statusCodeOf(err) == http.StatusBadRequest && body.OutputConfig != nil && attempt < anthropicMaxRetries {
				structured = false
				continue
			}
			classified := classify(err, KindAnthropic, p.cfg.Model)
			lastErr = classified
			if classified.Retryable() && attempt < anthropicMaxRetries {
				if !sleepFor(ctx, delayFor(classified, attempt)) {
					return Result{}, ctx.Err()
				}
				continue
			}
			return Result{}, classified
		}
		text := answer.text
		if answer.stopReason == "refusal" {
			// A refusal can carry text (the model's own words); it is never
			// an answer, so it is the empty completion the caller repairs or
			// falls back from. StopReason says why.
			text = ""
		}
		if request.Format != nil && text != "" {
			text = CompactJSON(answer.text)
			if text == "" && answer.stopReason == "max_tokens" && attempt < anthropicMaxRetries {
				// Doubled up to 16000; a cap already above it is kept.
				maxTokens = max(maxTokens, min(anthropicMaxOutputTokens, maxTokens*2))
				if !sleepFor(ctx, 500*time.Millisecond) {
					return Result{}, ctx.Err()
				}
				continue
			}
		}
		return Result{Text: text, Model: p.cfg.Model, StopReason: answer.stopReason, Usage: answer.usage}, nil
	}
	if lastErr != nil {
		return Result{}, lastErr
	}
	return Result{Model: p.cfg.Model}, nil
}

func (p *Anthropic) messagesBody(request Request, maxTokens int, structured bool) anthropicRequest {
	body := anthropicRequest{
		Model:     p.cfg.Model,
		MaxTokens: maxTokens,
		System:    request.System,
		Messages:  []anthropicMessage{{Role: "user", Content: request.Prompt}},
	}
	if structured && request.Format != nil {
		body.OutputConfig = &anthropicOutputConfig{Format: &anthropicOutputFormat{
			Type: "json_schema", Schema: anthropicSchema(request.Format.Schema),
		}}
	}
	return body
}

func (p *Anthropic) send(ctx context.Context, requestKey string, attempt int, body anthropicRequest) (anthropicAnswer, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return anthropicAnswer{}, fmt.Errorf("llm: encode request: %w", err)
	}
	header := http.Header{
		"Content-Type":      {"application/json"},
		"Anthropic-Version": {anthropicAPIVersion},
		"X-Api-Key":         {p.cfg.APIKey.Reveal()},
	}
	var answer anthropicAnswer
	err = guarded(ctx, p.cfg.Budget, Attempt{
		IdempotencyKey:  fmt.Sprintf("%s:attempt:%d", requestKey, attempt),
		Provider:        KindAnthropic,
		Model:           p.cfg.Model,
		BaseURL:         p.cfg.BaseURL,
		MaxInputBytes:   int64(max(1, len(payload))),
		MaxOutputTokens: int64(body.MaxTokens),
	}, func(ctx context.Context) (Usage, error) {
		err := exchange(withAttempt(ctx, attempt), p.client, http.MethodPost, p.cfg.BaseURL+"/v1/messages",
			header, bytes.NewReader(payload), KindAnthropic, p.cfg.Model, func(raw []byte) error {
				decoded, err := parseAnthropicResponse(raw)
				answer = decoded
				return err
			})
		return answer.usage, err
	})
	return answer, err
}

// parseAnthropicResponse reads the text and usage of a Messages API body.
// Input tokens are reported as the OpenAI client reports them: every prompt
// token, cached ones included (Anthropic counts cache reads and writes apart
// from input_tokens), with cache reads as the cached subset.
func parseAnthropicResponse(raw []byte) (anthropicAnswer, error) {
	var decoded anthropicResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return anthropicAnswer{}, err
	}
	var text strings.Builder
	for _, block := range decoded.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	answer := anthropicAnswer{text: text.String(), stopReason: decoded.StopReason}
	if usage := decoded.Usage; usage != nil {
		var input *int
		if usage.InputTokens != nil {
			total := *usage.InputTokens
			for _, extra := range []*int{usage.CacheCreationInputTokens, usage.CacheReadInputTokens} {
				if extra != nil {
					total += *extra
				}
			}
			input = &total
		}
		answer.usage = usageOf(input, usage.OutputTokens, usage.CacheReadInputTokens)
	}
	return answer, nil
}

// anthropicUnsupportedKeywords are the JSON Schema keywords Anthropic's
// structured outputs reject: numeric and string bounds, and array bounds
// other than minItems 0 or 1.
var anthropicUnsupportedKeywords = map[string]struct{}{
	"minimum": {}, "maximum": {}, "exclusiveMinimum": {}, "exclusiveMaximum": {}, "multipleOf": {},
	"minLength": {}, "maxLength": {}, "maxItems": {},
}

// anthropicSchema is a copy of schema without the keywords Anthropic rejects.
// The copy goes through JSON so every nested map and slice is reached
// whatever Go type built it, and the caller's schema is never modified. A
// schema that cannot be copied is sent as it is: the API then answers 400 and
// Complete retries without output_config.
func anthropicSchema(schema map[string]any) map[string]any {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return schema
	}
	out, ok := stripSchema(generic).(map[string]any)
	if !ok {
		return schema
	}
	return out
}

func stripSchema(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			if _, drop := anthropicUnsupportedKeywords[key]; drop {
				continue
			}
			if key == "minItems" {
				if n, ok := inner.(float64); !ok || (n != 0 && n != 1) {
					continue
				}
			}
			if names, ok := inner.(map[string]any); ok && (key == "properties" || key == "$defs" || key == "definitions") {
				// Property names are data, not keywords: a property called
				// "maximum" stays.
				kept := make(map[string]any, len(names))
				for name, property := range names {
					kept[name] = stripSchema(property)
				}
				out[key] = kept
				continue
			}
			out[key] = stripSchema(inner)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = stripSchema(inner)
		}
		return out
	}
	return value
}
