package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The response bodies in this package's tests are hand-written in each API's
// documented shape. No test reaches the network.

const testKey = "sk-test-0123456789abcdefghijklmnopqrstuv"

// step is one scripted answer: a status and a body (a string is sent as is,
// anything else as JSON), plus optional headers.
type step struct {
	status int
	body   any
	header http.Header
}

type capture struct {
	mu       sync.Mutex
	paths    []string
	headers  []http.Header
	requests []map[string]any
}

func (c *capture) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

func (c *capture) request(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests[i]
}

// scriptedServer answers the i-th request with steps[i], repeating the last.
func scriptedServer(t *testing.T, steps ...step) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		index := min(len(c.paths), len(steps)-1)
		c.paths = append(c.paths, r.URL.Path)
		c.headers = append(c.headers, r.Header.Clone())
		c.requests = append(c.requests, body)
		c.mu.Unlock()
		s := steps[index]
		for name, values := range s.header {
			w.Header()[name] = values
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		switch typed := s.body.(type) {
		case string:
			_, _ = w.Write([]byte(typed))
		case nil:
		default:
			_ = json.NewEncoder(w).Encode(typed)
		}
	}))
	t.Cleanup(server.Close)
	return server, c
}

func intp(v int) *int { return &v }

// recordSpans installs a recording tracer provider for the test.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return recorder
}

// llmSpans is every ended provider span, in end order.
func llmSpans(recorder *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == spanName {
			spans = append(spans, span)
		}
	}
	return spans
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	return attrs
}
