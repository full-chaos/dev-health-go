package workloadtoken

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

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
