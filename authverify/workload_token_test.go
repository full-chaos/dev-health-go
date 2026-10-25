package authverify

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const workloadCorpusDir = "testdata/workload-token"

func loadWorkloadManifest(t testing.TB) corpusManifest {
	t.Helper()
	var m corpusManifest
	readJSON(t, filepath.Join(workloadCorpusDir, "manifest.json"), &m)
	var removed struct {
		Kids []string `json:"removed_kids"`
	}
	readJSON(t, filepath.Join(workloadCorpusDir, "manifest.json"), &removed)
	m.RemovedKids = removed.Kids
	return m
}

func workloadCorpusKeys(t testing.TB, m corpusManifest, withRemoved bool) StaticKeys {
	t.Helper()
	keys, err := NewEd25519JWKSVerifier(filepath.Join(workloadCorpusDir, "jwks.json")).Keys()
	if err != nil {
		t.Fatal(err)
	}
	set := StaticKeys(keys)
	if withRemoved {
		for _, k := range m.RemovedKeys {
			raw, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil {
				t.Fatal(err)
			}
			set[k.Kid] = ed25519.PublicKey(raw)
		}
	}
	return set
}

func workloadCorpusVerifier(t testing.TB, m corpusManifest, row corpusRow) *WorkloadVerifier {
	t.Helper()
	v, err := NewWorkloadVerifier(WorkloadVerifierConfig{
		Issuer: m.Issuer, Audience: row.Audience, DeniedKids: m.RemovedKids,
		Keys: workloadCorpusKeys(t, m, row.KeySet == "published_and_removed"), Leeway: time.Duration(m.LeewaySeconds) * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func readWorkloadVector(t testing.TB, name string) corpusVector {
	t.Helper()
	var vec corpusVector
	readJSON(t, filepath.Join(workloadCorpusDir, "vectors", name), &vec)
	return vec
}

type signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t testing.TB) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv: priv, pub: pub}
}

func (s signer) token(header, payload string) string {
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload))
	return input + "." + enc.EncodeToString(ed25519.Sign(s.priv, []byte(input)))
}

const rtIssuer = "https://auth.example.test"

func workloadHeader(kid string) string {
	return fmt.Sprintf(`{"alg":"EdDSA","typ":"workload+jwt","kid":%q}`, kid)
}

func workloadPayload(iat, nbf, exp int64) string {
	return fmt.Sprintf(`{"iss":%q,"aud":"auth-service","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"workload","jti":"000000000000000000000001","iat":%d,"nbf":%d,"exp":%d}`, rtIssuer, iat, nbf, exp)
}

func rtVerifier(t testing.TB, keys KeySource, leeway time.Duration, denied ...string) *WorkloadVerifier {
	t.Helper()
	v, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: rtIssuer, Audience: "auth-service", Keys: keys, DeniedKids: denied, Leeway: leeway})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWorkloadCorpusSourceIsPinned(t *testing.T) {
	file, err := os.Open("testdata/workload-token.SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pinned := map[string]string{}
	var paths []string
	sawCommit := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "ops_commit "); ok {
			if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(value) {
				t.Fatal("SOURCE ops_commit is not a full commit sha")
			}
			sawCommit = true
			continue
		}
		if value, ok := strings.CutPrefix(line, "ops_path "); ok {
			paths = append(paths, value)
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatal("bad SOURCE line")
		}
		pinned[name] = sum
	}
	if !sawCommit || strings.Join(paths, ",") != "contracts/auth/v1/golden/workload-token" {
		t.Fatal("SOURCE lacks ops_commit or the ops_path line")
	}
	var onDisk []string
	err = filepath.Walk(workloadCorpusDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel("testdata", path)
			onDisk = append(onDisk, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(onDisk)
	if len(onDisk) != len(pinned) || len(onDisk) < 64+3 {
		t.Fatalf("corpus holds %d files, SOURCE pins %d", len(onDisk), len(pinned))
	}
	for _, name := range onDisk {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if pinned[name] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s differs from its pinned sha256", name)
		}
	}
}

