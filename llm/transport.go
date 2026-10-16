package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// hardenedTimeout is llm/providers/_http.py's timeout=60.0.
const hardenedTimeout = 60 * time.Second

// newHardenedClient is _http.py's make_hardened_httpx_client: no redirects (a
// 30x on a POST drops or rewrites the body, and could carry the key to another
// host), no environment proxy, a bounded timeout.
func newHardenedClient() *http.Client {
	return &http.Client{
		Timeout:       hardenedTimeout,
		CheckRedirect: refuseRedirect,
		Transport:     &http.Transport{Proxy: nil},
	}
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// noRedirects is client with redirects refused, whatever the caller set.
func noRedirects(client *http.Client) *http.Client {
	copied := *client
	copied.CheckRedirect = refuseRedirect
	return &copied
}

// clientFor is the client a provider sends with: the hardened one, or the
// caller's with redirects refused and, for a net/http transport (nil is
// http.DefaultTransport, whose proxy is ProxyFromEnvironment), a clone with no
// proxy -- a proxy would receive the API key. The caller's own client and
// transport are not modified. A caller that must reach the provider through a
// proxy wraps its own http.RoundTripper, which is used as it is.
func clientFor(cfg Config) *http.Client {
	if cfg.HTTPClient == nil {
		return newHardenedClient()
	}
	client := noRedirects(cfg.HTTPClient)
	switch transport := client.Transport.(type) {
	case nil:
		direct := http.DefaultTransport.(*http.Transport).Clone()
		direct.Proxy = nil
		client.Transport = direct
	case *http.Transport:
		direct := transport.Clone()
		direct.Proxy = nil
		client.Transport = direct
	}
	return client
}

// Telemetry names, identical to ops categorize/llmspan.go so dashboards built
// on the ops spans keep working when ops adopts this package.
const (
	spanTracer = "dev_health/llm"
	spanName   = "dev_health.llm.request"

	attrProvider = "llm.provider"
	attrModel    = "llm.model"
	attrRole     = "llm.role"
	attrAttempt  = "llm.attempt"
	attrStatus   = "http.response.status_code"
	attrClass    = "error.class"
	attrBatchOp  = "llm.batch.operation"

	spanMaxModelLen = 128
)

// Role says why a request is made ("served", "shadow"); absent, the attribute
// is left off.
type Role string

const (
	RoleServed Role = "served"
	RoleShadow Role = "shadow"
)

type (
	roleKey    struct{}
	attemptKey struct{}
	batchOpKey struct{}
)

// WithRole marks the requests made under ctx.
func WithRole(ctx context.Context, role Role) context.Context {
	return context.WithValue(ctx, roleKey{}, role)
}

func withAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, attemptKey{}, attempt)
}

func withBatchOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, batchOpKey{}, operation)
}

// spanClass maps one finished exchange to error.class; "" is success.
func spanClass(status int, transportErr, ctxErr error) string {
	if transportErr != nil {
		switch {
		case ctxErr != nil:
			return "canceled"
		case isTimeout(transportErr):
			return "timeout"
		case strings.Contains(strings.ToLower(transportErr.Error()), "connection refused"):
			return "refused"
		}
		return "other"
	}
	switch {
	case status >= 200 && status < 300:
		return ""
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status == http.StatusUnauthorized, status == http.StatusPaymentRequired, status == http.StatusForbidden:
		return "auth"
	case status == http.StatusNotFound:
		return "model_not_found"
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return "invalid_request"
	case status >= 500:
		return "server"
	}
	return "other"
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
}

