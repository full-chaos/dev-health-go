package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestACallerClientNeverUsesAProxy: a caller-supplied *http.Transport with a
// proxy (http.DefaultTransport's ProxyFromEnvironment, or an explicit one)
// would carry the API key to that proxy. The provider drops the proxy.
func TestACallerClientNeverUsesAProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: "http://provider.invalid/v1", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = provider.Complete(context.Background(), Request{Prompt: "p"})
	if proxied.Load() != 0 {
		t.Fatalf("the proxy received %d requests carrying the API key", proxied.Load())
	}
	if client.Transport.(*http.Transport).Proxy == nil {
		t.Error("the caller's own transport was modified")
	}
}

// TestNoErrorLayerQuotesTheURL: *url.Error's text quotes the request URL,
// which can carry userinfo from a customer-supplied base URL. No layer of the
// returned chain may show it.
func TestNoErrorLayerQuotesTheURL(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close() // nothing listens: the dial is refused

	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: "http://gwuser:hunter2secretpass@" + address + "/v1?token=querysecretvalue"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), Request{Prompt: "p"})
	if class, _ := ClassOf(err); class != ClassTransport {
		t.Fatalf("err = %v, want a transport failure", err)
	}
	for layer := err; layer != nil; layer = errors.Unwrap(layer) {
		for _, secret := range []string{"hunter2secretpass", "querysecretvalue", "gwuser"} {
			if strings.Contains(layer.Error(), secret) {
				t.Errorf("layer %T quotes %q: %s", layer, secret, layer.Error())
			}
		}
	}
}

func TestABudgetsOwnErrorIsNeverAProviderClass(t *testing.T) {
	server, c := scriptedServer(t, step{status: 200, body: openAIOK(`{}`)})
	budget := &recordingBudget{refuse: NewError(ClassRateLimit, KindOpenAI, "m", "budget store busy")}
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, Budget: budget})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if !IsBudgetRefusal(err) {
		t.Fatalf("err = %v, want a budget refusal", err)
	}
	if class, ok := ClassOf(err); ok {
		t.Errorf("ClassOf = %q: a budget error read as a provider failure, which a caller would retry", class)
	}
	if c.calls() != 0 || len(budget.attempts) != 1 {
		t.Errorf("calls=%d admissions=%d, want 0 and 1", c.calls(), len(budget.attempts))
	}
}

func TestATimeoutIsClassedTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	if class, _ := ClassOf(err); class != ClassTimeout {
		t.Fatalf("err = %v, want class timeout", err)
	}
}

func TestABodyReadFailureIsMarkedOnTheSpan(t *testing.T) {
	recorder := recordSpans(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output_text":`))
		// The handler returns with the body short of its length: the client
		// read ends with an unexpected EOF.
	}))
	t.Cleanup(server.Close)
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p"}); err == nil {
		t.Fatal("a truncated body answered as success")
	}
	spans := llmSpans(recorder)
	if len(spans) == 0 {
		t.Fatal("no span")
	}
	for i, span := range spans {
		if attributeOf(span, "error.class") == "" {
			t.Errorf("span %d records no error.class for a body that could not be read", i)
		}
	}
}
