package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

const (
	// defaultLocalBaseURL is local.py's DEFAULT_ENDPOINTS["ollama"]: Ollama's
	// OpenAI-compatible /v1 endpoint.
	defaultLocalBaseURL = "http://localhost:11434/v1"
	// defaultLocalAPIKey is what local.py sends when no key is configured;
	// most self-hosted servers ignore the bearer token.
	defaultLocalAPIKey = "not-needed"
	// defaultGeminiBaseURL is gemini.py's DEFAULT_GEMINI_BASE_URL.
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	// defaultDashScopeBaseURL is qwen.py's DEFAULT_DASHSCOPE_BASE_URL (the
	// China region; a caller passes its region's endpoint).
	defaultDashScopeBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

	compatibleMinOutputTokens = 4096
	compatibleTemperature     = 0.3
	compatibleMaxRetries      = 1
)

// Compatible is llm/providers/local.py's LocalProvider: an OpenAI-compatible
// Chat Completions client, ported from ops
// internal/jobs/investment/categorize/localprovider.go. It serves three
// Kinds, as Python's subclasses did, differing only in default endpoint,
// default model and provider name (gemini.py's GeminiProvider and qwen.py's
// QwenProvider are LocalProvider subclasses):
//
//   - local: any self-hosted OpenAI-compatible server (Ollama's /v1, LM
//     Studio, vLLM); the key is optional.
//   - gemini: Google's OpenAI-compatible endpoint; a key is required.
//   - qwen: Alibaba DashScope's compatible mode; a key is required. qwen.py's
//     Batch API path is not ported.
//
// A schema Format is sent as response_format json_schema (strict) and an object
// Format as json_object; no Format is json_object for Gemini and Qwen and no
// response_format for local. A 400 on a format is a final invalid_request,
// never retried in another format (D6044). The JSON gate applies only under a
// Format: a schema-free answer comes back as written,
// which the explanation callers parse themselves. Temperature 0.3, local.py's
// default, unless Config.Temperature is TemperatureOmit; Request.Seed is sent
// as seed, except to Gemini, which refuses the field: a Gemini request with a
// seed is refused before any call. Config.OutputCap = OutputCapOmit sends no
// max_completion_tokens, so the server's own limit applies.
type Compatible struct {
	cfg    Config
	client *http.Client
}

var _ Provider = (*Compatible)(nil)

// NewCompatible builds the Chat Completions client for cfg.Kind (local,
// gemini or qwen).
func NewCompatible(cfg Config) (*Compatible, error) {
	var defaultBaseURL string
	switch cfg.Kind {
	case KindLocal:
		defaultBaseURL = defaultLocalBaseURL
		if !cfg.APIKey.Configured() {
			cfg.APIKey = NewSecret(defaultLocalAPIKey)
		}
	case KindGemini:
		defaultBaseURL = defaultGeminiBaseURL
	case KindQwen:
		defaultBaseURL = defaultDashScopeBaseURL
	default:
		return nil, fmt.Errorf("llm: kind %q has no OpenAI-compatible client", cfg.Kind)
	}
	if !cfg.APIKey.Configured() {
		return nil, fmt.Errorf("llm: provider %q is not configured: missing an api key", cfg.Kind)
	}
	cfg = cfg.withDefaults(defaultBaseURL)
	cfg.MaxOutputTokens = max(cfg.MaxOutputTokens, compatibleMinOutputTokens)
	return &Compatible{cfg: cfg, client: clientFor(cfg)}, nil
}

func (p *Compatible) Model() string { return p.cfg.Model }
func (p *Compatible) Kind() Kind    { return p.cfg.Kind }

// Close releases the client's idle connections.
func (p *Compatible) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

func (p Compatible) view() providerView {
	return providerView{Kind: p.cfg.Kind, Model: p.cfg.Model, BaseURL: p.cfg.BaseURL}
}

// Format, LogValue and MarshalJSON print the provider without its key.
func (p Compatible) Format(state fmt.State, verb rune) { p.view().format(state, verb) }
func (p Compatible) LogValue() slog.Value              { return p.view().logValue() }
func (p Compatible) MarshalJSON() ([]byte, error)      { return p.view().marshalJSON() }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReplyMessage struct {
	chatMessage
	Refusal string `json:"refusal"`
}

type chatResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *chatJSONSchema `json:"json_schema,omitempty"`
}

type chatJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

type chatRequest struct {
	Model               string              `json:"model"`
	Messages            []chatMessage       `json:"messages"`
	MaxCompletionTokens *int                `json:"max_completion_tokens,omitempty"`
	Temperature         *float64            `json:"temperature,omitempty"`
	ResponseFormat      *chatResponseFormat `json:"response_format,omitempty"`
	Seed                *int64              `json:"seed,omitempty"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatReplyMessage `json:"message"`
		FinishReason string           `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	} `json:"usage"`
}

type chatAnswer struct {
	text    string
	finish  string
	usage   Usage
	refused bool
}

