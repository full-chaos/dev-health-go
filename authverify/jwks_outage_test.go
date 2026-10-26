package authverify

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type outageHarness struct {
	srv      *issuerServer
	clock    *fakeClock
	client   *JWKSClient
	requests *atomic.Int64
	known    signer
	stranger signer
	class    string
}

type outageMode struct {
	name  string
	apply func(t *testing.T, h *outageHarness)
}

var outageModes = []outageMode{
	{"status503", func(t *testing.T, h *outageHarness) {
		h.srv.set(func(s *issuerServer) { s.status = http.StatusServiceUnavailable })
	}},
	{"connectionClosed", func(t *testing.T, h *outageHarness) { h.srv.server.Close() }},
	{"hang", func(t *testing.T, h *outageHarness) {
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		h.srv.set(func(s *issuerServer) { s.gate = gate })
	}},
}

func newOutageHarness(t *testing.T, class string) *outageHarness {
	t.Helper()
	h := &outageHarness{known: newSigner(t), stranger: newSigner(t), class: class, requests: &atomic.Int64{}}
	h.srv = newIssuerServer(t, jwksFor("wl-a", h.known.pub))
	inner := h.srv.server.Config.Handler
	h.srv.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		inner.ServeHTTP(w, r)
	})
	h.clock = newClock()
	h.client = h.srv.client(t, h.clock, func(c *JWKSClientConfig) { c.Timeout = 300 * time.Millisecond })
	return h
}

