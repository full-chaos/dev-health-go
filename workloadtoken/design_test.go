package workloadtoken

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// captured is the request the transport behind the token transport received.
type captured struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (c *captured) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.body = append(c.body, body)
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
}

func (c *captured) last(t *testing.T) (*http.Request, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatal("nothing reached the transport behind")
	}
	return c.reqs[len(c.reqs)-1], c.body[len(c.body)-1]
}

// TestTheTransportBuildsItsOwnRequest: the request sent is a new one, built from the fixed target, the caller's method,
// path, query and body, the allow-listed caller headers and exactly one Authorization; nothing else of the caller's.
func TestTheTransportBuildsItsOwnRequest(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	next := &captured{}
	rt, err := c.Transport(next, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://API.test:443/v1/thing%2Fx?a=1&b=2", strings.NewReader(`{"k":1}`))
	req.Header = http.Header{
		"accept": {"application/json"}, "Content-Type": {"application/json"}, "user-agent": {"probe/1"},
		"X-Request-Id": {"r-1"}, "traceparent": {"00-abc-def-01"}, "TRACESTATE": {"k=v"},
		"Cookie": {"session=secret"}, "Proxy-Authorization": {"Basic secret"}, "Proxy-Connection": {"keep-alive"},
		"Forwarded": {"for=1.2.3.4"}, "X-Forwarded-For": {"1.2.3.4"}, "X-Forwarded-Host": {"attacker.test"},
		"X-Forwarded-Proto": {"http"}, "Authorization": {"Bearer secret"}, "authorization": {"Bearer secret"},
		"Content-Length": {"999"}, "X-Custom": {"secret"}, "Host": {"attacker.test"},
	}
	before := req.Header.Clone()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	out, body := next.last(t)
	if out == req {
		t.Fatal("the caller's request itself was sent")
	}
	if out.Method != http.MethodPost || out.URL.String() != "https://api.test/v1/thing%2Fx?a=1&b=2" || out.Host != "api.test" {
		t.Fatalf("sent %s %s Host %q", out.Method, out.URL, out.Host)
	}
	var keys []string
	for key := range out.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"Accept", "Authorization", "Content-Type", "Traceparent", "Tracestate", "User-Agent", "X-Request-Id"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("sent header keys %v, want exactly %v", keys, want)
	}
	if got := out.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+tokenN(0) {
		t.Fatalf("Authorization %v", got)
	}
	for key, value := range map[string]string{"Accept": "application/json", "Content-Type": "application/json", "User-Agent": "probe/1",
		"X-Request-Id": "r-1", "Traceparent": "00-abc-def-01", "Tracestate": "k=v"} {
		if got := out.Header.Get(key); got != value {
			t.Fatalf("%s %q, want %q", key, got, value)
		}
	}
	var wire bytes.Buffer
	_ = out.Header.Write(&wire)
	if strings.Contains(wire.String(), "secret") || strings.Contains(wire.String(), "attacker") {
		t.Fatalf("a caller value left:\n%s", wire.String())
	}
	if body != `{"k":1}` || out.ContentLength != 7 {
		t.Fatalf("body %q length %d", body, out.ContentLength)
	}
	if len(out.TransferEncoding) != 0 || len(out.Trailer) != 0 {
		t.Fatal("transfer encoding or trailer copied")
	}
	if req.Header.Get("Cookie") != before.Get("Cookie") || len(req.Header) != len(before) || req.Host != "API.test:443" {
		t.Fatal("the caller's request was changed")
	}
}

func TestOnlyTheListedMethodsAreSent(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	next := &captured{}
	rt, err := c.Transport(next, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"", http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
		req.Method = method
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("method %q: %v", method, err)
		}
		_ = resp.Body.Close()
	}
	sent := len(next.reqs)
	for _, method := range []string{http.MethodConnect, http.MethodTrace, http.MethodOptions, "FOO", "get", "PROPFIND"} {
		req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
		req.Method = method
		if resp, err := rt.RoundTrip(req); !errors.Is(err, ErrRequestRefused) {
			if resp != nil {
				_ = resp.Body.Close()
			}
			t.Fatalf("method %q: err %v, want ErrRequestRefused", method, err)
		}
	}
	if len(next.reqs) != sent {
		t.Fatal("a refused method was sent")
	}
}

func TestACallerURLIsReadNeverRewritten(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	next := &captured{}
	rt, err := c.Transport(next, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*url.URL{
		{Scheme: "https", Host: "attacker.test", Path: "/x"},
		{Scheme: "https", Host: "api.test:8443", Path: "/x"},
		{Scheme: "http", Host: "api.test", Path: "/x"},
		{Host: "api.test", Path: "/x"},
		{Scheme: "https", Path: "/x"},
		{Path: "x"},
	} {
		req := &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}
		if resp, err := rt.RoundTrip(req); !errors.Is(err, ErrRequestRefused) {
			if resp != nil {
				_ = resp.Body.Close()
			}
			t.Fatalf("URL %#v: err %v, want ErrRequestRefused", u, err)
		}
	}
	if len(next.reqs) != 0 {
		t.Fatal("a refused URL was sent")
	}
	for raw, want := range map[string]string{"": "https://api.test/", "/": "https://api.test/", "/a/b?q=1": "https://api.test/a/b?q=1"} {
		u, _ := url.Parse(raw)
		resp, err := rt.RoundTrip(&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}})
		if err != nil {
			t.Fatalf("relative %q: %v", raw, err)
		}
		_ = resp.Body.Close()
		if out, _ := next.last(t); out.URL.String() != want {
			t.Fatalf("relative %q sent to %s, want %s", raw, out.URL, want)
		}
	}
}

