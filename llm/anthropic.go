package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
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
//     are removed from a copy first (anthropicSchema) and written into the
//     node's description, as the Anthropic SDKs do, so the model sees the
//     limits the caller's own validation still enforces.
//   - The mode is picked once, before the call, from the documented limits
//     (anthropicSchemaFit): structured outputs, or one tool call whose
//     input_schema is the schema. A 400 is an error, never a retry in
//     another mode: a refused request is billed for its input tokens.
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

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	ToolChoice   *anthropicToolChoice   `json:"tool_choice,omitempty"`
}

// anthropicMode is how a request asks for a Format's JSON: structured
// outputs when the schema fits their documented limits, otherwise one tool
// call whose input_schema is the schema (no such limits apply to a
// non-strict tool). The mode is picked once, before the call.
type anthropicMode int

const (
	anthropicStructured anthropicMode = iota
	anthropicForcedTool
)

// anthropicToolName is a valid Messages API tool name for the Format.
func anthropicToolName(format *JSONFormat) string {
	if format != nil && anthropicToolNamePattern.MatchString(format.Name) {
		return format.Name
	}
	return "output"
}

var anthropicToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
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
	if request.Seed != nil {
		return Result{}, errSeedUnsupported(KindAnthropic, p.cfg.Model)
	}
	maxTokens := max(p.cfg.MaxOutputTokens, request.MaxOutputTokens)
	// One mode, picked before any call from the documented limits: a refused
	// schema is billed for its input tokens, so a refusal is an error, never
	// a reason to try another mode.
	mode, reason := anthropicStructured, ""
	if request.Format.schema() {
		if fits, why := anthropicSchemaFit(anthropicSchema(request.Format.Schema)); !fits {
			mode, reason = anthropicForcedTool, why
		}
	}
	requestKey := "shared:" + randomID()
	// retries counts the transient and cut-answer retries (anthropic.py's
	// max_retries=1).
	retries := 0
	for call := 1; ; call++ {
		body := p.messagesBody(request, maxTokens, mode)
		answer, err := p.send(ctx, requestKey, call, body)
		if err != nil {
			if IsBudgetRefusal(err) || ctx.Err() != nil {
				return Result{}, err
			}
			classified := classify(err, KindAnthropic, p.cfg.Model)
			if classified.Retryable() && retries < anthropicMaxRetries {
				if !sleepFor(ctx, delayFor(classified, retries)) {
					return Result{}, ctx.Err()
				}
				retries++
				continue
			}
			return Result{}, classified
		}
		text, unparsed := answer.text, ""
		if answer.stopReason == "refusal" {
			// A refusal can carry text (the model's own words); it is never
			// an answer, so it is the empty completion the caller repairs or
			// falls back from. StopReason says why.
			text = ""
		}
		if request.Format != nil && answer.stopReason != "refusal" {
			text, unparsed = gateJSON(answer.text)
			if text == "" && answer.stopReason == "max_tokens" && retries < anthropicMaxRetries {
				// Doubled up to 16000; a cap already above it is kept.
				maxTokens = max(maxTokens, min(anthropicMaxOutputTokens, maxTokens*2))
				if !sleepFor(ctx, 500*time.Millisecond) {
					return Result{}, ctx.Err()
				}
				retries++
				continue
			}
		}
		result := Result{Text: text, Model: p.cfg.Model, StopReason: answer.stopReason, Usage: answer.usage, Unparsed: unparsed}
		if request.Format != nil {
			result.FormatPath, result.FormatReason = FormatPathSchema, reason
			if mode == anthropicForcedTool {
				result.FormatPath = FormatPathTool
			}
			if request.Format.object() {
				result.FormatPath = FormatPathObject
			}
		}
		return result, nil
	}
}

func (p *Anthropic) messagesBody(request Request, maxTokens int, mode anthropicMode) anthropicRequest {
	body := anthropicRequest{
		Model:     p.cfg.Model,
		MaxTokens: maxTokens,
		System:    request.System,
		Messages:  []anthropicMessage{{Role: "user", Content: request.Prompt}},
	}
	if !request.Format.schema() {
		return body
	}
	switch mode {
	case anthropicStructured:
		body.OutputConfig = &anthropicOutputConfig{Format: &anthropicOutputFormat{
			Type: "json_schema", Schema: anthropicSchema(request.Format.Schema),
		}}
	case anthropicForcedTool:
		name := anthropicToolName(request.Format)
		body.Tools = []anthropicTool{{Name: name, Description: "Return the answer as this tool's input. Give each property at the level the input schema defines it: never move a top-level property inside an object property, and add no property the schema does not list.", InputSchema: copySchema(request.Format.Schema)}}
		body.ToolChoice = &anthropicToolChoice{Type: "tool", Name: name}
		if !anthropicForcesTools(p.cfg.Model) {
			// These models answer 400 to a forced tool_choice; with the one
			// tool and its description they still call it.
			body.ToolChoice = &anthropicToolChoice{Type: "auto"}
		}
	}
	return body
}

// copySchema is a JSON copy of schema, so the caller's map is never shared
// with a request body; a schema that cannot be copied is sent as it is.
func copySchema(schema map[string]any) map[string]any {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var out map[string]any
	if err := decodeKeepingNumbers(encoded, &out); err != nil {
		return schema
	}
	return out
}

// decodeKeepingNumbers decodes one JSON value with numbers kept as
// json.Number, so integer text above 2^53 survives a copy.
func decodeKeepingNumbers(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	return decoder.Decode(target)
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
	var toolInput string
	for _, block := range decoded.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			if toolInput == "" {
				toolInput = string(block.Input)
			}
		}
	}
	answer := anthropicAnswer{text: text.String(), stopReason: decoded.StopReason}
	if toolInput != "" {
		// A forced tool call's input is the answer; any text beside it is
		// the model's preamble.
		answer.text = toolInput
	}
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
	"minLength": {}, "maxLength": {}, "maxItems": {}, "uniqueItems": {}, "minContains": {}, "maxContains": {},
	"minProperties": {}, "maxProperties": {},
}