func TestWorkloadCorpusDecisions(t *testing.T) {
	m := loadWorkloadManifest(t)
	if len(m.Vectors) != 64 {
		t.Fatalf("manifest holds %d rows, want 64", len(m.Vectors))
	}
	for _, row := range m.Vectors {
		t.Run(strings.TrimSuffix(row.File, ".json"), func(t *testing.T) {
			vec := readWorkloadVector(t, row.File)
			claims, err := workloadCorpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0))
			if row.Expect == "accept" {
				if err != nil || claims == nil {
					t.Fatalf("want accept, got %v", ReasonOf(err))
				}
				return
			}
			if claims != nil || ReasonOf(err) != Reason(row.Reason) {
				t.Fatalf("want reject %s, got %q", row.Reason, ReasonOf(err))
			}
		})
	}
}

func TestWorkloadVerifierConfigBounds(t *testing.T) {
	keys := StaticKeys{}
	good := WorkloadVerifierConfig{Issuer: rtIssuer, Audience: "auth-service", Keys: keys}
	if _, err := NewWorkloadVerifier(good); err != nil {
		t.Fatal(err)
	}
	otherService := good
	otherService.Audience = "dev-health-api"
	if _, err := NewWorkloadVerifier(otherService); err != nil {
		t.Fatalf("a verifier for another token audience refused: %v", err)
	}
	atMax := good
	atMax.Leeway = MaxWorkloadLeeway
	if _, err := NewWorkloadVerifier(atMax); err != nil {
		t.Fatalf("leeway at the maximum refused: %v", err)
	}
	for name, mutate := range map[string]func(*WorkloadVerifierConfig){
		"no keys":           func(c *WorkloadVerifierConfig) { c.Keys = nil },
		"http issuer":       func(c *WorkloadVerifierConfig) { c.Issuer = "http://auth.example.test" },
		"envelope audience": func(c *WorkloadVerifierConfig) { c.Audience = "query-api" },
		"unknown audience":  func(c *WorkloadVerifierConfig) { c.Audience = "other" },
		"empty audience":    func(c *WorkloadVerifierConfig) { c.Audience = "" },
		"leeway over 30 s":  func(c *WorkloadVerifierConfig) { c.Leeway = MaxWorkloadLeeway + time.Second },
		"leeway 60 s":       func(c *WorkloadVerifierConfig) { c.Leeway = MaxLeeway },
		"leeway frac":       func(c *WorkloadVerifierConfig) { c.Leeway = 500 * time.Millisecond },
		"leeway negative":   func(c *WorkloadVerifierConfig) { c.Leeway = -time.Second },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewWorkloadVerifier(cfg); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

func TestWorkloadLeewayShiftsClockEdges(t *testing.T) {
	m := loadWorkloadManifest(t)
	vec := readWorkloadVector(t, "valid-auth-service.json")
	v, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: m.Issuer, Audience: "auth-service", Keys: workloadCorpusKeys(t, m, false), Leeway: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[int64]Reason{1759999994: ReasonNotYetValid, 1759999995: "", 1760000304: "", 1760000305: ReasonExpired} {
		if _, err := v.Verify(vec.compact(), time.Unix(at, 0)); ReasonOf(err) != want {
			t.Errorf("at %d: got %q want %q", at, ReasonOf(err), want)
		}
	}
}

func TestWorkloadLifetimeCeilingEdge(t *testing.T) {
	s := newSigner(t)
	keys := StaticKeys{"wl-a": s.pub}
	const iat = 1760000000
	for _, leeway := range []time.Duration{0, MaxWorkloadLeeway} {
		v := rtVerifier(t, keys, leeway)
		at300 := s.token(workloadHeader("wl-a"), workloadPayload(iat, iat, iat+300))
		if _, err := v.Verify(at300, time.Unix(iat, 0)); err != nil {
			t.Errorf("leeway %v: 300 s lifetime refused: %v", leeway, ReasonOf(err))
		}
		at301 := s.token(workloadHeader("wl-a"), workloadPayload(iat, iat, iat+301))
		for _, now := range []int64{iat, iat + 150, iat + 300} {
			if _, err := v.Verify(at301, time.Unix(now, 0)); ReasonOf(err) != ReasonContract {
				t.Errorf("leeway %v now %d: 301 s lifetime gave %q", leeway, now, ReasonOf(err))
			}
		}
		pastExp := time.Unix(iat+300+int64(leeway/time.Second), 0)
		if _, err := v.Verify(at300, pastExp); ReasonOf(err) != ReasonExpired {
			t.Errorf("leeway %v: token past exp+leeway gave %q", leeway, ReasonOf(err))
		}
	}
}

func userShapedToken(s signer, kid string) string {
	header := fmt.Sprintf(`{"alg":"EdDSA","typ":"at+jwt","kid":%q}`, kid)
	payload := fmt.Sprintf(`{"iss":%q,"aud":"auth-service","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"human","sid":"ses_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000002","iat":1760000000,"nbf":1760000000,"exp":1760000300,"scope":"auth:session"}`, rtIssuer)
	return s.token(header, payload)
}

func TestUserTokenRefusedByWorkloadVerifier(t *testing.T) {
	s := newSigner(t)
	keys := StaticKeys{"wl-a": s.pub}
	now := time.Unix(1760000100, 0)
	user := userShapedToken(s, "wl-a")
	userVerifier, err := NewVerifier(VerifierConfig{Issuer: rtIssuer, Audience: "auth-service", Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := userVerifier.Verify(user, now); err != nil {
		t.Fatalf("control: the user token is not valid for the user verifier: %v", ReasonOf(err))
	}
	if _, err := rtVerifier(t, keys, 0).Verify(user, now); ReasonOf(err) != ReasonContract {
		t.Fatalf("user token at the workload verifier gave %q", ReasonOf(err))
	}
}

func TestWorkloadTokenRefusedByUserVerifier(t *testing.T) {
	s := newSigner(t)
	keys := StaticKeys{"wl-a": s.pub}
	now := time.Unix(1760000100, 0)
	workload := s.token(workloadHeader("wl-a"), workloadPayload(1760000000, 1760000000, 1760000300))
	if _, err := rtVerifier(t, keys, 0).Verify(workload, now); err != nil {
		t.Fatalf("control: the workload token is not valid for the workload verifier: %v", ReasonOf(err))
	}
	for _, audience := range []string{"auth-service", "dev-health-api"} {
		v, err := NewVerifier(VerifierConfig{Issuer: rtIssuer, Audience: audience, Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(workload, now); ReasonOf(err) != ReasonContract {
			t.Errorf("workload token at the user verifier (%s) gave %q", audience, ReasonOf(err))
		}
	}
}

func TestWorkloadBadHeaderRefusedBeforeKeyLookup(t *testing.T) {
	m := loadWorkloadManifest(t)
	base := readWorkloadVector(t, "valid-auth-service.json")
	const kid = "auth-example-2026-10"
	headers := []string{
		`{"alg":"none","typ":"workload+jwt","kid":"` + kid + `"}`,
		`{"alg":"HS256","typ":"workload+jwt","kid":"` + kid + `"}`,
		`{"alg":"eddsa","typ":"workload+jwt","kid":"` + kid + `"}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":"` + kid + `"}`,
		`{"alg":"EdDSA","typ":"JWT","kid":"` + kid + `"}`,
		`{"alg":"EdDSA","typ":"Workload+JWT","kid":"` + kid + `"}`,
		`{"alg":"EdDSA","typ":"workload+jwt"}`,
		`{"alg":"EdDSA","typ":"workload+jwt","kid":""}`,
		`{"alg":"EdDSA","typ":"workload+jwt","kid":"` + kid + `","crit":["x"]}`,
		`{"alg":"EdDSA","typ":"workload+jwt","kid":"` + kid + `","jku":"https://x.example/jwks"}`,
		`{"alg":"EdDSA","typ":"workload+jwt","kid":"` + kid + `","jwk":{}}`,
		`{"alg":"EdDSA","typ":"workload+jwt","kid":"` + kid + `","x5u":"https://x.example/c"}`,
	}
	for _, header := range headers {
		keys := &countingKeys{StaticKeys: workloadCorpusKeys(t, m, false)}
		v, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: m.Issuer, Audience: "auth-service", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		vec := base
		vec.HeaderJSON = header
		_, err = v.Verify(vec.compact(), time.Unix(1760000100, 0))
		if ReasonOf(err) != ReasonContract || keys.lookups != 0 {
			t.Errorf("header %q: reason %q, lookups %d", header, ReasonOf(err), keys.lookups)
		}
	}
}

func TestWorkloadErrorsCarryNoTokenOrKeyBytes(t *testing.T) {
	m := loadWorkloadManifest(t)
	for _, row := range m.Vectors {
		vec := readWorkloadVector(t, row.File)
		_, err := workloadCorpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0))
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: refusal does not wrap ErrRefused", row.File)
		}
		text := err.Error()
		if text != ErrRefused.Error()+": "+row.Reason {
			t.Errorf("%s: unexpected error text shape", row.File)
		}
		for _, secret := range []string{vec.SignatureHex, "auth-example", "prn_", "workload"} {
			if secret != "" && strings.Contains(text, secret) {
				t.Errorf("%s: error text carries token material", row.File)
			}
		}
	}
}

func TestWorkloadNonCompactTokensRefused(t *testing.T) {
	m := loadWorkloadManifest(t)
	good := readWorkloadVector(t, "valid-auth-service.json").compact()
	row := corpusRow{Audience: "auth-service"}
	v := workloadCorpusVerifier(t, m, row)
	if _, err := v.Verify(good, time.Unix(1760000100, 0)); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	for _, junk := range []string{"\r\n", "\n", "\r", " ", "\t", "=", "+", "/", "é"} {
		for i := range parts {
			mutated := append([]string(nil), parts...)
			mutated[i] = mutated[i][:3] + junk + mutated[i][3:]
			_, err := v.Verify(strings.Join(mutated, "."), time.Unix(1760000100, 0))
			if ReasonOf(err) != ReasonContract || !errors.Is(err, ErrRefused) {
				t.Errorf("segment %d with %q: reason %q", i, junk, ReasonOf(err))
			}
		}
	}
	if _, err := v.Verify(good+"\n", time.Unix(1760000100, 0)); !errors.Is(err, ErrRefused) {
		t.Error("trailing newline accepted")
	}
	if _, err := v.Verify(strings.Repeat("A", maxTokenBytes+1), time.Unix(1760000100, 0)); ReasonOf(err) != ReasonContract {
		t.Error("oversize token not refused as contract")
	}
}

func jwksFor(kid string, pub ed25519.PublicKey) []byte {
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid, "x": base64.RawURLEncoding.EncodeToString(pub),
	}}})
	return doc
}

