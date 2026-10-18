package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	defaultOpenAIBaseURL = "https://api.openai.com/v1"

	openAIMinOutputTokens = 2048
	openAIMaxOutputTokens = 8192
	openAIMaxRetries      = 1
)

// OpenAI is llm/providers/openai.py's OpenAIGPT5Provider over the Responses
// API, ported from ops internal/jobs/investment/categorize/openaiprovider.go
// with the same behaviour: strict json_schema output (json_object with no
// schema), reasoning effort low, verbosity low, no temperature (GPT-5 models
// take none), an output floor of 2048 combined with the request's floor by
// max(), one retry of a transient failure, and one retry with the output cap
// doubled (to at most 8192) when the answer was cut at max_output_tokens.
type OpenAI struct {
	cfg    Config
	client *http.Client
}

var _ Provider = (*OpenAI)(nil)

// NewOpenAI builds an OpenAI provider. An empty BaseURL is the public API, an
// empty Model is DefaultModel(KindOpenAI). An API key is required.
func NewOpenAI(cfg Config) (*OpenAI, error) {
	cfg.Kind = KindOpenAI
	if !cfg.APIKey.Configured() {
		return nil, fmt.Errorf("llm: provider %q is not configured: missing an api key", KindOpenAI)
	}
	cfg = cfg.withDefaults(defaultOpenAIBaseURL)
	cfg.MaxOutputTokens = max(cfg.MaxOutputTokens, openAIMinOutputTokens)
	return &OpenAI{cfg: cfg, client: clientFor(cfg)}, nil
}

func (p *OpenAI) Model() string { return p.cfg.Model }

func (p OpenAI) view() providerView {
	return providerView{Kind: KindOpenAI, Model: p.cfg.Model, BaseURL: p.cfg.BaseURL}
}

// Format, LogValue and MarshalJSON print the provider without its key.
func (p OpenAI) Format(state fmt.State, verb rune) { p.view().format(state, verb) }
func (p OpenAI) LogValue() slog.Value              { return p.view().logValue() }
func (p OpenAI) MarshalJSON() ([]byte, error)      { return p.view().marshalJSON() }
func (p *OpenAI) Kind() Kind                       { return KindOpenAI }

// Close releases the client's idle connections.
func (p *OpenAI) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

type openAIResponsesRequest struct {
	Model           string             `json:"model"`
	Instructions    string             `json:"instructions"`
	Input           string             `json:"input"`
	Text            openAIResponseText `json:"text"`
	Reasoning       openAIReasoning    `json:"reasoning"`
	MaxOutputTokens int                `json:"max_output_tokens"`
}

type openAIResponseText struct {
	Format    openAITextFormat `json:"format"`
	Verbosity string           `json:"verbosity"`
}

type openAITextFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Strict bool           `json:"strict,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
}

type openAIReasoning struct {
	Effort string `json:"effort"`
}

type openAIResponsesResponse struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		InputTokens        *int `json:"input_tokens"`
		OutputTokens       *int `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// openAIAnswer is one decoded Responses API body.
type openAIAnswer struct {
	text    string
	reason  string
	usage   Usage
	refused bool
}

