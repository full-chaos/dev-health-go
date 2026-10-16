package llm

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrorClass is the closed set of failure classes, llm/errors.py's exception
// hierarchy as one discriminator. A caller switches on it; it never parses
// Error().
type ErrorClass string

const (
	ClassGeneric        ErrorClass = "generic"
	ClassAuth           ErrorClass = "auth"
	ClassModelNotFound  ErrorClass = "model_not_found"
	ClassInvalidRequest ErrorClass = "invalid_request"
	ClassRateLimit      ErrorClass = "rate_limit"
	ClassContextLength  ErrorClass = "context_length"
	ClassServer         ErrorClass = "server"
	ClassTimeout        ErrorClass = "timeout"
	ClassTransport      ErrorClass = "transport"
	// ClassOutput is a reply the provider sent that holds no usable answer.
	ClassOutput ErrorClass = "output"
	// ClassBatchTimeout is a provider batch that did not finish in time.
	ClassBatchTimeout ErrorClass = "batch_timeout"
)

// retryableClasses is errors.py's is_retryable.
var retryableClasses = map[ErrorClass]struct{}{
	ClassRateLimit: {}, ClassServer: {}, ClassTimeout: {}, ClassTransport: {}, ClassOutput: {},
}

// Error is every provider failure this package returns.
type Error struct {
	Class    ErrorClass
	Provider Kind
	Model    string
	// RetryAfter is the provider's Retry-After for a rate limit, capped at
	// 60 s; zero when absent.
	RetryAfter time.Duration
	message    string
	cause      error
}

// NewError builds an Error a caller reports in this package's terms (a batch
// line with no result, a batch that timed out).
func NewError(class ErrorClass, provider Kind, model, message string) *Error {
	return &Error{Class: class, Provider: provider, Model: model, message: message}
}

func (e Error) Error() string {
	parts := []string{sanitizeMessage(e.message)}
	if e.Provider != "" {
		parts = append(parts, "provider="+string(e.Provider))
	}
	if e.Model != "" {
		parts = append(parts, "model="+e.Model)
	}
	if e.cause != nil {
		parts = append(parts, fmt.Sprintf("cause_type=%T", e.cause))
	}
	return strings.Join(parts, " | ")
}

// Format prints Error() for every verb: the struct's own fields hold the
// unsanitized message, which %#v would otherwise print.
func (e Error) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(e.Error())) }

// GoString is Error(), for the same reason.
func (e Error) GoString() string { return e.Error() }

// Unwrap returns the cause without any provider text: a status error keeps
// its status code only, a transport error its operation and class.
func (e *Error) Unwrap() error { return e.cause }

// Retryable is errors.py's is_retryable.
func (e *Error) Retryable() bool {
	_, ok := retryableClasses[e.Class]
	return ok
}

// ClassOf reports the class of err. ok is false when err is not this
// package's *Error (a budget refusal, a context cancellation): the caller
// must not treat that as a provider failure.
func ClassOf(err error) (ErrorClass, bool) {
	var typed *Error
	if IsBudgetRefusal(err) || !errors.As(err, &typed) {
		return "", false
	}
	return typed.Class, true
}

// statusError is a non-2xx response. Error() carries the status only; the body
// is provider text, read by the classifier alone.
type statusError struct {
	statusCode int
	header     http.Header
	body       string
}

func (e *statusError) Error() string { return fmt.Sprintf("http %d", e.statusCode) }

// transportError is a request that got no response at all.
type transportError struct{ cause error }

func (e *transportError) Error() string { return e.cause.Error() }
func (e *transportError) Unwrap() error { return e.cause }

func statusCodeOf(err error) int {
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		return statusErr.statusCode
	}
	return 0
}

func headerOf(err error) http.Header {
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		return statusErr.header
	}
	return nil
}

func responseBodyOf(err error) string {
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		return statusErr.body
	}
	return ""
}

