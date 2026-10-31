package workloadtoken

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func signWith(header, payload string) string {
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload))
	return input + "." + enc.EncodeToString(ed25519.Sign(testIssuerPrivate, []byte(input)))
}

// TestEveryTokenCheckRefusesItsCase is the one table of the one token check: each row breaks exactly one check; the
// token is then refused, never cached and never sent, both when it arrives and when the host clock moves after it was
// cached. verifierRefuses rows are also refused by the real leeway-0 WorkloadVerifier at the same clock reading (the
// client checks what the verifier checks); the other rows are the client's own stricter bounds (the 30 s margin, the
// expires_in bound).
func TestEveryTokenCheckRefusesItsCase(t *testing.T) {
	base := time.Unix(2_000_000_000, 0)
	T := base.Unix()
	const goodHeader = `{"alg":"EdDSA","typ":"workload+jwt","kid":"test-key"}`
	q := func(s string) string { return `"` + s + `"` }
	payload := func(over map[string]string, drop ...string) string {
		members := map[string]string{
			"iss": q(testIssuerURL), "aud": q(testAudience), "sub": q("prn_AAAAAAAAAAAAAAAAAAAAAA"), "principal_type": q("workload"),
			"jti": q("000000000000000000000001"), "iat": fmt.Sprint(T), "nbf": fmt.Sprint(T), "exp": fmt.Sprint(T + 300),
		}
		for k, v := range over {
			members[k] = v
		}
		for _, k := range drop {
			delete(members, k)
		}
		var parts []string
		for _, k := range []string{"iss", "aud", "sub", "principal_type", "jti", "iat", "nbf", "exp", "scope"} {
			if v, ok := members[k]; ok {
				parts = append(parts, q(k)+":"+v)
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	good := signWith(goodHeader, payload(nil))
	gp := strings.Split(good, ".")
	enc := base64.RawURLEncoding
	type row struct {
		token           string
		expiresIn       int
		clockAfter      time.Duration // the clock moves by this after the token is cached (0: the check at arrival)
		verifierRefuses bool
	}
	rows := map[string]row{
		// header and signature
		"alg RS256":               {signWith(`{"alg":"RS256","typ":"workload+jwt","kid":"test-key"}`, payload(nil)), 300, 0, true},
		"alg none":                {signWith(`{"alg":"none","typ":"workload+jwt","kid":"test-key"}`, payload(nil)), 300, 0, true},
		"typ JWT":                 {signWith(`{"alg":"EdDSA","typ":"JWT","kid":"test-key"}`, payload(nil)), 300, 0, true},
		"no kid":                  {signWith(`{"alg":"EdDSA","typ":"workload+jwt"}`, payload(nil)), 300, 0, true},
		"empty signature":         {gp[0] + "." + gp[1] + ".", 300, 0, true},
		"short signature":         {gp[0] + "." + gp[1] + "." + enc.EncodeToString(make([]byte, 63)), 300, 0, true},
		"long signature":          {gp[0] + "." + gp[1] + "." + enc.EncodeToString(make([]byte, 65)), 300, 0, true},
		"padded signature":        {gp[0] + "." + gp[1] + "." + gp[2] + "==", 300, 0, true},
		"header not JSON":         {enc.EncodeToString([]byte("x")) + "." + gp[1] + "." + gp[2], 300, 0, true},
		"header kid case-folded":  {signWith(`{"alg":"EdDSA","typ":"workload+jwt","KID":"test-key"}`, payload(nil)), 300, 0, true},
		"header alg case-folded":  {signWith(`{"ALG":"EdDSA","typ":"workload+jwt","kid":"test-key"}`, payload(nil)), 300, 0, true},
		"header extra member":     {signWith(`{"alg":"EdDSA","typ":"workload+jwt","kid":"test-key","jku":"https://x.test/k"}`, payload(nil)), 300, 0, true},
		"header duplicate alg":    {signWith(`{"alg":"RS256","alg":"EdDSA","typ":"workload+jwt","kid":"test-key"}`, payload(nil)), 300, 0, false},
		"header kid with a space": {signWith(`{"alg":"EdDSA","typ":"workload+jwt","kid":"test key"}`, payload(nil)), 300, 0, true},
		// claims
		"wrong iss":                    {signWith(goodHeader, payload(map[string]string{"iss": q("https://other-issuer.test")})), 300, 0, true},
		"missing iss":                  {signWith(goodHeader, payload(nil, "iss")), 300, 0, true},
		"wrong aud":                    {signWith(goodHeader, payload(map[string]string{"aud": q("auth-service")})), 300, 0, true},
		"extra aud (array)":            {signWith(goodHeader, payload(map[string]string{"aud": `["` + testAudience + `","auth-service"]`})), 300, 0, true},
		"aud as one-array":             {signWith(goodHeader, payload(map[string]string{"aud": `["` + testAudience + `"]`})), 300, 0, true},
		"missing aud":                  {signWith(goodHeader, payload(nil, "aud")), 300, 0, true},
		"missing sub":                  {signWith(goodHeader, payload(nil, "sub")), 300, 0, true},
		"sub not prn_":                 {signWith(goodHeader, payload(map[string]string{"sub": q("user_1")})), 300, 0, true},
		"principal user":               {signWith(goodHeader, payload(map[string]string{"principal_type": q("user")})), 300, 0, true},
		"jti short":                    {signWith(goodHeader, payload(map[string]string{"jti": q("abc")})), 300, 0, true},
		"extra claim":                  {signWith(goodHeader, payload(map[string]string{"scope": q("read")})), 300, 0, true},
		"duplicate aud":                {signWith(goodHeader, strings.Replace(payload(nil), `"aud":`, `"aud":"auth-service","aud":`, 1)), 300, 0, true},
		"iat not integer":              {signWith(goodHeader, payload(map[string]string{"iat": fmt.Sprint(T) + ".5"})), 300, 0, true},
		"nbf before iat":               {signWith(goodHeader, payload(map[string]string{"nbf": fmt.Sprint(T - 1)})), 300, 0, true},
		"iat wraps exp-iat":            {signWith(goodHeader, payload(map[string]string{"iat": "-9223372036854775808", "nbf": fmt.Sprint(T), "exp": fmt.Sprint(T + 300)})), 300, 0, true},
		"exp at nbf":                   {signWith(goodHeader, payload(map[string]string{"exp": fmt.Sprint(T)})), 300, 0, true},
		"expires_in inside the margin": {good, 30, 0, false},
		"lifetime 301 s":               {signWith(goodHeader, payload(map[string]string{"exp": fmt.Sprint(T + 301)})), 300, 0, true},
		"exp passed":                   {signWith(goodHeader, payload(map[string]string{"iat": fmt.Sprint(T - 400), "nbf": fmt.Sprint(T - 400), "exp": fmt.Sprint(T - 100)})), 300, 0, true},
		"nbf in the future":            {signWith(goodHeader, payload(map[string]string{"iat": fmt.Sprint(T + 5), "nbf": fmt.Sprint(T + 5), "exp": fmt.Sprint(T + 305)})), 300, 0, true},
		// the window, after the token was cached
		"nbf in the future after a clock step back": {good, 300, -5 * time.Second, true},
		"exp passed after a clock step forward":     {good, 300, 301 * time.Second, true},
		"inside the 30 s margin":                    {good, 300, 271 * time.Second, false},
		"past the expires_in bound":                 {good, 100, 71 * time.Second, false},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			clock := base
			now := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}
			exchange := &fakeExchange{answer: func(n int, _ url.Values) (*http.Response, error) {
				if n > 0 {
					return outage(n, nil)
				}
				return issued(tc.token, tc.expiresIn), nil
			}}
			c := newTestClient(t, exchange, writeSubject(t, testSubject), func(cfg *Config) { cfg.Now = now })
			next := &captured{}
			rt, err := c.Transport(next, testTarget, testAudience)
			if err != nil {
				t.Fatal(err)
			}
			send := func() error {
				req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
				resp, err := rt.RoundTrip(req)
				if err == nil {
					_ = resp.Body.Close()
				}
				return err
			}
			if tc.clockAfter != 0 {
				if err := send(); err != nil {
					t.Fatalf("the token before the clock moved: %v", err)
				}
				mu.Lock()
				clock = clock.Add(tc.clockAfter)
				mu.Unlock()
			}
			sent := len(next.reqs)
			mustNoUsable(t, send(), nil)
			if len(next.reqs) != sent {
				t.Fatal("the refused token was sent")
			}
			key, _, _ := requestKey(testAudience, nil)
			c.mu.Lock()
			cached := c.slots[key] != nil && c.slots[key].entry != nil
			c.mu.Unlock()
			if cached {
				t.Fatal("the refused token stays cached")
			}
			_, verr := testVerifier(t).Verify(tc.token, now())
			if tc.verifierRefuses && verr == nil {
				t.Fatal("the verifier accepts this token: the row pins nothing")
			}
			if !tc.verifierRefuses && verr != nil {
				t.Fatalf("the verifier refuses a row marked as the client's own bound: %v", verr)
			}
		})
	}
	t.Run("the canonical token", func(t *testing.T) {
		c := newTestClient(t, &fakeExchange{answer: func(int, url.Values) (*http.Response, error) { return issued(good, 300), nil }},
			writeSubject(t, testSubject), func(cfg *Config) { cfg.Now = func() time.Time { return base } })
		if token, err := c.Token(context.Background(), testAudience); err != nil || token != good {
			t.Fatalf("err %v", err)
		}
		if _, err := testVerifier(t).Verify(good, base); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTokenClaimsUpperDateBound(t *testing.T) {
	const goodHeader = `{"alg":"EdDSA","typ":"workload+jwt","kid":"test-key"}`
	token := func(iat, nbf, exp int64) string {
		return signWith(goodHeader, fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"workload","jti":"000000000000000000000001","iat":%d,"nbf":%d,"exp":%d}`,
			testIssuerURL, testAudience, iat, nbf, exp))
	}
	if _, _, err := tokenClaims(token(253402300700, 253402300700, 253402300800), testIssuerURL, testAudience); err == nil {
		t.Fatal("exp beyond 253402300799 is accepted")
	}
	if _, _, err := tokenClaims(token(253402300500, 253402300500, 253402300799), testIssuerURL, testAudience); err != nil {
		t.Fatalf("exp at 253402300799 is refused: %v", err)
	}
}