// Complete is openai.py's OpenAIGPT5Provider.complete.
func (p *OpenAI) Complete(ctx context.Context, request Request) (Result, error) {
	if request.Seed != nil {
		return Result{}, errSeedUnsupported(KindOpenAI, p.cfg.Model)
	}
	maxTokens := max(p.cfg.MaxOutputTokens, request.MaxOutputTokens)
	requestKey := "shared:" + randomID()
	var lastErr *Error
	for attempt := 0; attempt <= openAIMaxRetries; attempt++ {
		body := p.responsesBody(request, maxTokens)
		answer, err := p.send(ctx, requestKey, attempt+1, body)
		if err != nil {
			if IsBudgetRefusal(err) || ctx.Err() != nil {
				return Result{}, err
			}
			classified := classify(err, KindOpenAI, p.cfg.Model)
			lastErr = classified
			if classified.Retryable() && attempt < openAIMaxRetries {
				if !sleepFor(ctx, delayFor(classified, attempt)) {
					return Result{}, ctx.Err()
				}
				continue
			}
			return Result{}, classified
		}
		text, unparsed := gateJSON(answer.text)
		if answer.refused {
			text, unparsed = "", ""
		}
		if request.Format == nil {
			unparsed = ""
		}
		if text == "" && answer.reason == "max_output_tokens" && attempt < openAIMaxRetries {
			// Doubled up to 8192; a cap already above 8192 is kept, never
			// lowered (the ops port's min() lowered it).
			maxTokens = max(maxTokens, min(openAIMaxOutputTokens, maxTokens*2))
			if !sleepFor(ctx, 500*time.Millisecond) {
				return Result{}, ctx.Err()
			}
			continue
		}
		// An answer that is not JSON is an empty completion, not an error:
		// the caller's repair handles it (openai.py's own contract).
		result := Result{Text: text, Model: p.cfg.Model, StopReason: answer.reason, Usage: answer.usage, Unparsed: unparsed, Refused: answer.refused}
		if request.Format != nil {
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

// responsesBody is the one Responses API body of this provider: the
// synchronous call and every Batch API line are built here.
func (p *OpenAI) responsesBody(request Request, maxTokens int) openAIResponsesRequest {
	format := openAITextFormat{Type: "json_object"}
	if request.Format.schema() {
		format = openAITextFormat{Type: "json_schema", Name: request.Format.Name, Strict: true, Schema: request.Format.Schema}
	}
	effort := "low"
	if request.ReasoningEffort != "" {
		effort = request.ReasoningEffort
	}
	return openAIResponsesRequest{
		Model:           p.cfg.Model,
		Instructions:    request.System,
		Input:           request.Prompt,
		Text:            openAIResponseText{Format: format, Verbosity: "low"},
		Reasoning:       openAIReasoning{Effort: effort},
		MaxOutputTokens: maxTokens,
	}
}

func (p *OpenAI) headers() http.Header {
	return http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + p.cfg.APIKey.Reveal()},
	}
}

// send runs one attempt, under the budget when there is one.
func (p *OpenAI) send(ctx context.Context, requestKey string, attempt int, body openAIResponsesRequest) (openAIAnswer, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return openAIAnswer{}, fmt.Errorf("llm: encode request: %w", err)
	}
	var answer openAIAnswer
	err = guarded(ctx, p.cfg.Budget, Attempt{
		IdempotencyKey:  fmt.Sprintf("%s:attempt:%d", requestKey, attempt),
		Provider:        KindOpenAI,
		Model:           p.cfg.Model,
		BaseURL:         p.cfg.BaseURL,
		MaxInputBytes:   int64(max(1, len(payload))),
		MaxOutputTokens: int64(body.MaxOutputTokens),
	}, func(ctx context.Context) (Usage, error) {
		err := exchange(withAttempt(ctx, attempt), p.client, http.MethodPost, p.cfg.BaseURL+"/responses",
			p.headers(), bytes.NewReader(payload), KindOpenAI, p.cfg.Model, func(raw []byte) error {
				decoded, err := parseOpenAIResponse(raw)
				answer = decoded
				return err
			})
		return answer.usage, err
	})
	return answer, err
}

// parseOpenAIResponse is the one parser of a Responses API body: output_text,
// else every output_text/text chunk of output[].content[] joined. A refusal
// chunk marks the answer refused only when no text came with it: a reply that
// has text keeps being read as an answer, as the ops port reads it.
func parseOpenAIResponse(raw []byte) (openAIAnswer, error) {
	var decoded openAIResponsesResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return openAIAnswer{}, err
	}
	text := decoded.OutputText
	if text == "" {
		var parts bytes.Buffer
		for _, item := range decoded.Output {
			for _, chunk := range item.Content {
				if chunk.Type == "output_text" || chunk.Type == "text" {
					parts.WriteString(chunk.Text)
				}
			}
		}
		text = parts.String()
	}
	answer := openAIAnswer{text: text, reason: "completed"}
	for _, item := range decoded.Output {
		for _, chunk := range item.Content {
			if chunk.Type == "refusal" && text == "" {
				answer.refused = true
			}
		}
	}
	if decoded.IncompleteDetails != nil && decoded.IncompleteDetails.Reason != "" {
		answer.reason = decoded.IncompleteDetails.Reason
	}
	if usage := decoded.Usage; usage != nil {
		var cached *int
		if usage.InputTokensDetails != nil {
			cached = usage.InputTokensDetails.CachedTokens
		}
		answer.usage = usageOf(usage.InputTokens, usage.OutputTokens, cached)
	}
	return answer, nil
}

func randomID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