func (h *outageHarness) verify(t *testing.T, s signer, kid string) error {
	t.Helper()
	now := h.clock.Now()
	if h.class == "workload" {
		payload := workloadPayload(now.Unix(), now.Unix(), now.Unix()+300)
		v, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: rtIssuer, Audience: "auth-service", Keys: h.client})
		if err != nil {
			t.Fatal(err)
		}
		_, err = v.Verify(s.token(workloadHeader(kid), payload), now)
		return err
	}
	header := fmt.Sprintf(`{"alg":"EdDSA","typ":"at+jwt","kid":%q}`, kid)
	payload := fmt.Sprintf(`{"iss":%q,"aud":"auth-service","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"human","sid":"ses_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000002","iat":%d,"nbf":%d,"exp":%d,"scope":"auth:session"}`,
		rtIssuer, now.Unix(), now.Unix(), now.Unix()+300)
	v, err := NewVerifier(VerifierConfig{Issuer: rtIssuer, Audience: "auth-service", Keys: h.client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(s.token(header, payload), now)
	return err
}

func wantUnavailable(t *testing.T, name string, err error) {
	t.Helper()
	var unavailable *UnavailableError
	if !errors.Is(err, ErrKeySourceUnavailable) || !errors.As(err, &unavailable) {
		t.Errorf("%s: got %v, want UnavailableError wrapping ErrKeySourceUnavailable", name, err)
		return
	}
	if ReasonOf(err) != "" || errors.Is(err, ErrRefused) {
		t.Errorf("%s: an outage must not be a refusal, got reason %q", name, ReasonOf(err))
	}
}

func wantUnknownKid(t *testing.T, name string, err error) {
	t.Helper()
	if ReasonOf(err) != ReasonUnknownKid || errors.Is(err, ErrKeySourceUnavailable) {
		t.Errorf("%s: got %v, want a refusal with reason unknown_kid", name, err)
	}
}

func forEachOutage(t *testing.T, f func(t *testing.T, h *outageHarness)) {
	for _, class := range []string{"user", "workload"} {
		for _, mode := range outageModes {
			t.Run(class+"/"+mode.name, func(t *testing.T) {
				h := newOutageHarness(t, class)
				mode.apply(t, h)
				f(t, h)
			})
		}
	}
}

func TestJWKSOutageIsUnavailableNotUnknownKid(t *testing.T) {
	forEachOutage(t, func(t *testing.T, h *outageHarness) {
		wantUnavailable(t, "published kid, nothing fetched yet", h.verify(t, h.known, "wl-a"))
	})
}

func TestJWKSOutageAfterWarmCacheKnownKeysOnlyInsideGrace(t *testing.T) {
	for _, class := range []string{"user", "workload"} {
		for _, mode := range outageModes {
			t.Run(class+"/"+mode.name, func(t *testing.T) {
				h := newOutageHarness(t, class)
				if err := h.verify(t, h.known, "wl-a"); err != nil {
					t.Fatalf("warm-up: %v", err)
				}
				mode.apply(t, h)
				h.clock.Advance(6 * time.Minute)
				if err := h.verify(t, h.known, "wl-a"); err != nil {
					t.Errorf("known kid inside TTL+grace: %v", err)
				}
				h.clock.Advance(time.Minute)
				wantUnavailable(t, "unknown kid during outage", h.verify(t, h.stranger, "wl-x"))
				h.clock.Advance(10 * time.Minute)
				wantUnavailable(t, "known kid past TTL+grace", h.verify(t, h.known, "wl-a"))
			})
		}
	}
}

func TestUnknownKidWithReachableJWKSStaysRefusal(t *testing.T) {
	for _, class := range []string{"user", "workload"} {
		t.Run(class, func(t *testing.T) {
			h := newOutageHarness(t, class)
			wantUnknownKid(t, "first call", h.verify(t, h.stranger, "wl-x"))
			if err := h.verify(t, h.known, "wl-a"); err != nil {
				t.Fatalf("published kid: %v", err)
			}
			h.clock.Advance(time.Second)
			wantUnknownKid(t, "unknown kid inside the floor after a good fetch", h.verify(t, h.stranger, "wl-x"))
			h.clock.Advance(time.Minute)
			wantUnknownKid(t, "unknown kid after the floor", h.verify(t, h.stranger, "wl-x"))
		})
	}
}

func TestOutageInsideFloorStaysUnavailableWithOneRefresh(t *testing.T) {
	forEachOutage(t, func(t *testing.T, h *outageHarness) {
		for i := 0; i < 5; i++ {
			wantUnavailable(t, "call inside the floor", h.verify(t, h.stranger, "wl-x"))
		}
		if got := h.requests.Load(); got > 1 {
			t.Errorf("%d requests inside the floor, want at most 1", got)
		}
	})
}

func TestUnknownKidCausesOneRefreshPerFloorOnReachableJWKS(t *testing.T) {
	h := newOutageHarness(t, "user")
	for i := 0; i < 5; i++ {
		wantUnknownKid(t, "unknown kid", h.verify(t, h.stranger, "wl-x"))
	}
	if got := h.requests.Load(); got != 2 {
		t.Errorf("%d requests for 5 unknown-kid calls, want 2 (discovery + jwks, one refresh)", got)
	}
}

func TestOutageRecoveryServesAgain(t *testing.T) {
	h := newOutageHarness(t, "workload")
	h.srv.set(func(s *issuerServer) { s.status = http.StatusServiceUnavailable })
	wantUnavailable(t, "outage", h.verify(t, h.known, "wl-a"))
	h.srv.set(func(s *issuerServer) { s.status = http.StatusOK })
	h.clock.Advance(time.Minute)
	if err := h.verify(t, h.known, "wl-a"); err != nil {
		t.Errorf("recovery: %v", err)
	}
	wantUnknownKid(t, "unknown kid after recovery", h.verify(t, h.stranger, "wl-x"))
}

func TestUnavailableErrorCarriesNoTokenBytes(t *testing.T) {
	h := newOutageHarness(t, "user")
	h.srv.set(func(s *issuerServer) { s.status = http.StatusServiceUnavailable })
	err := h.verify(t, h.known, "wl-a")
	if err == nil || strings.Contains(err.Error(), "wl-a") || strings.Contains(err.Error(), "eyJ") {
		t.Errorf("error text leaks: %v", err)
	}
}