func TestWorkloadTokensThroughTheJWKSClientFailClosed(t *testing.T) {
	known, stranger := newSigner(t), newSigner(t)
	srv := newIssuerServer(t, jwksFor("wl-a", known.pub))
	clock := newClock()
	client := srv.client(t, clock, nil)
	issuer := srv.server.URL
	verify := func(s signer, kid string, denied ...string) (*WorkloadClaims, error) {
		now := clock.Now().Unix()
		payload := strings.Replace(workloadPayload(now, now, now+300), rtIssuer, issuer, 1)
		v, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: issuer, Audience: "auth-service", Keys: client, DeniedKids: denied})
		if err != nil {
			t.Fatal(err)
		}
		return v.Verify(s.token(workloadHeader(kid), payload), clock.Now())
	}
	step := func(name string, err error, want Reason) {
		t.Helper()
		if ReasonOf(err) != want {
			t.Errorf("%s: got %q want %q", name, ReasonOf(err), want)
		}
	}

	srv.set(func(s *issuerServer) { s.status = http.StatusInternalServerError })
	_, err := verify(known, "wl-a")
	step("outage before any key was fetched", err, ReasonUnknownKid)

	clock.Advance(time.Minute)
	srv.set(func(s *issuerServer) { s.status = http.StatusOK })
	_, err = verify(known, "wl-a")
	step("published key", err, "")
	_, err = verify(known, "wl-a", "wl-a")
	step("denied kid the client still holds", err, ReasonUnknownKid)

	hitsBefore := srv.jwksHits.Load()
	for i := 0; i < 5; i++ {
		_, err = verify(stranger, "wl-x")
		step("unpublished kid", err, ReasonUnknownKid)
	}
	if srv.jwksHits.Load()-hitsBefore > 1 {
		t.Errorf("unknown kid caused %d JWKS fetches inside the floor", srv.jwksHits.Load()-hitsBefore)
	}

	srv.set(func(s *issuerServer) { s.status = http.StatusInternalServerError })
	clock.Advance(6 * time.Minute)
	_, err = verify(known, "wl-a")
	step("outage: cached known kid inside TTL + grace", err, "")
	_, err = verify(stranger, "wl-x")
	step("outage: unknown kid", err, ReasonUnknownKid)

	clock.Advance(10 * time.Minute)
	_, err = verify(known, "wl-a")
	step("outage: known kid past TTL + grace", err, ReasonUnknownKid)

	srv.set(func(s *issuerServer) { s.status = http.StatusOK })
	clock.Advance(time.Minute)
	_, err = verify(known, "wl-a")
	step("recovery", err, "")
}