// Complete is local.py's LocalProvider.complete.
func (p *Compatible) Complete(ctx context.Context, request Request) (Result, error) {
	if request.Seed != nil && p.cfg.Kind == KindGemini {
		// Gemini's OpenAI-compatible endpoint answers 400 to any request
		// that carries seed, whatever its value.
		return Result{}, errSeedUnsupported(KindGemini, p.cfg.Model)
	}
	// A schema Format is json_schema (strict) and an object Format is
	// json_object, on every kind. No Format asks Gemini and Qwen for a JSON
	// object, which their APIs document, and asks a local server for
	// nothing: a self-hosted server's modes are unknown (an LM Studio-style
	// server answers 400 to json_object; local.py sent no format). A server
	// that rejects a format with a 400 has refused it: that is a final
	// invalid_request, never a second, billed call with another format.
	var format *chatResponseFormat
	switch {
	case request.Format.schema():
		format = &chatResponseFormat{Type: "json_schema", JSONSchema: &chatJSONSchema{
			Name: request.Format.Name, Schema: request.Format.Schema, Strict: true,
		}}
	case request.Format.object(), p.cfg.Kind != KindLocal:
		format = &chatResponseFormat{Type: "json_object"}
	}
	var maxCompletionTokens *int
	if p.cfg.OutputCap != OutputCapOmit {
		value := max(p.cfg.MaxOutputTokens, request.MaxOutputTokens)
		maxCompletionTokens = &value
	}
	var temperature *float64
	if p.cfg.Temperature != TemperatureOmit {
		value := compatibleTemperature
		temperature = &value
	}
	requestKey := "shared:" + randomID()
	var lastErr *Error
	for attempt := 0; attempt <= compatibleMaxRetries; attempt++ {
		body := chatRequest{
			Model: p.cfg.Model,
			Messages: []chatMessage{
				{Role: "system", Content: request.System},
				{Role: "user", Content: request.Prompt},
			},
			MaxCompletionTokens: maxCompletionTokens,
			Temperature:         temperature,
			ResponseFormat:      format,
			Seed:                request.Seed,
			ReasoningEffort:     request.ReasoningEffort,
		}
		answer, err := p.send(ctx, requestKey, attempt+1, body)
		if err != nil {
			if IsBudgetRefusal(err) || ctx.Err() != nil {
				return Result{}, err
			}
			classified := classify(err, p.cfg.Kind, p.cfg.Model)
			lastErr = classified
			if classified.Retryable() && attempt < compatibleMaxRetries {
				if !sleepFor(ctx, delayFor(classified, attempt)) {
					return Result{}, ctx.Err()
				}
				continue
			}
			return Result{}, classified
		}
		result := Result{Text: answer.text, Model: p.cfg.Model, StopReason: answer.finish, Usage: answer.usage, Refused: answer.refused}
		if request.Format != nil {
			if !answer.refused {
				result.Text, result.Unparsed = gateJSON(answer.text)
			}
			result.FormatPath = FormatPathSchema
			if request.Format.object() {
				result.FormatPath = FormatPathObject
			}
		}
		return result, nil
	}
	if lastErr != nil {
		return Result{}, lastErr
	}
	return Result{Model: p.cfg.Model}, nil
}

func (p *Compatible) send(ctx context.Context, requestKey string, attempt int, body chatRequest) (chatAnswer, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return chatAnswer{}, fmt.Errorf("llm: encode request: %w", err)
	}
	header := http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + p.cfg.APIKey.Reveal()},
	}
	var answer chatAnswer
	err = guarded(ctx, p.cfg.Budget, Attempt{
		IdempotencyKey:  fmt.Sprintf("%s:attempt:%d", requestKey, attempt),
		Provider:        p.cfg.Kind,
		Model:           p.cfg.Model,
		BaseURL:         p.cfg.BaseURL,
		MaxInputBytes:   int64(max(1, len(payload))),
		MaxOutputTokens: int64(valueOr(body.MaxCompletionTokens, 0)),
	}, func(ctx context.Context) (Usage, error) {
		err := exchange(withAttempt(ctx, attempt), p.client, http.MethodPost, p.cfg.BaseURL+"/chat/completions",
			header, bytes.NewReader(payload), p.cfg.Kind, p.cfg.Model, func(raw []byte) error {
				decoded, err := parseChatResponse(raw)
				answer = decoded
				return err
			})
		return answer.usage, err
	})
	return answer, err
}

// errNoChoices is a 2xx body with no choice: no answer at all.
var errNoChoices = fmt.Errorf("llm: the response had no choices")

func parseChatResponse(raw []byte) (chatAnswer, error) {
	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return chatAnswer{}, err
	}
	var answer chatAnswer
	if usage := decoded.Usage; usage != nil {
		// Read before the choices: a reply with no choice can still have
		// been billed, and the budget settles from what was reported.
		answer.usage = usageOf(usage.PromptTokens, usage.CompletionTokens, nil)
	}
	if len(decoded.Choices) == 0 {
		return answer, errNoChoices
	}
	answer.text, answer.finish = decoded.Choices[0].Message.Content, decoded.Choices[0].FinishReason
	answer.refused = decoded.Choices[0].Message.Refusal != ""
	if answer.refused {
		answer.text = ""
	}
	return answer, nil
}

func valueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
