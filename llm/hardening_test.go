package llm

import (
	"context"
	"errors"
	"fmt"
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

func TestErrorPrintsOnlyItsSanitizedText(t *testing.T) {
	err := NewError(ClassGeneric, KindOpenAI, "m", "upstream said key="+testKey)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, err); strings.Contains(got, testKey) {
			t.Errorf("%s prints the key: %s", verb, got)
		}
	}
}

// keyEchoingTransport fails with an error that quotes the request's
// Authorization header, as a careless caller transport might.
type keyEchoingTransport struct{}

func (keyEchoingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("gateway refused %s with %s", req.URL, req.Header.Get("Authorization"))
}

func TestNoUnwrappedLayerCarriesATransportsText(t *testing.T) {
	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: "https://api.example/v1",
		HTTPClient: &http.Client{Transport: keyEchoingTransport{}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), Request{Prompt: "p"})
	if err == nil {
		t.Fatal("no error")
	}
	for layer := err; layer != nil; layer = errors.Unwrap(layer) {
		if strings.Contains(layer.Error(), testKey) || strings.Contains(fmt.Sprintf("%#v", layer), testKey) {
			t.Errorf("layer %T carries the key: %s", layer, layer.Error())
		}
	}
}

func TestACapAboveTheRetryCeilingIsNeverLowered(t *testing.T) {
	server, c := scriptedServer(t,
		step{status: 200, body: map[string]any{"output_text": `{"a":`, "incomplete_details": map[string]any{"reason": "max_output_tokens"}}},
		step{status: 200, body: openAIOK(`{"a":1}`)},
	)
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, MaxOutputTokens: 10000})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: testFormat}); err != nil {
		t.Fatal(err)
	}
	if got := c.request(1)["max_output_tokens"]; got != float64(10000) {
		t.Errorf("retry cap = %v, want 10000 kept", got)
	}
}

// replacedDefault is a process-wide DefaultTransport that is not a
// *http.Transport (some instrumentation libraries install one).
type replacedDefault struct{ inner http.RoundTripper }

func (r replacedDefault) RoundTrip(req *http.Request) (*http.Response, error) {
	return r.inner.RoundTrip(req)
}

func TestAReplacedDefaultTransportDoesNotPanic(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = replacedDefault{inner: previous}
	t.Cleanup(func() { http.DefaultTransport = previous })
	server, c := scriptedServer(t, step{status: 200, body: openAIOK(`{}`)})
	provider, err := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL, HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p"}); err != nil || c.calls() != 1 {
		t.Fatalf("err = %v calls = %d", err, c.calls())
	}
}

// An opaque key no pattern recognises, echoed by a provider in a header and
// in its body: no unwrapped layer may carry it, in any print form.
const opaqueKey = "plainkey7Q"

func assertChainFreeOf(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	for layer := err; layer != nil; layer = errors.Unwrap(layer) {
		for _, form := range []string{layer.Error(), fmt.Sprintf("%#v", layer), fmt.Sprintf("%+v", layer)} {
			if strings.Contains(form, secret) {
				t.Errorf("layer %T carries the key: %s", layer, form)
			}
		}
	}
}

func TestAProviderEchoOfTheKeyIsNotKept(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		w.WriteHeader(418)
		_, _ = w.Write([]byte("rejected credential " + opaqueKey))
	}))
	t.Cleanup(server.Close)
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(opaqueKey), BaseURL: server.URL})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	assertChainFreeOf(t, err, opaqueKey)
}

type echoingBody struct{}

func (echoingBody) Read([]byte) (int, error) { return 0, errors.New("read failed for " + opaqueKey) }
func (echoingBody) Close() error             { return nil }

type bodyFailingTransport struct{}

func (bodyFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: echoingBody{}}, nil
}

func TestABodyReadErrorTextIsNotKept(t *testing.T) {
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(opaqueKey), BaseURL: "https://api.example/v1",
		HTTPClient: &http.Client{Transport: bodyFailingTransport{}}})
	_, err := provider.Complete(context.Background(), Request{Prompt: "p"})
	assertChainFreeOf(t, err, opaqueKey)
}

func TestAnErrorValuePrintsOnlyItsSanitizedText(t *testing.T) {
	value := *NewError(ClassGeneric, KindOpenAI, "m", "upstream said "+testKey)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, value); strings.Contains(got, testKey) {
			t.Errorf("%s of an Error value prints the key: %s", verb, got)
		}
	}
}