func TestAnEmptyHostnameIsRefused(t *testing.T) {
	path := writeSubject(t, testSubject)
	for _, raw := range []string{"https://:443", "https://:8095", "https://[]:443"} {
		if _, err := New(Config{ExchangeURL: raw, Subject: KubernetesTokenFile(path), Transport: &fakeExchange{}}); err == nil {
			t.Fatalf("New accepted the exchange URL %q", raw)
		}
		c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path)
		if _, err := c.Transport(&captured{}, raw, testAudience); err == nil {
			t.Fatalf("Transport accepted the target %q", raw)
		}
	}
	plain := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path, func(cfg *Config) { cfg.AllowPlainHTTP = true })
	if _, err := plain.Transport(&captured{}, "http://:80", testAudience); err == nil {
		t.Fatal("Transport accepted http://:80")
	}
}

func TestHTTPClientRefusesEveryRedirect(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	api := &hostResource{status: http.StatusFound, redirect: map[string]string{"api.test/v1/thing": "https://api.test/elsewhere"}}
	client, err := c.HTTPClient(api, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect == nil {
		t.Fatal("the client has no redirect policy")
	}
	resp, err := client.Get(testTarget + "/v1/thing")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect was followed")
	}
	if !errors.Is(err, ErrRequestRefused) {
		t.Fatalf("err %v, want ErrRequestRefused", err)
	}
	if got := api.seen("api.test"); len(got) != 1 {
		t.Fatalf("the target received %d requests, want 1", len(got))
	}
}

// TestATokenNotYetValidIsRefusedAtOnce: an issuer clock ahead of this host's puts nbf in the future; the strictest legal
// verifier (leeway 0) refuses such a token, so the client does not send it, and logs the skew once (never the token).
func TestATokenNotYetValidIsRefusedAtOnce(t *testing.T) {
	for _, ahead := range []int64{1, 5, 299} {
		t.Run(time.Duration(ahead*int64(time.Second)).String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
					if len(groups) == 0 && a.Key == slog.TimeKey {
						return slog.Attr{}
					}
					return a
				}}))
				var token string
				exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
					now := time.Now().Unix()
					token = signToken(n, now+ahead, now+ahead, now+ahead+300)
					return issued(token, 300), nil
				}}
				c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.Logger = logger })
				next := &captured{}
				rt, err := c.Transport(next, testTarget, testAudience)
				if err != nil {
					t.Fatal(err)
				}
				req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
				_, err = rt.RoundTrip(req)
				mustNoUsable(t, err, ErrExchangeUnavailable)
				if len(next.reqs) != 0 {
					t.Fatal("a not-yet-valid token was sent")
				}
				if _, err := testVerifier(t).Verify(token, time.Now()); err == nil {
					t.Fatal("the oracle accepts the token: the case does not test anything")
				}
				if got := strings.Count(logs.String(), "workload token not yet valid"); got != 1 || !strings.Contains(logs.String(), "ahead_seconds=") {
					t.Fatalf("want one not-yet-valid line with ahead_seconds:\n%s", logs.String())
				}
				if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), testSubject) {
					t.Fatalf("the log carries a credential:\n%s", logs.String())
				}
			})
		})
	}
}

// TestEveryTokenTheClientUsesTheStrictestVerifierAccepts: for issuer clocks equal to and behind this host's, at every
// second of a token's life, a token Token returns is one a leeway-0 WorkloadVerifier accepts at that instant.
func TestEveryTokenTheClientUsesTheStrictestVerifierAccepts(t *testing.T) {
	for _, behind := range []int64{0, 1, 29, 30, 31, 200, 269, 270, 299} {
		t.Run(time.Duration(behind*int64(time.Second)).String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
					now := time.Now().Unix() - behind
					return issued(signToken(n, now, now, now+300), 300), nil
				}}
				c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.BackoffFloor, cfg.BackoffCeiling = time.Second, time.Second })
				verifier := testVerifier(t)
				start := time.Now()
				used := 0
				for second := 0; second <= 600; second++ {
					sleepUntil(start, time.Duration(second)*time.Second)
					token, err := c.Token(context.Background(), testAudience)
					if err != nil {
						continue
					}
					used++
					if _, verr := verifier.Verify(token, time.Now()); verr != nil {
						t.Fatalf("at %ds the client used a token the verifier refuses: %v", second, verr)
					}
				}
				if behind < 270 && used == 0 {
					t.Fatal("no token was ever usable: the case tests nothing")
				}
				if behind >= 270 && used != 0 {
					t.Fatalf("%d uses of a token whose exp is inside the safety margin at receipt", used)
				}
			})
		})
	}
}


func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// TestAClockSteppedBackNeverUsesATokenBeforeItsNbf: the window is checked when Token returns, not only when the token
// arrives; after this host's clock steps back before a cached token's nbf, that token is not used.
func TestAClockSteppedBackNeverUsesATokenBeforeItsNbf(t *testing.T) {
	clock := time.Unix(2_000_000_000, 0)
	var mu sync.Mutex
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
		if n > 0 {
			return outage(n, nil)
		}
		at := now().Unix()
		return issued(signToken(n, at, at, at+300), 300), nil
	}}
	c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.Now = now })
	if _, err := c.Token(context.Background(), testAudience); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clock = clock.Add(-5 * time.Second)
	mu.Unlock()
	token, err := c.Token(context.Background(), testAudience)
	if err == nil {
		t.Fatalf("a token was used 5 s before its nbf (the verifier refuses it): %q", token[:10])
	}
	mustNoUsable(t, err, nil)
}
