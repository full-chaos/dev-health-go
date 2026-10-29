package workloadtoken

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// hostResource records, per host, the Authorization of every request that reached it, and answers a redirect for the
// hosts in redirect.
type hostResource struct {
	mu       sync.Mutex
	byHost   map[string][]string
	status   int
	redirect map[string]string
}

func (r *hostResource) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	if r.byHost == nil {
		r.byHost = map[string][]string{}
	}
	r.byHost[req.URL.Host] = append(r.byHost[req.URL.Host], req.Header.Get("Authorization"))
	location, redirect := r.redirect[req.URL.Host+req.URL.Path]
	r.mu.Unlock()
	if redirect {
		return &http.Response{StatusCode: r.status, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (r *hostResource) seen(host string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.byHost[host]...)
}

// TestTheTokenNeverFollowsARedirect: behind an ordinary http.Client, a redirect is refused before the next hop is sent,
// to another host or the same one: the token is never re-attached to a request the caller did not build.
func TestTheTokenNeverFollowsARedirect(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for name, location := range map[string]string{
			"other host": "https://attacker.test/collect",
			"same host":  "https://api.test/elsewhere",
		} {
			t.Run(name+" "+http.StatusText(status), func(t *testing.T) {
				c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
				api := &hostResource{status: status, redirect: map[string]string{"api.test/v1/thing": location}}
				rt, err := c.Transport(api, testTarget, testAudience)
				if err != nil {
					t.Fatal(err)
				}
				client := &http.Client{Transport: rt}
				resp, err := client.Get("https://api.test/v1/thing")
				if err == nil {
					_ = resp.Body.Close()
					t.Fatalf("the redirect was followed (status %d)", resp.StatusCode)
				}
				if got := api.seen("attacker.test"); len(got) != 0 {
					t.Fatalf("the redirect target received %v", got)
				}
				if got := api.seen("api.test"); len(got) != 1 || got[0] != "Bearer "+tokenN(0) {
					t.Fatalf("the target received %v, want only the first request with the token", got)
				}
			})
		}
	}
}

// TestTheTokenIsSentOnlyToItsTarget: a request to any other origin (host, port or scheme) is refused and not sent.
func TestTheTokenIsSentOnlyToItsTarget(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	api := &hostResource{}
	rt, err := c.Transport(api, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://attacker.test/x", "http://api.test/x", "https://api.test:8443/x", "https://api.test.attacker.test/x"} {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		if resp, err := rt.RoundTrip(req); err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s was sent", raw)
		}
	}
	for _, host := range []string{"attacker.test", "api.test:8443", "api.test.attacker.test"} {
		if got := api.seen(host); len(got) != 0 {
			t.Fatalf("%s received %v", host, got)
		}
	}
	if got := api.seen("api.test"); len(got) != 0 {
		t.Fatalf("the http request to api.test was sent: %v", got)
	}
	for _, raw := range []string{"https://api.test/x", "https://API.test/x", "https://api.test:443/x"} {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		_ = resp.Body.Close()
	}
	// The request sent is built for the target, so all three reach it under its own host.
	if got := api.seen("api.test"); len(got) != 3 {
		t.Fatalf("api.test received %d requests, want 3", len(got))
	}
	if resp, err := rt.RoundTrip(&http.Request{Method: http.MethodGet, Header: http.Header{}}); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a request without a URL was sent")
	}
}

func TestRoundTripperRefusesABadTarget(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	for _, target := range []string{"", "api.test", "https://api.test/v1", "https://u@api.test", "https://api.test?x=1", "https://api.test#f", "http://api.test", "ftp://api.test"} {
		if _, err := c.Transport(&hostResource{}, target, testAudience); err == nil {
			t.Fatalf("target %q accepted", target)
		}
	}
	plain := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject), func(cfg *Config) { cfg.AllowPlainHTTP = true })
	if _, err := plain.Transport(&hostResource{}, "http://api.test:8080", testAudience); err != nil {
		t.Fatalf("an http target with AllowPlainHTTP: %v", err)
	}
}

// TestAnAnswerArrivingAfterItsUsableWindowIsNotUsed: the window starts before the request is sent, so a slow answer
// can arrive already past it; it is then not returned, not cached, and nothing is sent.
func TestAnAnswerArrivingAfterItsUsableWindowIsNotUsed(t *testing.T) {
	for _, tc := range []struct {
		delay time.Duration
		ok    bool
	}{{999 * time.Millisecond, true}, {time.Second, false}, {4 * time.Second, false}} {
		t.Run(tc.delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
					time.Sleep(tc.delay)
					return issued(mintToken(n, 31), 31), nil
				}}
				c := newTestClient(t, exchange, writeSubject(t, testSubject))
				api := &hostResource{}
				rt, err := c.Transport(api, testTarget, testAudience)
				if err != nil {
					t.Fatal(err)
				}
				req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
				resp, err := rt.RoundTrip(req)
				if tc.ok {
					if err != nil {
						t.Fatalf("an answer inside its window: %v", err)
					}
					_ = resp.Body.Close()
					return
				}
				mustNoUsable(t, err, ErrExchangeUnavailable)
				if got := api.seen("api.test"); len(got) != 0 {
					t.Fatalf("a token past its usable window was sent: %v", got)
				}
				if _, err := c.Token(context.Background(), testAudience); err == nil {
					t.Fatal("the late answer was cached")
				}
			})
		})
	}
}

func TestAHeaderlessRequestGetsTheTokenWithoutAPanic(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	api := &hostResource{}
	rt, err := c.Transport(api, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
	req.Header = nil
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := api.seen("api.test"); len(got) != 1 || got[0] != "Bearer "+tokenN(0) {
		t.Fatalf("the resource got %v", got)
	}
	if req.Header != nil {
		t.Fatal("the caller's request was changed")
	}
}
