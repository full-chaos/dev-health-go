package pep

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func goodAllow(t testing.TB) []byte {
	t.Helper()
	return readFile(t, "authorization_decision", "decision-valid-allow-with-obligations.json")
}

func replaceOnce(t testing.TB, raw []byte, old, new string) []byte {
	t.Helper()
	if !bytes.Contains(raw, []byte(old)) {
		t.Fatalf("fixture lacks %q", old)
	}
	return bytes.Replace(raw, []byte(old), []byte(new), 1)
}

type failure struct {
	name    string
	handler func(w http.ResponseWriter, r *http.Request)
	want    Reason
}

func transportFailures(t *testing.T) []failure {
	good := goodAllow(t)
	cases := []failure{
		{"500", replyBytes(500, "application/json", good), ReasonUnavailable},
		{"502", replyBytes(502, "", nil), ReasonUnavailable},
		{"503", replyBytes(503, "application/json", []byte(`{}`)), ReasonUnavailable},
		{"504", replyBytes(504, "", nil), ReasonUnavailable},
		{"429", replyBytes(429, "application/json", good), ReasonRateLimited},
		{"400", replyBytes(400, "application/json", good), ReasonRefused},
		{"401", replyBytes(401, "application/json", good), ReasonRefused},
		{"403", replyBytes(403, "application/json", good), ReasonRefused},
		{"404", replyBytes(404, "application/json", good), ReasonRefused},
		{"413", replyBytes(413, "application/json", good), ReasonRefused},
		{"201 with an allow body", replyBytes(201, "application/json", good), ReasonRefused},
		{"204", replyBytes(204, "", nil), ReasonRefused},
		{"redirect is not followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
		}, ReasonRefused},
		{"no content type", replyBytes(200, "", good), ReasonMalformed},
		{"text content type", replyBytes(200, "text/plain", good), ReasonMalformed},
		{"empty body", replyJSON(nil), ReasonMalformed},
		{"null", replyJSON([]byte(`null`)), ReasonMalformed},
		{"array", replyJSON([]byte(`[]`)), ReasonMalformed},
		{"not json", replyJSON([]byte(`allowed`)), ReasonMalformed},
		{"truncated", replyJSON(good[:len(good)-12]), ReasonMalformed},
		{"trailing document", replyJSON(append(append([]byte{}, good...), []byte(` {}`)...)), ReasonMalformed},
		{"trailing garbage", replyJSON(append(append([]byte{}, good...), 'x')), ReasonMalformed},
		{"error envelope as a decision", replyJSON(replaceOnce(t, good, `"authorization_decision.v1"`, `"error.v1"`)), ReasonMalformed},
		{"unknown member", replyJSON(replaceOnce(t, good, `"allowed": true,`, `"allowed": true, "extra": 1,`)), ReasonMalformed},
		{"member in another case", replyJSON(replaceOnce(t, good, `"allowed"`, `"Allowed"`)), ReasonMalformed},
		{"duplicate member", replyJSON(replaceOnce(t, good, `"allowed": true,`, `"allowed": false, "allowed": true,`)), ReasonMalformed},
		{"duplicate member by escape", replyJSON(replaceOnce(t, good, `"allowed": true,`, `"allowed": false, "allowed": true,`)), ReasonMalformed},
		{"duplicate member in obligations", replyJSON(replaceOnce(t, good, `"step_up": false,`, `"step_up": false, "step_up": true,`)), ReasonMalformed},
		{"allowed as a string", replyJSON(replaceOnce(t, good, `"allowed": true`, `"allowed": "true"`)), ReasonMalformed},
		{"allowed missing", replyJSON(replaceOnce(t, good, `"allowed": true,`, ``)), ReasonMalformed},
		{"unknown reason", replyJSON(replaceOnce(t, good, `allowed_by_repository_grant`, `allowed_by_magic`)), ReasonMalformed},
		{"allow with a deny reason", replyJSON(replaceOnce(t, good, `allowed_by_repository_grant`, `grant_absent`)), ReasonMalformed},
		{"deny with an allow reason", replyJSON(replaceOnce(t, readFile(t, "authorization_decision", "decision-valid-deny.json"), `"reason_code": "grant_absent"`, `"reason_code": "allowed_by_role"`)), ReasonMalformed},
		{"policy revision as decimal", replyJSON(replaceOnce(t, good, `"policy_revision": 42`, `"policy_revision": 42.0`)), ReasonMalformed},
		{"policy revision as exponent", replyJSON(replaceOnce(t, good, `"policy_revision": 42`, `"policy_revision": 4.2e1`)), ReasonMalformed},
		{"policy revision negative", replyJSON(replaceOnce(t, good, `"policy_revision": 42`, `"policy_revision": -1`)), ReasonMalformed},
		{"policy revision over int64", replyJSON(replaceOnce(t, good, `"policy_revision": 42`, `"policy_revision": 9223372036854775808`)), ReasonMalformed},
		{"policy revision leading zero", replyJSON(replaceOnce(t, good, `"policy_revision": 42`, `"policy_revision": 042`)), ReasonMalformed},
		{"expiry with 7 fractional digits", replyJSON(replaceOnce(t, good, `2026-09-03T05:00:30Z`, `2026-09-03T05:00:30.1234567Z`)), ReasonMalformed},
		{"unknown obligation", replyJSON(replaceOnce(t, good, `"step_up": false,`, `"step_up": false, "export_ok": true,`)), ReasonUnknownObligation},
		{"organization differs", replyJSON(replaceOnce(t, good, `"organization_id": "org_EXAMPLE0000000000000001",`, `"organization_id": "org_EXAMPLE0000000000000002",`)), ReasonMismatch},
	}
	return cases
}

