package workloadtoken

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestTheExchangeRequestIsTheRFC8693Form(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeSubject(t, testSubject+"\n"))
	if _, err := c.Token(context.Background(), testAudience, "read", "list", "read"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(context.Background(), "auth-service"); err != nil {
		t.Fatal(err)
	}
	first := exchange.call(t, 0)
	if first.method != http.MethodPost || first.url != testBase+ExchangePath {
		t.Errorf("request %s %s, want POST %s", first.method, first.url, testBase+ExchangePath)
	}
	if got := first.header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type %q", got)
	}
	if got := first.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept %q", got)
	}
	if got := first.header.Get("Authorization"); got != "" {
		t.Errorf("the exchange request carries an Authorization header %q", got)
	}
	want := url.Values{
		"grant_type": {GrantTypeTokenExchange}, "subject_token": {testSubject}, "subject_token_type": {TokenTypeJWT},
		"audience": {testAudience}, "scope": {"list read"},
	}
	if first.form.Encode() != want.Encode() {
		t.Errorf("form %q, want %q", first.form.Encode(), want.Encode())
	}
	second := exchange.call(t, 1)
	if _, present := second.form["scope"]; present || second.form.Get("audience") != "auth-service" {
		t.Errorf("an empty scope must send no scope field: %q", second.raw)
	}
}

func TestOneExchangeServesEveryCallBeforeTheRefreshPoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exchange := &fakeExchange{answer: issueEach(300)}
		c := newTestClient(t, exchange, writeSubject(t, testSubject))
		start := time.Now()
		for _, at := range []time.Duration{0, time.Second, 100 * time.Second, 149*time.Second + 999*time.Millisecond} {
			sleepUntil(start, at)
			token, err := c.Token(context.Background(), testAudience)
			if err != nil || token != tokenN(0) {
				t.Fatalf("at %v: token %q err %v, want the first token", at, token, err)
			}
		}
		if got := exchange.count(); got != 1 {
			t.Fatalf("%d exchanges before the refresh point, want 1", got)
		}
		sleepUntil(start, 150*time.Second)
		token, err := c.Token(context.Background(), testAudience)
		if err != nil || token != tokenN(1) {
			t.Fatalf("at the refresh point: token %q err %v, want a new token", token, err)
		}
		if got := exchange.count(); got != 2 {
			t.Fatalf("%d exchanges at the refresh point, want 2", got)
		}
		if _, err := c.Token(context.Background(), "auth-service"); err != nil || exchange.count() != 3 {
			t.Fatalf("another audience has its own token: err %v, exchanges %d", err, exchange.count())
		}
	})
}