func TestWorkloadClaimShapesRefused(t *testing.T) {
	s := newSigner(t)
	v := rtVerifier(t, StaticKeys{"wl-a": s.pub}, 0)
	const prefix = `{"iss":"https://auth.example.test","aud":"auth-service","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"workload",`
	const dates = `"iat":1760000000,"nbf":1760000000,"exp":1760000300}`
	cases := map[string]string{
		"jti absent":           prefix + dates,
		"jti short":            prefix + `"jti":"short",` + dates,
		"jti long":             prefix + `"jti":"` + strings.Repeat("A", 65) + `",` + dates,
		"jti bad characters":   prefix + `"jti":"000000000000000000000+",` + dates,
		"jti number":           prefix + `"jti":123,` + dates,
		"iat absent":           prefix + `"jti":"000000000000000000000001","nbf":1760000000,"exp":1760000300}`,
		"nbf absent":           prefix + `"jti":"000000000000000000000001","iat":1760000000,"exp":1760000300}`,
		"exp absent":           prefix + `"jti":"000000000000000000000001","iat":1760000000,"nbf":1760000000}`,
		"iat zero":             prefix + `"jti":"000000000000000000000001","iat":0,"nbf":1760000000,"exp":1760000300}`,
		"exp as string":        prefix + `"jti":"000000000000000000000001","iat":1760000000,"nbf":1760000000,"exp":"1760000300"}`,
		"exp negative":         prefix + `"jti":"000000000000000000000001","iat":1760000000,"nbf":1760000000,"exp":-1}`,
		"exp past year 9999":   prefix + `"jti":"000000000000000000000001","iat":1760000000,"nbf":1760000000,"exp":253402300800}`,
		"extra claim email":    prefix + `"jti":"000000000000000000000001","email":"a@b.example",` + dates,
		"extra claim org_id":   prefix + `"jti":"000000000000000000000001","org_id":"00000000-0000-0000-0000-000000000000",` + dates,
		"extra claim sid":      prefix + `"jti":"000000000000000000000001","sid":"ses_AAAAAAAAAAAAAAAAAAAAAA",` + dates,
		"extra claim scope":    prefix + `"jti":"000000000000000000000001","scope":"auth:session",` + dates,
		"payload is an array":  `[]`,
		"payload is null":      `null`,
		"payload has trailing": prefix + `"jti":"000000000000000000000001",` + dates + ` {}`,
	}
	for name, payload := range cases {
		token := s.token(workloadHeader("wl-a"), payload)
		if _, err := v.Verify(token, time.Unix(1760000100, 0)); ReasonOf(err) != ReasonContract {
			t.Errorf("%s: got %q", name, ReasonOf(err))
		}
	}
	good := s.token(workloadHeader("wl-a"), prefix+`"jti":"000000000000000000000001",`+dates)
	if _, err := v.Verify(good, time.Unix(1760000100, 0)); err != nil {
		t.Fatalf("control payload refused: %v", ReasonOf(err))
	}
}