// classify is errors.py's classify_provider_error: the status code first where
// it decides, then the provider's error vocabulary in the body.
func classify(err error, provider Kind, model string) *Error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		if already.Provider == "" {
			filled := *already
			filled.Provider, filled.Model = provider, model
			return &filled
		}
		return already
	}
	statusCode := statusCodeOf(err)
	msgLower := strings.ToLower(err.Error() + " " + responseBodyOf(err))
	cause := err
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		// The unwrapped cause keeps the status only: a provider can echo the
		// key in a header or in its body, in a shape no pattern recognises.
		cause = &statusError{statusCode: statusErr.statusCode}
	}
	build := func(class ErrorClass, message string) *Error {
		return &Error{Class: class, Provider: provider, Model: model, message: message, cause: cause}
	}

	// A request that got no response is a transport failure whatever the
	// network error's wording (ops categorize/errors.go, codex round 3 #2178).
	var transportErr *transportError
	if errors.As(err, &transportErr) {
		// errors.py classed a timeout by its exception type before any
		// connection error: a client or dial timeout is ClassTimeout.
		if isTimeout(transportErr.cause) {
			return build(ClassTimeout, "LLM provider request timed out.")
		}
		return build(ClassTransport, "LLM provider endpoint could not be reached.")
	}

	// The checks and their order are errors.py's classify_provider_error, as
	// the ops port has them: msgLower carries "http <status>" plus the body,
	// where Python matched the SDK's "Error code: <status> - <body>" text.
	switch {
	case containsAny(msgLower, "insufficient_quota", "current quota"):
		return build(ClassAuth, "LLM quota exhausted. Check provider billing/quota or use a different API key.")
	case containsAny(msgLower, "model_not_found", "model not found", "model does not exist"):
		return build(ClassModelNotFound, fmt.Sprintf("LLM model not found for provider %q using configured model %q.", provider, model))
	case containsAny(msgLower, "401", "invalid_api_key", "authentication", "unauthorized", "missing api key", "api key missing", "api_key is required"):
		return build(ClassAuth, "Invalid or missing LLM API key.")
	case statusCode == http.StatusTooManyRequests || containsAny(msgLower, "429", "rate_limit", "rate limit", "too many requests"):
		e := build(ClassRateLimit, "Transient LLM rate limit from provider.")
		e.RetryAfter = retryAfterFromHeader(headerOf(err))
		return e
	case containsAny(msgLower, "context_length_exceeded", "maximum context length", "too many tokens", "input too long", "reduce your prompt", "prompt is too long"):
		return build(ClassContextLength, "LLM prompt exceeds the model context window.")
	case statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity ||
		containsAny(msgLower, "unsupported parameter", "unsupported value", "invalid parameter", "invalid_request_error", "invalid request"):
		return build(ClassInvalidRequest, "LLM provider rejected an unsupported request parameter.")
	case containsAny(msgLower, "timeout", "timed out"):
		return build(ClassTimeout, "LLM provider request timed out.")
	case containsAny(msgLower, "connection error", "connection refused", "network is unreachable", "name or service not known", "dns error", "tls error"):
		return build(ClassTransport, "LLM provider endpoint could not be reached.")
	case statusCode >= 500 || containsAny(msgLower, "500", "502", "503", "504", "server error", "internal error"):
		return build(ClassServer, "Transient LLM provider server error.")
	// Past errors.py: statuses its keyword checks left generic, classed by
	// what the status means (Anthropic's billing_error, permission_error,
	// not_found_error, timeout_error, request_too_large).
	case statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden:
		return build(ClassAuth, "LLM provider refused the API key for billing or permission.")
	case statusCode == http.StatusNotFound:
		return build(ClassModelNotFound, fmt.Sprintf("LLM model or endpoint not found for provider %q using configured model %q.", provider, model))
	case statusCode == http.StatusRequestTimeout:
		return build(ClassTimeout, "LLM provider request timed out.")
	case statusCode == http.StatusRequestEntityTooLarge:
		return build(ClassInvalidRequest, "LLM provider rejected the request as too large.")
	}
	return build(ClassGeneric, err.Error())
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

const (
	baseRetryDelay    = 500 * time.Millisecond
	maxRetryDelay     = 30 * time.Second
	maxRetryAfterSecs = 60.0
)

// retryDelay is errors.py's retry_delay: exponential, 0-indexed, capped.
func retryDelay(attempt int) time.Duration {
	delay := baseRetryDelay << uint(attempt)
	if delay > maxRetryDelay || delay <= 0 {
		return maxRetryDelay
	}
	return delay
}

// delayFor honours a rate limit's own Retry-After over the backoff.
func delayFor(err *Error, attempt int) time.Duration {
	if err.Class == ClassRateLimit && err.RetryAfter > 0 {
		return err.RetryAfter
	}
	return retryDelay(attempt)
}

// retryAfterFromHeader reads Retry-After (seconds or an HTTP date), clamped to
// [0, 60 s] so a hostile value cannot pin a worker.
func retryAfterFromHeader(header http.Header) time.Duration {
	if header == nil {
		return 0
	}
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		when, dateErr := http.ParseTime(raw)
		if dateErr != nil {
			return 0
		}
		seconds = time.Until(when).Seconds()
	}
	seconds = max(0, min(seconds, maxRetryAfterSecs))
	return time.Duration(seconds * float64(time.Second))
}

// secretPatterns redact credentials from text that can reach a log: an
// Authorization value as one unit, URI userinfo, a credential-word label, a
// known key prefix, and any long opaque run (over-redaction is the safe side).
// Ported from ops categorize/errors.go.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization[\s:=]+)['"]?[A-Za-z0-9+/_.~-]+=*(?:\s+[A-Za-z0-9+/_.~-]+=*)?`),
	regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s@]+@`),
	regexp.MustCompile(`(?i)((?:secret|token|key|password|passwd|credential|authorization|signature)[\s:=]+)['"]?[^'"\s,;}]+`),
	regexp.MustCompile(`\b(?:sk-|sk_live_|sk_test_|ghp_|gho_|github_pat_|xox[a-zA-Z0-9]+-|AKIA|AIza)[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`\b[A-Za-z0-9+/_-]{24,}={0,2}\b`),
}

// sanitizeMessage is errors.py's _sanitize_message: one line, credentials
// redacted, at most 500 bytes.
func sanitizeMessage(message string) string {
	if message == "" {
		message = "LLM provider error"
	}
	text := strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ").Replace(message))
	for _, pattern := range secretPatterns {
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			if sub := pattern.FindStringSubmatch(match); len(sub) > 1 && sub[1] != "" {
				return sub[1] + "<redacted>"
			}
			return "<redacted>"
		})
	}
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}
