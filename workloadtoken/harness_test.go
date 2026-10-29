package workloadtoken

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/authverify"
)

const (
	testBase     = "https://auth-service.test:8095"
	testAudience = "dev-health-api"
	testSubject  = "eyJhbGciOiJSUzI1NiJ9.c3ViamVjdC12MQ.c2lnbmF0dXJl"
	testTarget   = "https://api.test"
)

// recorded is one request the fake exchange received.
type recorded struct {
	method string
	url    string
	header http.Header
	form   url.Values
	raw    string
}

// fakeExchange is an in-process exchange: the Client's Transport, so the tests
// open no socket. answer decides the reply to the nth call (0-based); when
// hold is set, a call waits for it (or for its request context) first.
type fakeExchange struct {
	mu     sync.Mutex
	calls  []recorded
	answer func(n int, form url.Values) (*http.Response, error)
	hold   chan struct{}
}

func (f *fakeExchange) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(raw))
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, recorded{method: req.Method, url: req.URL.String(), header: req.Header.Clone(), form: form, raw: string(raw)})
	answer, hold := f.answer, f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return answer(n, form)
}

func (f *fakeExchange) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExchange) call(t *testing.T, n int) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.calls) {
		t.Fatalf("%d exchange call(s), want call %d", len(f.calls), n)
	}
	return f.calls[n]
}

func (f *fakeExchange) set(answer func(n int, form url.Values) (*http.Response, error)) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

// testIssuer signs the test workload tokens; testVerifier is the strictest legal verifier of them (leeway 0), the oracle
// for "a token the client sends is one the verifier accepts".
const testIssuerURL = "https://auth-service.test"

var (
	testIssuerPublic, testIssuerPrivate = mustKey()
	mintedMu                            sync.Mutex
	minted                              = map[int]string{}
)

func mustKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub, priv
}

// signToken signs a workload-token.v1 token for testAudience with jti n and the given dates.
func signToken(n int, iat, nbf, exp int64) string {
	return signTokenFor(testAudience, n, iat, nbf, exp)
}

// signTokenFor is signToken for another audience.
func signTokenFor(audience string, n int, iat, nbf, exp int64) string {
	enc := base64.RawURLEncoding
	header := `{"alg":"EdDSA","typ":"workload+jwt","kid":"test-key"}`
	payload := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"workload","jti":"%024d","iat":%d,"nbf":%d,"exp":%d}`,
		testIssuerURL, audience, n, iat, nbf, exp)
	input := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload))
	return input + "." + enc.EncodeToString(ed25519.Sign(testIssuerPrivate, []byte(input)))
}

// mintToken signs token n issued now (on the clock of the caller: the bubble clock inside synctest) for lifetime
// seconds, and records it as tokenN(n).
func mintToken(n int, lifetime int) string { return mintTokenFor(testAudience, n, lifetime) }

// mintTokenFor is mintToken for another audience.
func mintTokenFor(audience string, n int, lifetime int) string {
	now := time.Now().Unix()
	token := signTokenFor(audience, n, now, now, now+int64(lifetime))
	mintedMu.Lock()
	minted[n] = token
	mintedMu.Unlock()
	return token
}

// tokenN is the token last minted with jti n.
func tokenN(n int) string {
	mintedMu.Lock()
	defer mintedMu.Unlock()
	return minted[n]
}

func testVerifier(t *testing.T) *authverify.WorkloadVerifier {
	t.Helper()
	v, err := authverify.NewWorkloadVerifier(authverify.WorkloadVerifierConfig{
		Issuer: testIssuerURL, Audience: testAudience, Keys: authverify.StaticKeys{"test-key": testIssuerPublic},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func issued(token string, expiresIn int) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"access_token": token, "issued_token_type": TokenTypeJWT, "token_type": "Bearer", "expires_in": expiresIn,
	})
	return jsonResponse(http.StatusOK, string(body))
}

// issueEach answers call n with a token minted now, for the audience asked, for the given lifetime (recorded as tokenN(n)).
func issueEach(expiresIn int) func(int, url.Values) (*http.Response, error) {
	return func(n int, form url.Values) (*http.Response, error) {
		audience := form.Get("audience")
		if audience == "" {
			audience = testAudience
		}
		return issued(mintTokenFor(audience, n, expiresIn), expiresIn), nil
	}
}

func outage(n int, _ url.Values) (*http.Response, error) {
	return jsonResponse(http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`), nil
}

func writeSubject(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestClient(t *testing.T, exchange http.RoundTripper, subjectPath string, mutate ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{ExchangeURL: testBase, Issuer: testIssuerURL, Subject: KubernetesTokenFile(subjectPath), Transport: exchange}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// resource is the server behind Client.RoundTripper: it records every request
// that reached it.
type resource struct {
	mu   sync.Mutex
	auth []string
}

func (r *resource) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (r *resource) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}

func get(t *testing.T, rt http.RoundTripper) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://api.test/v1/thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// sleepUntil advances the bubble clock to start+offset.
func sleepUntil(start time.Time, offset time.Duration) {
	time.Sleep(time.Until(start.Add(offset)))
}

func mustNoUsable(t *testing.T, err error, class error) {
	t.Helper()
	if !errors.Is(err, ErrNoUsableToken) {
		t.Fatalf("err = %v, want ErrNoUsableToken", err)
	}
	if class != nil && !errors.Is(err, class) {
		t.Fatalf("err = %v, want %v", err, class)
	}
}