// do sends req inside one span. The span ends when the caller closes the
// body, so a failure while reading it belongs to the span. A transport error
// comes back as *transportError whose text names the operation and class
// only: *url.Error's text quotes the URL, which can carry a key.
func do(client *http.Client, req *http.Request, provider Kind, model string) (*http.Response, error) {
	ctx := req.Context()
	if len(model) > spanMaxModelLen {
		model = model[:spanMaxModelLen]
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrProvider, string(provider)),
		attribute.String(attrModel, model),
	}
	if role, ok := ctx.Value(roleKey{}).(Role); ok && (role == RoleServed || role == RoleShadow) {
		attrs = append(attrs, attribute.String(attrRole, string(role)))
	}
	if attempt, ok := ctx.Value(attemptKey{}).(int); ok && attempt > 0 {
		attrs = append(attrs, attribute.Int(attrAttempt, attempt))
	}
	if operation, ok := ctx.Value(batchOpKey{}).(string); ok && operation != "" {
		attrs = append(attrs, attribute.String(attrBatchOp, operation))
	}
	spanCtx, span := otel.Tracer(spanTracer).Start(ctx, spanName, oteltrace.WithAttributes(attrs...))

	resp, err := client.Do(req.WithContext(spanCtx))
	if err != nil {
		endSpan(span, 0, spanClass(0, err, ctx.Err()))
		return nil, &transportError{cause: transportFailure(err)}
	}
	resp.Body = &spanBody{ReadCloser: resp.Body, span: span, status: resp.StatusCode}
	return resp, nil
}

func endSpan(span oteltrace.Span, status int, class string) {
	if status > 0 {
		span.SetAttributes(attribute.Int(attrStatus, status))
	}
	if class != "" {
		span.SetAttributes(attribute.String(attrClass, class))
		span.SetStatus(codes.Error, class)
	}
	span.End()
}

type spanBody struct {
	io.ReadCloser
	span    oteltrace.Span
	status  int
	once    sync.Once
	invalid bool
	readErr error
}

// Read records a failure while the body is read, so the span does not end
// as a success when the answer never fully arrived.
func (b *spanBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.readErr == nil {
		b.readErr = err
	}
	return n, err
}

func (b *spanBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		class := spanClass(b.status, nil, nil)
		switch {
		case class != "":
		case b.readErr != nil:
			class = spanClass(0, b.readErr, nil)
		case b.invalid:
			class = "invalid_answer"
		}
		endSpan(b.span, b.status, class)
	})
	return err
}

// markInvalidAnswer records on resp's span that a 2xx body could not be used.
func markInvalidAnswer(resp *http.Response) {
	if body, ok := resp.Body.(*spanBody); ok {
		body.invalid = true
	}
}

// classifiedError carries this package's own text and keeps the cause for
// errors.Is/As.
type classifiedError struct {
	text  string
	cause error
}

func (e *classifiedError) Error() string { return e.text }
func (e *classifiedError) Unwrap() error { return e.cause }

func transportFailure(err error) error {
	op := "exchange"
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Op != "" {
		op = urlErr.Op
	}
	class := "failed"
	switch {
	case errors.Is(err, context.Canceled):
		class = "canceled"
	case isTimeout(err):
		class = "timeout"
	}
	cause := err
	if urlErr != nil {
		// *url.Error's text quotes the request URL, which can carry userinfo
		// or a query from a customer-supplied base URL: keep its operation
		// and inner error (for errors.Is/As), never the URL.
		cause = &url.Error{Op: urlErr.Op, URL: redactedURL(urlErr.URL), Err: urlErr.Err}
	}
	return &classifiedError{text: op + " request " + class, cause: cause}
}

// redactedURL is raw's scheme and host only.
func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "<redacted>"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func decodeFailure(err error) error {
	class := "invalid"
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		class = "syntax"
	case errors.As(err, &typeErr):
		class = "type"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		class = "eof"
	}
	return &Error{Class: ClassGeneric, message: "decode response: " + class, cause: &classifiedError{text: "decode response: " + class, cause: err}}
}

// sleepFor waits delay, false when ctx ended first.
func sleepFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// exchange sends one request and hands a 2xx body to decode before the body
// is closed, so a body decode cannot use is marked on the attempt's span. A
// non-2xx answer is a *statusError, no answer a *transportError, an unusable
// 2xx body a ClassOutput *Error.
func exchange(ctx context.Context, client *http.Client, method, target string, header http.Header, body io.Reader,
	provider Kind, model string, decode func([]byte) error,
) error {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New("llm: build request")
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := do(client, req, provider, model)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &transportError{cause: transportFailure(err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{statusCode: resp.StatusCode, header: resp.Header, body: string(raw)}
	}
	if err := decode(raw); err != nil {
		markInvalidAnswer(resp)
		return decodeFailure(err)
	}
	return nil
}
