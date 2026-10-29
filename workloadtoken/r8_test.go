package workloadtoken

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

func signPayload(payload string) string {
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(`{"alg":"EdDSA","typ":"workload+jwt","kid":"test-key"}`)) + "." + enc.EncodeToString([]byte(payload))
	return input + "." + enc.EncodeToString(ed25519.Sign(testIssuerPrivate, []byte(input)))
}

// steppingClock returns base until its step-th reading, and base + shift from then on; it records the last reading.
type steppingClock struct {
	mu    sync.Mutex
	base  time.Time
	shift time.Duration
	step  int
	calls int
	last  time.Time
}

func (s *steppingClock) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	t := s.base
	if s.calls >= s.step {
		t = t.Add(s.shift)
	}
	s.last = t
	return t
}

// TestNoTokenIsReturnedOutsideItsWindowWhateverTheClockDoes: wherever in Token's work the host clock steps back before
// the token's nbf (or forward past its usable end), a token Token returns is inside its window at the clock reading
// taken when it is returned, and the strictest verifier accepts it there.
func TestNoTokenIsReturnedOutsideItsWindowWhateverTheClockDoes(t *testing.T) {
	base := time.Unix(2_000_000_000, 0)
	for _, shift := range []time.Duration{-5 * time.Second, 271 * time.Second} {
		for step := 1; step <= 8; step++ {
			t.Run(fmt.Sprintf("shift %v at reading %d", shift, step), func(t *testing.T) {
				clock := &steppingClock{base: base, shift: shift, step: step}
				exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
					at := base.Unix()
					return issued(signToken(n, at, at, at+300), 300), nil
				}}
				c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.Now = clock.now })
				for call := 0; call < 3; call++ {
					token, err := c.Token(context.Background(), testAudience)
					if err != nil {
						continue
					}
					clock.mu.Lock()
					at := clock.last
					clock.mu.Unlock()
					if _, verr := testVerifier(t).Verify(token, at); verr != nil {
						t.Fatalf("Token returned a token the verifier refuses at the clock reading of its return (%v): %v", at, verr)
					}
				}
			})
		}
	}
}

// TestARefreshFailureNeverFallsBackOutsideTheWindow: when a refresh fails, Token falls back to the cached token only if it
// is inside its window at the clock reading of that return, wherever in the failed refresh the clock steps back before
// the cached token's nbf.
func TestARefreshFailureNeverFallsBackOutsideTheWindow(t *testing.T) {
	base := time.Unix(2_000_000_000, 0)
	for step := 1; step <= 8; step++ {
		t.Run(fmt.Sprintf("step at reading %d", step), func(t *testing.T) {
			clock := &steppingClock{base: base, shift: 0, step: 1 << 30}
			exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
				if n > 0 {
					return outage(n, nil)
				}
				at := base.Unix()
				return issued(signToken(n, at, at, at+300), 300), nil
			}}
			c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.Now = clock.now })
			if _, err := c.Token(context.Background(), testAudience); err != nil {
				t.Fatal(err)
			}
			clock.mu.Lock()
			clock.base, clock.shift, clock.step, clock.calls = base.Add(150*time.Second), -155*time.Second, step, 0
			clock.mu.Unlock()
			token, err := c.Token(context.Background(), testAudience)
			if err != nil {
				return
			}
			clock.mu.Lock()
			at := clock.last
			clock.mu.Unlock()
			if _, verr := testVerifier(t).Verify(token, at); verr != nil {
				t.Fatalf("the fallback token is refused by the verifier at the clock reading of its return (%v): %v", at, verr)
			}
		})
	}
}