func TestSingleEveryFailureIsALocalDenyNeverAnAllow(t *testing.T) {
	for _, tc := range transportFailures(t) {
		t.Run(tc.name, func(t *testing.T) {
			pdp := newPDP(t, tc.handler)
			d := pdp.client(t).Decide(context.Background(), baseRequest())
			if d.Allowed() {
				t.Fatal("allowed")
			}
			if !d.Local() || d.Reason() != tc.want {
				t.Fatalf("reason %q local=%v, want local %q", d.Reason(), d.Local(), tc.want)
			}
			var e *Error
			if !errors.As(d.Err(), &e) || e.Reason() != tc.want {
				t.Fatalf("Err() = %v, want a *Error for %s", d.Err(), tc.want)
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var elsewhere int
	pdp := newPDP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			elsewhere++
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	pdp.client(t).Decide(context.Background(), baseRequest())
	if elsewhere != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestBatchEveryFailureDeniesEveryItem(t *testing.T) {
	good := readFile(t, "authorization_batch_response", "batch-response-valid-items-disagree.json")
	reqs := batchRequests(t, readManifest(t, "authorization_batch_response"))
	cases := []failure{
		{"500", replyBytes(500, "", nil), ReasonUnavailable},
		{"429", replyBytes(429, "", nil), ReasonRateLimited},
		{"403", replyBytes(403, "application/json", good), ReasonRefused},
		{"truncated", replyJSON(good[:len(good)/2]), ReasonMalformed},
		{"unknown member", replyJSON(replaceOnce(t, good, `"decisions"`, `"extra": 1, "decisions"`)), ReasonMalformed},
		{"duplicate member", replyJSON(replaceOnce(t, good, `"decisions"`, `"decisions": [], "decisions"`)), ReasonMalformed},
		{"wrong schema version", replyJSON(replaceOnce(t, good, `authorization_batch_response.v1`, `authorization_decision.v1`)), ReasonMalformed},
		{"item organization differs", replyJSON(replaceOnce(t, good, `"organization_id": "org_EXAMPLE0000000000000001"`, `"organization_id": "org_EXAMPLE0000000000000002"`)), ReasonMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pdp := newPDP(t, tc.handler)
			got := pdp.client(t).DecideBatch(context.Background(), reqs)
			if len(got) != len(reqs) {
				t.Fatalf("%d decisions for %d requests", len(got), len(reqs))
			}
			for i, d := range got {
				if d.Allowed() || !d.Local() || d.Reason() != tc.want {
					t.Fatalf("item %d: allowed=%v local=%v reason=%s, want local %s", i, d.Allowed(), d.Local(), d.Reason(), tc.want)
				}
			}
		})
	}
}

func TestBatchBounds(t *testing.T) {
	pdp := newPDP(t, replyBytes(500, "", nil))
	c := pdp.client(t)
	if got := c.DecideBatch(context.Background(), nil); len(got) != 0 {
		t.Fatalf("an empty batch returned %d decisions", len(got))
	}
	if pdp.calls.Load() != 0 {
		t.Fatal("an empty batch made a call")
	}
	over := make([]Request, MaxBatch+1)
	for i := range over {
		over[i] = baseRequest()
	}
	got := c.DecideBatch(context.Background(), over)
	if len(got) != len(over) {
		t.Fatalf("%d decisions for %d requests", len(got), len(over))
	}
	for _, d := range got {
		if d.Allowed() || d.Reason() != ReasonInvalidRequest {
			t.Fatalf("reason %s", d.Reason())
		}
	}
	bad := []Request{baseRequest(), baseRequest()}
	bad[1].Action = "admin"
	for _, d := range c.DecideBatch(context.Background(), bad) {
		if d.Allowed() || d.Reason() != ReasonInvalidRequest {
			t.Fatalf("one invalid item must deny the whole batch, reason %s", d.Reason())
		}
	}
	if pdp.calls.Load() != 0 {
		t.Fatal("an invalid batch made a call")
	}
}

func TestBatchOf100IsSent(t *testing.T) {
	pdp := newPDP(t, replyBytes(500, "", nil))
	reqs := make([]Request, MaxBatch)
	for i := range reqs {
		reqs[i] = baseRequest()
		reqs[i].RequestID = ""
	}
	pdp.client(t).DecideBatch(context.Background(), reqs)
	if pdp.calls.Load() != 1 {
		t.Fatal("a batch of 100 was not sent")
	}
}

func TestOversizeBodyIsADeny(t *testing.T) {
	pad := bytes.Repeat([]byte(" "), maxSingleBody)
	body := append(append([]byte{}, goodAllow(t)...), pad...)
	for name, h := range map[string]func(http.ResponseWriter, *http.Request){
		"declared": replyJSON(body),
		"chunked": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			f := w.(http.Flusher)
			_, _ = w.Write(goodAllow(t))
			f.Flush()
			_, _ = w.Write(pad)
		},
	} {
		t.Run(name, func(t *testing.T) {
			pdp := newPDP(t, h)
			d := pdp.client(t).Decide(context.Background(), baseRequest())
			if d.Allowed() || d.Reason() != ReasonTooLarge {
				t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
			}
		})
	}
	pdp := newPDP(t, replyJSON(bytes.Repeat([]byte(" "), maxBatchBody+1)))
	reqs := []Request{baseRequest(), baseRequest()}
	for _, d := range pdp.client(t).DecideBatch(context.Background(), reqs) {
		if d.Allowed() || d.Reason() != ReasonTooLarge {
			t.Fatalf("batch reason %s", d.Reason())
		}
	}
}

func TestNetworkTimeoutAndCancel(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		pdp := newPDP(t, replyBytes(200, "", nil))
		c := pdp.client(t)
		pdp.srv.Close()
		d := c.Decide(context.Background(), baseRequest())
		if d.Allowed() || d.Reason() != ReasonUnreachable {
			t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
		}
		for _, b := range c.DecideBatch(context.Background(), []Request{baseRequest()}) {
			if b.Allowed() || b.Reason() != ReasonUnreachable {
				t.Fatalf("batch reason %s", b.Reason())
			}
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		release := make(chan struct{})
		pdp := newPDP(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		t.Cleanup(func() { close(release) })
		c, err := New(Config{BaseURL: pdp.srv.URL, AllowPlainHTTP: true, HTTPClient: pdp.srv.Client(), ClientService: "dev-health-web", Timeout: 30 * time.Millisecond, Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		d := c.Decide(context.Background(), baseRequest())
		if d.Allowed() || d.Reason() != ReasonTimeout {
			t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		release := make(chan struct{})
		pdp := newPDP(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		d := pdp.client(t).Decide(ctx, baseRequest())
		if d.Allowed() || d.Reason() != ReasonTimeout {
			t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
		}
	})
	t.Run("cancel", func(t *testing.T) {
		pdp := newPDP(t, replyBytes(200, "", nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d := pdp.client(t).Decide(ctx, baseRequest())
		if d.Allowed() || d.Reason() != ReasonCanceled {
			t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
		}
		if pdp.calls.Load() != 0 {
			t.Fatal("a canceled call reached the network")
		}
	})
	t.Run("nil context", func(t *testing.T) {
		pdp := newPDP(t, replyBytes(200, "", nil))
		//nolint:staticcheck // the nil context is the case under test
		d := pdp.client(t).Decide(nil, baseRequest())
		if d.Allowed() || d.Reason() != ReasonInvalidRequest {
			t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
		}
	})
	t.Run("body read cut off", func(t *testing.T) {
		pdp := newPDP(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write(goodAllow(t))
		})
		d := pdp.client(t).Decide(context.Background(), baseRequest())
		if d.Allowed() || !d.Local() {
			t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
		}
	})
}

func TestErrorNeverCarriesTheTransportError(t *testing.T) {
	pdp := newPDP(t, replyBytes(200, "", nil))
	c := pdp.client(t)
	url := pdp.srv.URL
	pdp.srv.Close()
	d := c.Decide(context.Background(), baseRequest())
	if msg := d.Err().Error(); strings.Contains(msg, url) || strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "dial") {
		t.Fatalf("the error leaks the transport error: %q", msg)
	}
	if errors.Unwrap(d.Err()) != nil {
		t.Fatal("the error wraps the transport error")
	}
}

func TestRequestOnTheWire(t *testing.T) {
	pdp := newPDP(t, replyBytes(500, "", nil))
	c := pdp.client(t)
	c.Decide(context.Background(), baseRequest())
	if got := <-pdp.paths; got != "POST /v1/authorize" {
		t.Fatalf("single went to %s", got)
	}
	c.DecideBatch(context.Background(), []Request{baseRequest()})
	if got := <-pdp.paths; got != "POST /v1/authorize:batch" {
		t.Fatalf("batch went to %s", got)
	}
}

func TestRequestIDIsMintedWhenAbsent(t *testing.T) {
	pdp := newPDP(t, replyBytes(500, "", nil))
	r := baseRequest()
	r.RequestID = ""
	c := pdp.client(t)
	c.Decide(context.Background(), r)
	c.Decide(context.Background(), r)
	a, b := string(<-pdp.bodies), string(<-pdp.bodies)
	pick := func(s string) string {
		i := strings.Index(s, `"request_id":"`)
		if i < 0 {
			t.Fatalf("no request_id in %s", s)
		}
		return s[i+len(`"request_id":"`) : i+len(`"request_id":"`)+30]
	}
	if x, y := pick(a), pick(b); x == y || !requestIDPattern.MatchString(x) || !requestIDPattern.MatchString(y) {
		t.Fatalf("minted ids %q %q", x, y)
	}
}

func TestZeroAndNilClientsDeny(t *testing.T) {
	var nilClient *Client
	for name, c := range map[string]*Client{"nil": nilClient, "zero": {}} {
		d := c.Decide(context.Background(), baseRequest())
		if d.Allowed() || d.Reason() != ReasonNotConfigured || !d.Local() {
			t.Fatalf("%s client: allowed=%v reason=%s", name, d.Allowed(), d.Reason())
		}
		got := c.DecideBatch(context.Background(), []Request{baseRequest(), baseRequest()})
		if len(got) != 2 || got[0].Allowed() || got[1].Allowed() || got[0].Reason() != ReasonNotConfigured {
			t.Fatalf("%s client batch: %+v", name, got)
		}
	}
}

func TestZeroDecisionDenies(t *testing.T) {
	var d Decision
	if d.Allowed() || d.Reason() != ReasonNoDecision || !d.Local() || d.Err() == nil {
		t.Fatalf("zero decision: allowed=%v reason=%s local=%v err=%v", d.Allowed(), d.Reason(), d.Local(), d.Err())
	}
	if !d.ExpiresAt().IsZero() {
		t.Fatal("zero decision has an expiry")
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	ok := Config{BaseURL: "https://auth.example.test", HTTPClient: &http.Client{}, ClientService: "dev-health-web"}
	if _, err := New(ok); err != nil {
		t.Fatalf("the good config was refused: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"no client":         func(c *Config) { c.HTTPClient = nil },
		"no url":            func(c *Config) { c.BaseURL = "" },
		"plain http":        func(c *Config) { c.BaseURL = "http://auth.example.test" },
		"url with a path":   func(c *Config) { c.BaseURL = "https://auth.example.test/v1" },
		"url with userinfo": func(c *Config) { c.BaseURL = "https://u:p@auth.example.test" },
		"url with a query":  func(c *Config) { c.BaseURL = "https://auth.example.test/?a=b" },
		"ftp":               func(c *Config) { c.BaseURL = "ftp://auth.example.test" },
		"no service":        func(c *Config) { c.ClientService = "" },
		"unknown service":   func(c *Config) { c.ClientService = "dev-health-nothing" },
		"negative timeout":  func(c *Config) { c.Timeout = -time.Second },
	} {
		cfg := ok
		mut(&cfg)
		if c, err := New(cfg); err == nil || c != nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHTTPClientIsNotMutated(t *testing.T) {
	hc := &http.Client{}
	c, err := New(Config{BaseURL: "https://auth.example.test", HTTPClient: hc, ClientService: "dev-health-web"})
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if hc.CheckRedirect != nil || hc.Timeout != 0 {
		t.Fatal("New changed the caller's http.Client")
	}
}