// anthropicSchemaDataKeywords hold data, not schema: their values are kept
// exactly as given.
var anthropicSchemaDataKeywords = map[string]struct{}{
	"default": {}, "const": {}, "enum": {}, "examples": {},
}

// anthropicSchema is a copy of schema without the keywords Anthropic rejects;
// each dropped bound is worded into its node's description
// (describeDroppedBounds). The copy goes through JSON so every nested map and
// slice is reached whatever Go type built it, and the caller's schema is
// never modified. A schema that cannot be copied is sent as it is: the API
// then answers 400, which Complete returns as an error.
func anthropicSchema(schema map[string]any) map[string]any {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var generic any
	if err := decodeKeepingNumbers(encoded, &generic); err != nil {
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
			if _, data := anthropicSchemaDataKeywords[key]; data {
				out[key] = inner
				continue
			}
			if key == "minItems" {
				if n, ok := schemaNumber(inner); !ok || (n != 0 && n != 1) {
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
		describeDroppedBounds(typed, out)
		enumWithoutTypeList(out)
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

// anthropicBoundPhrases word each bound anthropicSchema drops, in the order
// they are written into a description.
var anthropicBoundPhrases = []struct{ keyword, format string }{
	{"minLength", "at least %s characters"},
	{"maxLength", "at most %s characters"},
	{"minimum", "at least %s"},
	{"maximum", "at most %s"},
	{"exclusiveMinimum", "greater than %s"},
	{"exclusiveMaximum", "less than %s"},
	{"multipleOf", "a multiple of %s"},
	{"minItems", "at least %s items"},
	{"maxItems", "at most %s items"},
	{"uniqueItems", "unique items"},
	{"minContains", "at least %s matching items"},
	{"maxContains", "at most %s matching items"},
	{"minProperties", "at least %s properties"},
	{"maxProperties", "at most %s properties"},
}

// describeDroppedBounds appends the bounds of original that stripped no
// longer carries to stripped's description, as the Anthropic SDKs do: the
// model then sees the limits the caller's own check enforces after the call.
func describeDroppedBounds(original, stripped map[string]any) {
	var limits []string
	for _, bound := range anthropicBoundPhrases {
		value, present := original[bound.keyword]
		if !present {
			continue
		}
		if _, kept := stripped[bound.keyword]; kept {
			continue
		}
		if bound.keyword == "uniqueItems" {
			if value == true {
				limits = append(limits, bound.format)
			}
			continue
		}
		limits = append(limits, fmt.Sprintf(bound.format, boundValue(value)))
	}
	if len(limits) == 0 {
		return
	}
	text := "Limits: " + strings.Join(limits, "; ") + "."
	if existing, ok := stripped["description"].(string); ok && existing != "" {
		text = existing + " " + text
	}
	stripped["description"] = text
}

func boundValue(value any) string {
	if number, ok := value.(json.Number); ok {
		return number.String()
	}
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

// enumWithoutTypeList rewrites an enum under a type list, such as
// {"type": ["string", "null"], "enum": ["high", null]}: structured outputs
// answer 400 to that shape ("Enum value 'high' does not match declared type
// '['string', 'null']'", measured 2026-10-16 on claude-haiku-5-5, with and
// without null in the enum) and accept the enum alone. The enum keeps only
// the values the type list allows, so the accepted values are the same.
func enumWithoutTypeList(schema map[string]any) {
	types, list := schema["type"].([]any)
	values, enum := schema["enum"].([]any)
	if !list || !enum {
		return
	}
	kept := make([]any, 0, len(values))
	for _, value := range values {
		for _, name := range types {
			if jsonTypeAllows(name, value) {
				kept = append(kept, value)
				break
			}
		}
	}
	schema["enum"] = kept
	delete(schema, "type")
}

func jsonTypeAllows(name any, value any) bool {
	switch name {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		if _, ok := value.(json.Number); ok {
			return true
		}
		_, ok := schemaNumber(value)
		return ok
	case "integer":
		return schemaInteger(value)
	}
	return false
}

func schemaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		return number, err == nil
	}
	return 0, false
}

// schemaInteger reports whether value is a whole number. A json.Number is
// judged on its text, exactly and without a size limit: json.Marshal already
// refused invalid number text, so the text is sign, digits, an optional
// fraction and an optional exponent.
func schemaInteger(value any) bool {
	if typed, ok := value.(json.Number); ok {
		return numberTextIsInteger(string(typed))
	}
	number, ok := value.(float64)
	return ok && number == float64(int64(number))
}

func numberTextIsInteger(text string) bool {
	text = strings.TrimPrefix(text, "-")
	exponent := 0
	if at := strings.IndexAny(text, "eE"); at >= 0 {
		parsed, err := strconv.Atoi(text[at+1:])
		if err != nil {
			if text[at+1] == '-' {
				parsed = -1 << 31
			} else {
				parsed = 1 << 31
			}
		}
		exponent = parsed
		text = text[:at]
	}
	whole, fraction, _ := strings.Cut(text, ".")
	dropped := len(fraction) - exponent
	if dropped <= 0 {
		return true
	}
	if strings.Trim(fraction[max(0, len(fraction)-dropped):], "0") != "" {
		return false
	}
	rest := dropped - len(fraction)
	return rest <= 0 || strings.Trim(whole[max(0, len(whole)-rest):], "0") == ""
}
