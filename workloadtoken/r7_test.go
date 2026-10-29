package workloadtoken

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// TestOnlyAWholeWorkloadTokenIsUsed: an access token is used only when all three segments are present and base64url, its
// header names EdDSA and workload+jwt, and its signature has the Ed25519 length; anything else is refused and not sent.
func TestOnlyAWholeWorkloadTokenIsUsed(t *testing.T) {
	enc := base64.RawURLEncoding
	now := time.Now().Unix()
	good := signToken(1, now, now, now+300)
	parts := strings.Split(good, ".")
	header := func(h string) string { return enc.EncodeToString([]byte(h)) }
	cases := map[string]string{
		"signature removed":       parts[0] + "." + parts[1] + ".",
		"header removed":          "." + parts[1] + "." + parts[2],
		"signature not base64url": parts[0] + "." + parts[1] + ".!!!!",
		"signature padded":        parts[0] + "." + parts[1] + "." + parts[2] + "==",
		"signature short":         parts[0] + "." + parts[1] + "." + enc.EncodeToString(make([]byte, ed25519.SignatureSize-1)),
		"signature long":          parts[0] + "." + parts[1] + "." + enc.EncodeToString(make([]byte, ed25519.SignatureSize+1)),
		"alg RS256":               header(`{"alg":"RS256","typ":"workload+jwt","kid":"test-key"}`) + "." + parts[1] + "." + parts[2],
		"alg none":                header(`{"alg":"none","typ":"workload+jwt","kid":"test-key"}`) + "." + parts[1] + "." + parts[2],
		"typ JWT":                 header(`{"alg":"EdDSA","typ":"JWT","kid":"test-key"}`) + "." + parts[1] + "." + parts[2],
		"header not JSON":         header(`not json`) + "." + parts[1] + "." + parts[2],
		"header not base64url":    "eyJh!." + parts[1] + "." + parts[2],
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: answerWith(200, "application/json",
				`{"access_token":"`+token+`","token_type":"Bearer","issued_token_type":"`+TokenTypeJWT+`","expires_in":300}`)}, writeSubject(t, testSubject))
			next := &captured{}
			rt, err := c.Transport(next, testTarget, testAudience)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
			_, err = rt.RoundTrip(req)
			mustNoUsable(t, err, ErrExchangeUnavailable)
			if len(next.reqs) != 0 {
				t.Fatal("a broken token was sent")
			}
		})
	}
	t.Run("whole token", func(t *testing.T) {
		c := newTestClient(t, &fakeExchange{answer: answerWith(200, "application/json",
			`{"access_token":"`+good+`","token_type":"Bearer","issued_token_type":"`+TokenTypeJWT+`","expires_in":300}`)}, writeSubject(t, testSubject))
		if token, err := c.Token(context.Background(), testAudience); err != nil || token != good {
			t.Fatalf("token %v err %v", token == good, err)
		}
	})
}

// TestASubjectWithAnEmptyOrBrokenSegmentIsNeverSent: the subject is a compact JWS of an unknown algorithm; every segment
// must be present and base64url.
func TestASubjectWithAnEmptyOrBrokenSegmentIsNeverSent(t *testing.T) {
	for name, value := range map[string]string{
		"empty signature":  "eyJhbGciOiJSUzI1NiJ9.c3ViamVjdA.",
		"empty header":     ".c3ViamVjdA.c2ln",
		"broken payload":   "eyJhbGciOiJSUzI1NiJ9.c3V!amVjdA.c2ln",
		"truncated base64": "eyJhbGciOiJSUzI1NiJ9.abcde.c2ln",
	} {
		t.Run(name, func(t *testing.T) {
			exchange := &fakeExchange{answer: issueEach(300)}
			c := newTestClient(t, exchange, writeSubject(t, value))
			_, err := c.Token(context.Background(), testAudience)
			mustNoUsable(t, err, ErrSubjectUnreadable)
			if exchange.count() != 0 {
				t.Fatal("a broken subject reached the exchange")
			}
		})
	}
}

// TestInvalidateClearsTheBackoff: after a failed refresh the next exchange waits a backoff; Invalidate (the caller's
// resource refused the token) drops the token and the backoff of that key, so the next Token exchanges at once.
func TestInvalidateClearsTheBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exchange := &fakeExchange{answer: issueEach(300)}
		c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) {
			cfg.BackoffFloor, cfg.BackoffCeiling = 30*time.Second, 30*time.Second
		})
		start := time.Now()
		if _, err := c.Token(context.Background(), testAudience, "read"); err != nil {
			t.Fatal(err)
		}
		exchange.set(outage)
		sleepUntil(start, 150*time.Second)
		if _, err := c.Token(context.Background(), testAudience, "read"); err != nil {
			t.Fatalf("the cached token inside its window: %v", err)
		}
		exchange.set(issueEach(300))
		c.Invalidate(testAudience, "read")
		token, err := c.Token(context.Background(), testAudience, "read")
		if err != nil {
			t.Fatalf("after Invalidate inside the backoff: %v", err)
		}
		if exchange.count() != 3 || token != tokenN(2) {
			t.Fatalf("exchanges %d, want 3 (the new token right after Invalidate)", exchange.count())
		}
	})
}