type shortKeys struct{}

func (shortKeys) PublicKey(string) (ed25519.PublicKey, bool) { return ed25519.PublicKey("short"), true }

func TestWorkloadMalformedKeyFromKeySourceRefused(t *testing.T) {
	s := newSigner(t)
	token := s.token(workloadHeader("wl-a"), workloadPayload(1760000000, 1760000000, 1760000300))
	if _, err := rtVerifier(t, shortKeys{}, 0).Verify(token, time.Unix(1760000100, 0)); ReasonOf(err) != ReasonUnknownKid {
		t.Fatalf("wrong-size key gave %q", ReasonOf(err))
	}
}

func TestWorkloadVerifierForAnotherAudienceRefusesEveryValidTokenBothWays(t *testing.T) {
	m := loadWorkloadManifest(t)
	other := map[string]string{"auth-service": "dev-health-api", "dev-health-api": "auth-service"}
	accepted := map[string]int{}
	for _, row := range m.Vectors {
		if row.Expect != "accept" {
			continue
		}
		accepted[row.Audience]++
		vec := readWorkloadVector(t, row.File)
		if claims, err := workloadCorpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0)); err != nil || claims == nil {
			t.Fatalf("%s: control: the %s verifier gave %q", row.File, row.Audience, ReasonOf(err))
		}
		swapped := row
		swapped.Audience = other[row.Audience]
		claims, err := workloadCorpusVerifier(t, m, swapped).Verify(vec.compact(), time.Unix(row.Now, 0))
		if claims != nil || ReasonOf(err) != ReasonAudience {
			t.Errorf("%s: %s verifier gave %q, want audience", row.File, swapped.Audience, ReasonOf(err))
		}
	}
	if accepted["auth-service"] != 6 || accepted["dev-health-api"] != 1 {
		t.Fatalf("corpus holds valid rows %v, want auth-service 6 and dev-health-api 1", accepted)
	}
}

