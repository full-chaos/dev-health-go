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

// TestTheClaimsTheVerifierChecksAreCheckedBeforeUse: the client refuses (never caches, never sends) a token its verifier
// would refuse for a reason the client can know: another audience, an audience array, a missing or extra claim, a
// principal type other than workload, an iss, sub or jti outside the workload-token.v1 shapes. For each case the strict
// verifier refuses the same token; the iss VALUE is the verifier's configuration and is not checked by the client.
func TestTheClaimsTheVerifierChecksAreCheckedBeforeUse(t *testing.T) {
	now := time.Now().Unix()
	claims := func(iss, aud, sub, ptype, jti string, extra string) string {
		return fmt.Sprintf(`{"iss":%s,"aud":%s,"sub":%s,"principal_type":%s,"jti":%s,"iat":%d,"nbf":%d,"exp":%d%s}`,
			iss, aud, sub, ptype, jti, now, now, now+300, extra)
	}
	q := func(s string) string { return `"` + s + `"` }
	iss, aud, sub, pt, jti := q(testIssuerURL), q(testAudience), q("prn_AAAAAAAAAAAAAAAAAAAAAA"), q("workload"), q("000000000000000000000001")
	refused := map[string]string{
		"another audience":   claims(iss, q("auth-service"), sub, pt, jti, ""),
		"audience array":     claims(iss, `["`+testAudience+`"]`, sub, pt, jti, ""),
		"audience empty":     claims(iss, q(""), sub, pt, jti, ""),
		"extra claim":        claims(iss, aud, sub, pt, jti, `,"scope":"read"`),
		"principal user":     claims(iss, aud, sub, q("user"), jti, ""),
		"iss http":           claims(q("http://auth-service.test"), aud, sub, pt, jti, ""),
		"iss number":         claims("1", aud, sub, pt, jti, ""),
		"sub not prn_":       claims(iss, aud, q("user_1"), pt, jti, ""),
		"jti short":          claims(iss, aud, sub, pt, q("abc"), ""),
		"jti bad character":  claims(iss, aud, sub, pt, q("00000000000000000000000!"), ""),
		"no aud":             fmt.Sprintf(`{"iss":%s,"sub":%s,"principal_type":%s,"jti":%s,"iat":%d,"nbf":%d,"exp":%d}`, iss, sub, pt, jti, now, now, now+300),
		"no principal_type":  fmt.Sprintf(`{"iss":%s,"aud":%s,"sub":%s,"jti":%s,"iat":%d,"nbf":%d,"exp":%d}`, iss, aud, sub, jti, now, now, now+300),
		"duplicate audience": fmt.Sprintf(`{"iss":%s,"aud":%s,"aud":%s,"sub":%s,"principal_type":%s,"jti":%s,"iat":%d,"nbf":%d,"exp":%d}`, iss, q("auth-service"), aud, sub, pt, jti, now, now, now+300),
	}
	for name, payload := range refused {
		t.Run(name, func(t *testing.T) {
			token := signPayload(payload)
			if _, err := testVerifier(t).Verify(token, time.Now()); err == nil {
				t.Fatalf("the verifier accepts this token: the case pins nothing")
			}
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
				t.Fatal("the token was sent")
			}
		})
	}
	t.Run("the canonical token", func(t *testing.T) {
		token := signPayload(claims(iss, aud, sub, pt, jti, ""))
		if _, err := testVerifier(t).Verify(token, time.Now()); err != nil {
			t.Fatal(err)
		}
		c := newTestClient(t, &fakeExchange{answer: answerWith(200, "application/json",
			`{"access_token":"`+token+`","token_type":"Bearer","issued_token_type":"`+TokenTypeJWT+`","expires_in":300}`)}, writeSubject(t, testSubject))
		if got, err := c.Token(context.Background(), testAudience); err != nil || got != token {
			t.Fatalf("err %v", err)
		}
	})
}
