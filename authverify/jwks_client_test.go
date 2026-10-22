package authverify

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type issuerServer struct {
	server    *httptest.Server
	mu        sync.Mutex
	jwks      []byte
	discovery func(issuer string) []byte
	status    int
	redirect  bool
	gate      chan struct{}
	discHits  atomic.Int64
	jwksHits  atomic.Int64
}

func newIssuerServer(t *testing.T, jwks []byte) *issuerServer {
	s := &issuerServer{jwks: jwks, status: http.StatusOK}
	s.discovery = func(issuer string) []byte {
		doc, _ := json.Marshal(map[string]any{
			"issuer": issuer, "jwks_uri": issuer + jwksPath, "id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
		return doc
	}
	s.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		status, redirect, jwks, discovery, gate := s.status, s.redirect, s.jwks, s.discovery, s.gate
		s.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if redirect {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		switch r.URL.Path {
		case discoveryPath:
			s.discHits.Add(1)
			_, _ = w.Write(discovery(s.server.URL))
		case jwksPath:
			s.jwksHits.Add(1)
			_, _ = w.Write(jwks)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *issuerServer) set(f func(*issuerServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *issuerServer) client(t *testing.T, clock *fakeClock, mutate func(*JWKSClientConfig)) *JWKSClient {
	cfg := JWKSClientConfig{Issuer: s.server.URL, HTTPClient: s.server.Client(), Now: clock.Now}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewJWKSClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func corpusJWKS(t testing.TB) []byte {
	raw, err := os.ReadFile(filepath.Join(corpusDir, "jwks.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1760000000, 0)} }

const knownKid = "auth-example-2026-10"

func TestClientServesKnownKidAndCaches(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	clock := newClock()
	c := s.client(t, clock, nil)
	want, _ := StaticKeys(mustKeys(t))[knownKid]
	for i := 0; i < 3; i++ {
		key, ok := c.PublicKey(knownKid)
		if !ok || !key.Equal(want) {
			t.Fatal("known kid not served")
		}
	}
	if s.discHits.Load() != 1 || s.jwksHits.Load() != 1 {
		t.Fatalf("fetches discovery=%d jwks=%d, want 1 each", s.discHits.Load(), s.jwksHits.Load())
	}
}

func mustKeys(t testing.TB) map[string]ed25519.PublicKey {
	keys, err := NewEd25519JWKSVerifier(filepath.Join(corpusDir, "jwks.json")).Keys()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestClientVerifiesCorpusTokensThroughTheClient(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	m := loadManifest(t)
	c := s.client(t, newClock(), nil)
	v, err := NewVerifier(VerifierConfig{Issuer: m.Issuer, Audience: "dev-health-api", Keys: c})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid-api", "valid-signed-by-overlap-key"} {
		var vec corpusVector
		readJSON(t, filepath.Join(corpusDir, "vectors", name+".json"), &vec)
		if _, err := v.Verify(vec.compact(), time.Unix(1760000300, 0)); err != nil {
			t.Errorf("%s: %v", name, ReasonOf(err))
		}
	}
	var unknown corpusVector
	readJSON(t, filepath.Join(corpusDir, "vectors", "invalid-kid-unknown.json"), &unknown)
	if _, err := v.Verify(unknown.compact(), time.Unix(1760000300, 0)); ReasonOf(err) != ReasonUnknownKid {
		t.Errorf("unknown kid: %q", ReasonOf(err))
	}
}

func TestUnknownKidSharesOneRefresh(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	clock := newClock()
	c := s.client(t, clock, nil)
	if _, ok := c.PublicKey(knownKid); !ok {
		t.Fatal("warm-up failed")
	}
	clock.Advance(time.Minute)
	gate := make(chan struct{})
	s.set(func(s *issuerServer) { s.gate = gate })
	const callers = 32
	var wg sync.WaitGroup
	var found atomic.Int64
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := c.PublicKey("no-such-kid"); ok {
				found.Add(1)
			}
		}()
	}
	close(start)
	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()
	if found.Load() != 0 {
		t.Fatal("an unknown kid was served")
	}
	if got := s.jwksHits.Load(); got != 2 {
		t.Fatalf("jwks fetches %d, want 2 (warm-up plus one shared refresh)", got)
	}
}

func TestRefreshFloorBoundsUnknownKidFlood(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	clock := newClock()
	c := s.client(t, clock, nil)
	c.PublicKey(knownKid)
	clock.Advance(31 * time.Second)
	for i := 0; i < 50; i++ {
		if _, ok := c.PublicKey(fmt.Sprintf("flood-%d", i)); ok {
			t.Fatal("unknown kid served")
		}
	}
	if got := s.jwksHits.Load(); got != 2 {
		t.Fatalf("fetches %d inside one floor, want 2", got)
	}
	clock.Advance(31 * time.Second)
	c.PublicKey("flood-again")
	if got := s.jwksHits.Load(); got != 3 {
		t.Fatalf("fetches %d after the floor, want 3", got)
	}
	if err := c.Refresh(nil); !errors.Is(err, ErrRefreshTooSoon) {
		t.Fatalf("Refresh inside the floor: %v", err)
	}
}

func TestRefreshFloorHoldsWithAZeroClock(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	c := s.client(t, &fakeClock{}, nil)
	for i := 0; i < 5; i++ {
		if _, ok := c.PublicKey(fmt.Sprintf("zero-%d", i)); ok {
			t.Fatal("unknown kid served")
		}
	}
	if s.discHits.Load() != 1 || s.jwksHits.Load() != 1 {
		t.Fatalf("fetches discovery=%d jwks=%d inside one floor, want 1 each", s.discHits.Load(), s.jwksHits.Load())
	}
}

func TestOutageKeepsKnownKeysOnlyInsideGrace(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	clock := newClock()
	c := s.client(t, clock, nil)
	c.PublicKey(knownKid)
	s.set(func(s *issuerServer) { s.status = http.StatusInternalServerError })
	clock.Advance(6 * time.Minute)
	if _, ok := c.PublicKey(knownKid); !ok {
		t.Fatal("known key lost inside the outage grace")
	}
	clock.Advance(10 * time.Minute)
	if _, ok := c.PublicKey(knownKid); ok {
		t.Fatal("known key served past the outage grace")
	}
	s.set(func(s *issuerServer) { s.status = http.StatusOK })
	clock.Advance(time.Minute)
	if _, ok := c.PublicKey(knownKid); !ok {
		t.Fatal("no recovery after the outage")
	}
}

func TestInvalidRefreshKeepsCacheAndRetiredKidLeaves(t *testing.T) {
	s := newIssuerServer(t, corpusJWKS(t))
	clock := newClock()
	c := s.client(t, clock, nil)
	c.PublicKey(knownKid)
	s.set(func(s *issuerServer) { s.jwks = []byte(`{"keys":[]}`) })
	clock.Advance(time.Minute)
	if _, ok := c.PublicKey("other"); ok {
		t.Fatal("unknown kid served")
	}
	if _, ok := c.PublicKey(knownKid); !ok {
		t.Fatal("an invalid document replaced the cache")
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(corpusJWKS(t), &doc); err != nil {
		t.Fatal(err)
	}
	onlySecond, _ := json.Marshal(map[string]any{"keys": doc.Keys[1:]})
	s.set(func(s *issuerServer) { s.jwks = onlySecond })
	clock.Advance(time.Minute)
	if _, ok := c.PublicKey(knownKid); !ok {
		t.Fatal("a removed kid left before the cache TTL ended")
	}
	clock.Advance(5 * time.Minute)
	if _, ok := c.PublicKey(knownKid); ok {
		t.Fatal("a retired kid stayed after a successful refresh")
	}
	if _, ok := c.PublicKey("auth-example-2026-11"); !ok {
		t.Fatal("the remaining kid was lost")
	}
}

func TestWireRefusals(t *testing.T) {
	cases := map[string]func(*issuerServer){
		"non-200":       func(s *issuerServer) { s.status = http.StatusNotFound },
		"redirect":      func(s *issuerServer) { s.redirect = true },
		"oversize jwks": func(s *issuerServer) { s.jwks = []byte(`{"keys":[],"pad":"` + strings.Repeat("a", 70<<10) + `"}`) },
		"discovery issuer": func(s *issuerServer) {
			s.discovery = func(string) []byte { return discoveryDoc("https://other.example.test", nil) }
		},
		"discovery uri": func(s *issuerServer) {
			s.discovery = func(i string) []byte { return discoveryDoc(i, map[string]any{"jwks_uri": i + "/keys.json"}) }
		},
		"discovery alg": func(s *issuerServer) {
			s.discovery = func(i string) []byte {
				return discoveryDoc(i, map[string]any{"id_token_signing_alg_values_supported": []string{"EdDSA", "none"}})
			}
		},
		"discovery extra": func(s *issuerServer) {
			s.discovery = func(i string) []byte { return discoveryDoc(i, map[string]any{"token_endpoint": i + "/token"}) }
		},
		"discovery duplicate": func(s *issuerServer) {
			s.discovery = func(i string) []byte {
				return []byte(`{"issuer":"` + i + `","issuer":"` + i + `","jwks_uri":"` + i + jwksPath + `","id_token_signing_alg_values_supported":["EdDSA"]}`)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := newIssuerServer(t, corpusJWKS(t))
			s.set(mutate)
			c := s.client(t, newClock(), nil)
			if _, ok := c.PublicKey(knownKid); ok {
				t.Fatal("key served")
			}
			err := c.Refresh(nil)
			if err != nil && (err.Error() != ErrJWKSUnavailable.Error() && !errors.Is(err, ErrRefreshTooSoon)) {
				t.Fatalf("error text %q", err)
			}
		})
	}
	t.Run("too many keys", func(t *testing.T) {
		s := newIssuerServer(t, corpusJWKS(t))
		c := s.client(t, newClock(), func(c *JWKSClientConfig) { c.MaxKeys = 1 })
		if _, ok := c.PublicKey(knownKid); ok {
			t.Fatal("key served from an over-limit document")
		}
	})
}

func discoveryDoc(issuer string, override map[string]any) []byte {
	doc := map[string]any{"issuer": issuer, "jwks_uri": issuer + jwksPath, "id_token_signing_alg_values_supported": []string{"EdDSA"}}
	for k, v := range override {
		doc[k] = v
	}
	raw, _ := json.Marshal(doc)
	return raw
}

func TestJWKSClientConfigBounds(t *testing.T) {
	good := JWKSClientConfig{Issuer: "https://auth.example.test"}
	if _, err := NewJWKSClient(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*JWKSClientConfig){
		"http issuer":    func(c *JWKSClientConfig) { c.Issuer = "http://auth.example.test" },
		"trailing":       func(c *JWKSClientConfig) { c.Issuer = "https://auth.example.test/" },
		"timeout":        func(c *JWKSClientConfig) { c.Timeout = time.Minute },
		"ttl":            func(c *JWKSClientConfig) { c.CacheTTL = 2 * time.Hour },
		"floor over ttl": func(c *JWKSClientConfig) { c.RefreshFloor = 10 * time.Minute },
		"grace":          func(c *JWKSClientConfig) { c.OutageGrace = 2 * time.Hour },
		"negative":       func(c *JWKSClientConfig) { c.MaxKeys = -1 },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewJWKSClient(cfg); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

type goldenManifest struct {
	Accept         []struct{ File string } `json:"accept"`
	Reject         []struct{ File string } `json:"reject"`
	RejectByClient []struct{ File string } `json:"reject_by_client"`
}

func loadGolden(t testing.TB, set string) goldenManifest {
	var m goldenManifest
	readJSON(t, filepath.Join("testdata", set, "manifest.json"), &m)
	return m
}

func TestGoldenDiscovery(t *testing.T) {
	m := loadGolden(t, "discovery")
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", "discovery", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	pinFor := func(name string) string {
		if name == "valid-port-and-path-prefix.json" {
			return "https://auth.example.test:8443/dev-health"
		}
		return "https://auth.example.test"
	}
	if len(m.Accept) == 0 || len(m.Reject) == 0 || len(m.RejectByClient) == 0 {
		t.Fatal("empty golden manifest")
	}
	for _, e := range m.Accept {
		c, _ := NewJWKSClient(JWKSClientConfig{Issuer: pinFor(e.File)})
		if !c.discoveryValid(read(e.File)) {
			t.Errorf("%s refused", e.File)
		}
	}
	for _, list := range [][]struct{ File string }{m.Reject, m.RejectByClient} {
		for _, e := range list {
			c, _ := NewJWKSClient(JWKSClientConfig{Issuer: pinFor(e.File)})
			if c.discoveryValid(read(e.File)) {
				t.Errorf("%s accepted", e.File)
			}
		}
	}
}

func TestGoldenJWKS(t *testing.T) {
	m := loadGolden(t, "jwks")
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", "jwks", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, e := range m.Accept {
		if _, err := parseJWKS(read(e.File)); err != nil {
			t.Errorf("%s refused", e.File)
		}
	}
	for _, list := range [][]struct{ File string }{m.Reject, m.RejectByClient} {
		for _, e := range list {
			if _, err := parseJWKS(read(e.File)); err == nil {
				t.Errorf("%s accepted", e.File)
			}
		}
	}
}

func FuzzDiscoveryAndJWKS(f *testing.F) {
	for _, set := range []string{"discovery", "jwks"} {
		entries, _ := os.ReadDir(filepath.Join("testdata", set))
		for _, e := range entries {
			if raw, err := os.ReadFile(filepath.Join("testdata", set, e.Name())); err == nil {
				f.Add(raw)
			}
		}
	}
	c, err := NewJWKSClient(JWKSClientConfig{Issuer: "https://auth.example.test"})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if c.discoveryValid(raw) {
			var doc struct {
				Issuer string `json:"issuer"`
				URI    string `json:"jwks_uri"`
			}
			if json.Unmarshal(raw, &doc) != nil || doc.Issuer != "https://auth.example.test" || doc.URI != doc.Issuer+jwksPath {
				t.Fatal("accepted a discovery document that breaks the pin")
			}
		}
		if keys, err := parseJWKS(raw); err == nil {
			for _, key := range keys {
				if len(key) != ed25519.PublicKeySize {
					t.Fatal("accepted a wrong-size key")
				}
			}
		}
	})
}