// skewedIssue answers with a token whose exp claim is stamped by an issuer
// clock skew away from the client's, and expires_in from that clock.
func skewedIssue(skew time.Duration, expiresIn int) func(int, url.Values) (*http.Response, error) {
	return func(n int, _ url.Values) (*http.Response, error) {
		issuerNow := time.Now().Add(skew)
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"jti":"t%d","iat":%d,"exp":%d}`,
			n, issuerNow.Unix(), issuerNow.Add(time.Duration(expiresIn)*time.Second).Unix())))
		return issued("eyJhbGciOiJFZERTQSJ9."+payload+".c2ln", expiresIn), nil
	}
}

func TestTheRefreshPointDependsOnlyOnExpiresInUnderClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{-10 * time.Minute, -30 * time.Second, 30 * time.Second, 10 * time.Minute} {
		for _, tc := range []struct {
			expiresIn             int
			refreshAt, usableTill time.Duration
		}{
			{300, 150 * time.Second, 270 * time.Second},
			{100, 50 * time.Second, 70 * time.Second},
			{80, 40 * time.Second, 50 * time.Second},
		} {
			t.Run(fmt.Sprintf("skew %v expires_in %d", skew, tc.expiresIn), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					exchange := &fakeExchange{answer: skewedIssue(skew, tc.expiresIn)}
					c := newTestClient(t, exchange, writeSubject(t, testSubject))
					start := time.Now()
					first, err := c.Token(context.Background(), testAudience)
					if err != nil {
						t.Fatal(err)
					}
					sleepUntil(start, tc.refreshAt-time.Millisecond)
					if token, _ := c.Token(context.Background(), testAudience); token != first || exchange.count() != 1 {
						t.Fatalf("an exchange before %v (count %d)", tc.refreshAt, exchange.count())
					}
					exchange.set(outage)
					sleepUntil(start, tc.refreshAt)
					if token, err := c.Token(context.Background(), testAudience); err != nil || token != first || exchange.count() != 2 {
						t.Fatalf("at %v: token changed %v err %v count %d, want one failed re-exchange and the cached token",
							tc.refreshAt, token != first, err, exchange.count())
					}
					sleepUntil(start, tc.usableTill-time.Millisecond)
					if token, err := c.Token(context.Background(), testAudience); err != nil || token != first {
						t.Fatalf("just before %v the cached token is still usable: err %v", tc.usableTill, err)
					}
					sleepUntil(start, tc.usableTill)
					_, err = c.Token(context.Background(), testAudience)
					mustNoUsable(t, err, nil)
				})
			})
		}
	}
}

func TestAnExchangeOutageServesTheCachedTokenUntilItsUsableWindowEnds(t *testing.T) {
	outages := map[string]func(int, url.Values) (*http.Response, error){
		"503":       outage,
		"transport": func(int, url.Values) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") },
		"500":       func(int, url.Values) (*http.Response, error) { return jsonResponse(500, `{}`), nil },
		"malformed": func(int, url.Values) (*http.Response, error) { return jsonResponse(200, `{"access_token":`), nil },
	}
	for name, failing := range outages {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { runOutage(t, failing, nil) })
		})
	}
	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			runOutage(t, issueEach(300), hold)
		})
	})
}

func runOutage(t *testing.T, failing func(int, url.Values) (*http.Response, error), hold chan struct{}) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.Timeout = 10 * time.Millisecond })
	api := &resource{}
	rt, err := c.RoundTripper(api, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := get(t, rt); err != nil {
		t.Fatal(err)
	}
	exchange.mu.Lock()
	exchange.answer, exchange.hold = failing, hold
	exchange.mu.Unlock()
	for _, at := range []time.Duration{150 * time.Second, 200 * time.Second, 269 * time.Second} {
		sleepUntil(start, at)
		if err := get(t, rt); err != nil {
			t.Fatalf("at %v the cached token is still usable: %v", at, err)
		}
	}
	calls := exchange.count()
	if calls != 4 {
		t.Fatalf("%d exchange calls, want 4 (one per check, each past the backoff)", calls)
	}
	sleepUntil(start, 270*time.Second)
	mustNoUsable(t, get(t, rt), ErrExchangeUnavailable)
	if got := exchange.count(); got != calls {
		t.Fatalf("%d exchange calls, want %d: a call inside the backoff must not exchange", got, calls)
	}
	sleepUntil(start, 274*time.Second)
	mustNoUsable(t, get(t, rt), ErrExchangeUnavailable)
	mustNoUsable(t, get(t, rt), nil)
	if got := exchange.count(); got != calls+1 {
		t.Fatalf("%d exchange calls, want %d: one past the backoff, none inside it", got, calls+1)
	}
	seen := api.seen()
	if len(seen) != 4 {
		t.Fatalf("the resource got %d requests, want 4: none after the usable window", len(seen))
	}
	for i, auth := range seen {
		if auth != "Bearer "+tokenN(0) {
			t.Fatalf("resource request %d carried %q, want the first token", i, auth)
		}
	}
	exchange.mu.Lock()
	exchange.answer, exchange.hold = func(int, url.Values) (*http.Response, error) { return issued(tokenN(99), 300), nil }, nil
	exchange.mu.Unlock()
	time.Sleep(DefaultBackoffCeiling)
	if err := get(t, rt); err != nil {
		t.Fatalf("after the outage: %v", err)
	}
	if last := api.seen()[4]; last != "Bearer "+tokenN(99) {
		t.Fatalf("after the outage the resource got %q", last)
	}
}

func TestAnExchangeRefusalSendsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		class error
	}{
		"invalid_grant":   {`{"error":"invalid_grant"}`, ErrExchangeRefused},
		"invalid_request": {`{"error":"invalid_request"}`, ErrExchangeInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			exchange := &fakeExchange{answer: func(int, url.Values) (*http.Response, error) { return jsonResponse(400, tc.body), nil }}
			c := newTestClient(t, exchange, writeSubject(t, testSubject))
			api := &resource{}
			rt, err := c.RoundTripper(api, testTarget, testAudience)
			if err != nil {
				t.Fatal(err)
			}
			mustNoUsable(t, get(t, rt), tc.class)
			if n := len(api.seen()); n != 0 {
				t.Fatalf("the resource got %d requests after a refusal, want 0", n)
			}
		})
	}
}

func TestConcurrentCallersShareOneExchange(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hold := make(chan struct{})
				exchange := &fakeExchange{answer: issueEach(300), hold: hold}
				if fail {
					exchange.answer = outage
				}
				c := newTestClient(t, exchange, writeSubject(t, testSubject))
				const callers = 20
				var wg sync.WaitGroup
				tokens := make([]string, callers)
				errs := make([]error, callers)
				for i := range callers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						tokens[i], errs[i] = c.Token(context.Background(), testAudience)
					}()
				}
				synctest.Wait()
				if got := exchange.count(); got != 1 {
					t.Fatalf("%d exchanges in flight for %d callers, want 1", got, callers)
				}
				close(hold)
				wg.Wait()
				if got := exchange.count(); got != 1 {
					t.Fatalf("%d exchanges, want 1", got)
				}
				for i := range callers {
					if fail {
						mustNoUsable(t, errs[i], nil)
					} else if errs[i] != nil || tokens[i] != tokenN(0) {
						t.Fatalf("caller %d: %q %v", i, tokens[i], errs[i])
					}
				}
			})
		})
	}
}

func TestACancelledCallerDoesNotFailTheSharedExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		exchange := &fakeExchange{answer: issueEach(300), hold: hold}
		c := newTestClient(t, exchange, writeSubject(t, testSubject))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := c.Token(ctx, testAudience)
			done <- err
		}()
		synctest.Wait()
		cancel()
		close(hold)
		if err := <-done; err != nil {
			t.Fatalf("the exchange owner: %v", err)
		}
		if token, err := c.Token(context.Background(), testAudience); err != nil || token != tokenN(0) || exchange.count() != 1 {
			t.Fatalf("token %q err %v count %d", token, err, exchange.count())
		}
	})
}

func TestInvalidateForcesTheNextExchange(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeSubject(t, testSubject))
	if _, err := c.Token(context.Background(), testAudience, "read"); err != nil {
		t.Fatal(err)
	}
	c.Invalidate(testAudience, "read")
	if token, err := c.Token(context.Background(), testAudience, "read"); err != nil || token != tokenN(1) {
		t.Fatalf("token %q err %v, want a new exchange", token, err)
	}
}

func TestTheRoundTripperSetsTheHeaderOnACopy(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeSubject(t, testSubject))
	api := &resource{}
	rt, err := c.RoundTripper(api, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.test/v1/thing", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer svc_static")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := api.seen(); len(got) != 1 || got[0] != "Bearer "+tokenN(0) {
		t.Fatalf("the resource got %v", got)
	}
	if req.Header.Get("Authorization") != "Bearer svc_static" {
		t.Fatal("the caller's request was changed")
	}
}