func TestWorkloadAudiencesAreTheWorkloadClassAudiences(t *testing.T) {
	// Names from the ops contract file contracts/auth/v1/vocabulary/audiences.json: audiences whose classes include workload_token.
	want := []string{"auth-service", "dev-health-api"}
	var got []string
	for name := range workloadAudiences {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("workload audiences are %v, want %v", got, want)
	}
}

func TestWorkloadBadClaimsRefusedBeforeKeyLookup(t *testing.T) {
	m := loadWorkloadManifest(t)
	contractRows := 0
	for _, row := range m.Vectors {
		if row.Reason != string(ReasonContract) {
			continue
		}
		contractRows++
		vec := readWorkloadVector(t, row.File)
		keys := &countingKeys{StaticKeys: workloadCorpusKeys(t, m, row.KeySet == "published_and_removed")}
		v, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: m.Issuer, Audience: row.Audience, DeniedKids: m.RemovedKids, Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(vec.compact(), time.Unix(row.Now, 0)); ReasonOf(err) != ReasonContract || keys.lookups != 0 {
			t.Errorf("%s: reason %q, key lookups %d", row.File, ReasonOf(err), keys.lookups)
		}
	}
	if contractRows != 37 {
		t.Fatalf("manifest holds %d contract rows, want 37", contractRows)
	}
	s := newSigner(t)
	const prefix = `{"iss":"https://auth.example.test","aud":"auth-service","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA","principal_type":"workload",`
	payloads := map[string]string{
		"jti short":         prefix + `"jti":"short","iat":1760000000,"nbf":1760000000,"exp":1760000300}`,
		"extra claim scope": prefix + `"jti":"000000000000000000000001","scope":"x","iat":1760000000,"nbf":1760000000,"exp":1760000300}`,
		"exp as string":     prefix + `"jti":"000000000000000000000001","iat":1760000000,"nbf":1760000000,"exp":"1760000300"}`,
		"duplicate member":  prefix + `"jti":"000000000000000000000001","jti":"000000000000000000000002","iat":1760000000,"nbf":1760000000,"exp":1760000300}`,
		"not an object":     `[]`,
		"lifetime 301 s":    prefix + `"jti":"000000000000000000000001","iat":1760000000,"nbf":1760000000,"exp":1760000301}`,
	}
	for name, payload := range payloads {
		keys := &countingKeys{StaticKeys: StaticKeys{"wl-a": s.pub}}
		v := rtVerifier(t, keys, 0)
		if _, err := v.Verify(s.token(workloadHeader("wl-a"), payload), time.Unix(1760000100, 0)); ReasonOf(err) != ReasonContract || keys.lookups != 0 {
			t.Errorf("%s: reason %q, key lookups %d", name, ReasonOf(err), keys.lookups)
		}
	}
}
