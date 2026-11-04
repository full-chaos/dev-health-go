package authverify

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const saCorpusDir = "testdata/service-account-token"

// serviceAccountCorpusOpsCommit is the dev-health-ops commit the service
// account corpus was copied from. A corpus refresh updates this constant and
// testdata/service-account-token.SOURCE together.
const serviceAccountCorpusOpsCommit = "f85ada30f72e3af2a39e52ecca5ef0f0b7d22377"

const saKid = "auth-example-2026-10"

func loadSAManifest(t testing.TB) corpusManifest {
	t.Helper()
	var m corpusManifest
	readJSON(t, filepath.Join(saCorpusDir, "manifest.json"), &m)
	var removed struct {
		Kids []string `json:"removed_kids"`
	}
	readJSON(t, filepath.Join(saCorpusDir, "manifest.json"), &removed)
	m.RemovedKids = removed.Kids
	return m
}

func saCorpusKeys(t testing.TB, m corpusManifest, withRemoved bool) StaticKeys {
	t.Helper()
	keys, err := NewEd25519JWKSVerifier(filepath.Join(saCorpusDir, "jwks.json")).Keys()
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

func saCorpusVerifier(t testing.TB, m corpusManifest, row corpusRow) *ServiceAccountVerifier {
	t.Helper()
	v, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{
		Issuer: m.Issuer, Audience: row.Audience, DeniedKids: m.RemovedKids,
		Keys: saCorpusKeys(t, m, row.KeySet == "published_and_removed"), Leeway: time.Duration(m.LeewaySeconds) * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func readSAVector(t testing.TB, name string) corpusVector {
	t.Helper()
	var vec corpusVector
	readJSON(t, filepath.Join(saCorpusDir, "vectors", name), &vec)
	return vec
}

func saHeader(kid string) string {
	return fmt.Sprintf(`{"alg":"EdDSA","typ":"sa+jwt","kid":%q}`, kid)
}

const saClaimsTail = `"principal_type":"service_account","org_id":"00000000-0000-4000-8000-000000000001","cid":"crd_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001"`

func saPayload(iat, nbf, exp int64) string {
	return fmt.Sprintf(`{"iss":%q,"aud":"dev-health-api","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA",%s,"iat":%d,"nbf":%d,"exp":%d}`, rtIssuer, saClaimsTail, iat, nbf, exp)
}

func saVerifier(t testing.TB, keys KeySource, leeway time.Duration) *ServiceAccountVerifier {
	t.Helper()
	v, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: rtIssuer, Audience: "dev-health-api", Keys: keys, Leeway: leeway})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestServiceAccountCorpusSourceIsPinned(t *testing.T) {
	file, err := os.Open("testdata/service-account-token.SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pinned := map[string]string{}
	var paths []string
	commits := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "ops_commit "); ok {
			if value != serviceAccountCorpusOpsCommit {
				t.Fatalf("SOURCE ops_commit is %q, the corpus is pinned to %q: a corpus refresh updates serviceAccountCorpusOpsCommit, testdata/service-account-token.SOURCE and the vector set together", value, serviceAccountCorpusOpsCommit)
			}
			commits++
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
	if commits != 1 || strings.Join(paths, ",") != "contracts/auth/v1/golden/service-account-token" {
		t.Fatalf("SOURCE must hold exactly one ops_commit line and the ops_path line (ops_commit lines: %d)", commits)
	}
	var onDisk []string
	err = filepath.Walk(saCorpusDir, func(path string, info os.FileInfo, err error) error {
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
	expected := saCorpusFileSet(t)
	if len(onDisk) != len(expected) {
		t.Errorf("corpus holds %d files, the manifest names %d", len(onDisk), len(expected))
	}
	for _, name := range onDisk {
		if !expected[name] {
			t.Errorf("%s is on disk but the manifest has no row for it", name)
		}
		if _, ok := pinned[name]; !ok {
			t.Errorf("%s is on disk but SOURCE does not pin it", name)
		}
	}
	for name := range expected {
		if _, ok := pinned[name]; !ok {
			t.Errorf("%s is named by the manifest but SOURCE does not pin it", name)
		}
	}
	for name := range pinned {
		if !expected[name] {
			t.Errorf("SOURCE pins %s, which the manifest does not name", name)
		}
	}
	if t.Failed() {
		t.FailNow()
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

const saVectorCount = 83

func saCorpusFileSet(t testing.TB) map[string]bool {
	t.Helper()
	m := loadSAManifest(t)
	if len(m.Vectors) != saVectorCount {
		t.Fatalf("manifest holds %d rows, want %d", len(m.Vectors), saVectorCount)
	}
	set := map[string]bool{
		"service-account-token/README.md":     true,
		"service-account-token/jwks.json":     true,
		"service-account-token/manifest.json": true,
	}
	for _, row := range m.Vectors {
		name := "service-account-token/vectors/" + row.File
		if row.File == "" || row.File != filepath.Base(row.File) {
			t.Fatalf("manifest row names the file %q, want a bare file name", row.File)
		}
		if set[name] {
			t.Fatalf("manifest names %s twice", name)
		}
		set[name] = true
		if _, err := os.Stat(filepath.Join("testdata", name)); err != nil {
			t.Fatalf("manifest row has no file: %v", err)
		}
	}
	return set
}

func TestServiceAccountCorpusDecisions(t *testing.T) {
	m := loadSAManifest(t)
	if len(m.Vectors) != saVectorCount {
		t.Fatalf("manifest holds %d rows, want %d", len(m.Vectors), saVectorCount)
	}
	for _, row := range m.Vectors {
		t.Run(strings.TrimSuffix(row.File, ".json"), func(t *testing.T) {
			vec := readSAVector(t, row.File)
			claims, err := saCorpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0))
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

func TestServiceAccountVerifierConfigBounds(t *testing.T) {
	good := ServiceAccountVerifierConfig{Issuer: rtIssuer, Audience: "dev-health-api", Keys: StaticKeys{}}
	if _, err := NewServiceAccountVerifier(good); err != nil {
		t.Fatal(err)
	}
	other := good
	other.Audience = "auth-service"
	if _, err := NewServiceAccountVerifier(other); err != nil {
		t.Fatalf("a verifier that is another service refused: %v", err)
	}
	atMax := good
	atMax.Leeway = MaxServiceAccountLeeway
	if _, err := NewServiceAccountVerifier(atMax); err != nil {
		t.Fatalf("leeway at the maximum refused: %v", err)
	}
	for name, mutate := range map[string]func(*ServiceAccountVerifierConfig){
		"no keys":           func(c *ServiceAccountVerifierConfig) { c.Keys = nil },
		"http issuer":       func(c *ServiceAccountVerifierConfig) { c.Issuer = "http://auth.example.test" },
		"envelope audience": func(c *ServiceAccountVerifierConfig) { c.Audience = "query-api" },
		"unknown audience":  func(c *ServiceAccountVerifierConfig) { c.Audience = "other" },
		"leeway over 30 s":  func(c *ServiceAccountVerifierConfig) { c.Leeway = MaxServiceAccountLeeway + time.Second },
		"leeway 60 s":       func(c *ServiceAccountVerifierConfig) { c.Leeway = MaxLeeway },
		"leeway frac":       func(c *ServiceAccountVerifierConfig) { c.Leeway = 500 * time.Millisecond },
		"leeway negative":   func(c *ServiceAccountVerifierConfig) { c.Leeway = -time.Second },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewServiceAccountVerifier(cfg); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

func TestServiceAccountLeewayShiftsClockEdges(t *testing.T) {
	m := loadSAManifest(t)
	vec := readSAVector(t, "valid-dev-health-api.json")
	v, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: m.Issuer, Audience: "dev-health-api", Keys: saCorpusKeys(t, m, false), Leeway: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[int64]Reason{1759999994: ReasonNotYetValid, 1759999995: "", 1760000304: "", 1760000305: ReasonExpired} {
		if _, err := v.Verify(vec.compact(), time.Unix(at, 0)); ReasonOf(err) != want {
			t.Errorf("at %d: got %q want %q", at, ReasonOf(err), want)
		}
	}
}

func TestServiceAccountLifetimeCeilingEdge(t *testing.T) {
	s := newSigner(t)
	keys := StaticKeys{"sa-a": s.pub}
	const iat = 1760000000
	for _, leeway := range []time.Duration{0, MaxServiceAccountLeeway} {
		v := saVerifier(t, keys, leeway)
		at300 := s.token(saHeader("sa-a"), saPayload(iat, iat, iat+300))
		claims, err := v.Verify(at300, time.Unix(iat, 0))
		if err != nil || claims == nil {
			t.Errorf("leeway %v: 300 s lifetime refused: %v", leeway, ReasonOf(err))
		}
		at301 := s.token(saHeader("sa-a"), saPayload(iat, iat, iat+301))
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

func TestServiceAccountClaimsReturned(t *testing.T) {
	s := newSigner(t)
	v := saVerifier(t, StaticKeys{"sa-a": s.pub}, 0)
	claims, err := v.Verify(s.token(saHeader("sa-a"), saPayload(1760000000, 1760000001, 1760000200)), time.Unix(1760000100, 0))
	if err != nil || claims == nil {
		t.Fatalf("refused: %v", ReasonOf(err))
	}
	want := ServiceAccountClaims{
		Issuer: rtIssuer, Audience: "dev-health-api", Subject: "prn_AAAAAAAAAAAAAAAAAAAAAA",
		OrgID: "00000000-0000-4000-8000-000000000001", CredentialID: "crd_AAAAAAAAAAAAAAAAAAAAAA", TokenID: "000000000000000000000001",
		IssuedAt: 1760000000, NotBefore: 1760000001, ExpiresAt: 1760000200,
	}
	if *claims != want {
		t.Fatalf("claims %+v, want %+v", *claims, want)
	}
}

func TestServiceAccountClaimTable(t *testing.T) {
	s := newSigner(t)
	keys := &countingKeys{StaticKeys: StaticKeys{"sa-a": s.pub}}
	v := saVerifier(t, keys, 0)
	const win = `"iat":1760000000,"nbf":1760000000,"exp":1760000300`
	head := fmt.Sprintf(`"iss":%q,"aud":"dev-health-api","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA"`, rtIssuer)
	cases := map[string]string{
		"sid":                `{` + head + `,` + saClaimsTail + `,"sid":"ses_AAAAAAAAAAAAAAAAAAAAAA",` + win + `}`,
		"scope":              `{` + head + `,` + saClaimsTail + `,"scope":"x",` + win + `}`,
		"audiences":          `{` + head + `,` + saClaimsTail + `,"audiences":["dev-health-api"],` + win + `}`,
		"act":                `{` + head + `,` + saClaimsTail + `,"act":{"sub":"prn_x"},` + win + `}`,
		"extra member":       `{` + head + `,` + saClaimsTail + `,"x":1,` + win + `}`,
		"aud array":          `{"iss":"` + rtIssuer + `","aud":["dev-health-api"],"sub":"prn_AAAAAAAAAAAAAAAAAAAAAA",` + saClaimsTail + `,` + win + `}`,
		"aud other":          `{"iss":"` + rtIssuer + `","aud":"acr-api","sub":"prn_AAAAAAAAAAAAAAAAAAAAAA",` + saClaimsTail + `,` + win + `}`,
		"principal human":    `{` + head + `,"principal_type":"human","org_id":"00000000-0000-4000-8000-000000000001","cid":"crd_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001",` + win + `}`,
		"principal workload": `{` + head + `,"principal_type":"workload","org_id":"00000000-0000-4000-8000-000000000001","cid":"crd_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001",` + win + `}`,
		"org upper":          `{` + head + `,"principal_type":"service_account","org_id":"00000000-0000-4000-8000-00000000000A","cid":"crd_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001",` + win + `}`,
		"org absent":         `{` + head + `,"principal_type":"service_account","cid":"crd_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001",` + win + `}`,
		"cid absent":         `{` + head + `,"principal_type":"service_account","org_id":"00000000-0000-4000-8000-000000000001","jti":"000000000000000000000001",` + win + `}`,
		"cid no prefix":      `{` + head + `,"principal_type":"service_account","org_id":"00000000-0000-4000-8000-000000000001","cid":"AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001",` + win + `}`,
		"cid principal":      `{` + head + `,"principal_type":"service_account","org_id":"00000000-0000-4000-8000-000000000001","cid":"prn_AAAAAAAAAAAAAAAAAAAAAA","jti":"000000000000000000000001",` + win + `}`,
		"cid over length":    `{` + head + `,"principal_type":"service_account","org_id":"00000000-0000-4000-8000-000000000001","cid":"crd_` + strings.Repeat("A", 65) + `","jti":"000000000000000000000001",` + win + `}`,
		"duplicate cid":      `{` + head + `,` + saClaimsTail + `,"cid":"crd_BBBBBBBBBBBBBBBBBBBBBB",` + win + `}`,
		"nbf before iat":     `{` + head + `,` + saClaimsTail + `,"iat":1760000010,"nbf":1760000000,"exp":1760000300}`,
		"exp decimal":        `{` + head + `,` + saClaimsTail + `,"iat":1760000000,"nbf":1760000000,"exp":1760000300.0}`,
	}
	for name, payload := range cases {
		keys.lookups = 0
		_, err := v.Verify(s.token(saHeader("sa-a"), payload), time.Unix(1760000100, 0))
		if ReasonOf(err) != ReasonContract || keys.lookups != 0 {
			t.Errorf("%s: reason %q, lookups %d", name, ReasonOf(err), keys.lookups)
		}
	}
}

func TestServiceAccountBadHeaderRefusedBeforeKeyLookup(t *testing.T) {
	m := loadSAManifest(t)
	base := readSAVector(t, "valid-dev-health-api.json")
	headers := []string{
		`{"alg":"none","typ":"sa+jwt","kid":"` + saKid + `"}`,
		`{"alg":"HS256","typ":"sa+jwt","kid":"` + saKid + `"}`,
		`{"alg":"eddsa","typ":"sa+jwt","kid":"` + saKid + `"}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":"` + saKid + `"}`,
		`{"alg":"EdDSA","typ":"workload+jwt","kid":"` + saKid + `"}`,
		`{"alg":"EdDSA","typ":"JWT","kid":"` + saKid + `"}`,
		`{"alg":"EdDSA","typ":"SA+JWT","kid":"` + saKid + `"}`,
		`{"alg":"EdDSA","typ":"sa+jwt "}`,
		`{"alg":"EdDSA","typ":"application/sa+jwt","kid":"` + saKid + `"}`,
		`{"alg":"EdDSA","typ":"sa+jwt"}`,
		`{"alg":"EdDSA","typ":"sa+jwt","kid":""}`,
		`{"alg":"EdDSA","typ":"sa+jwt","kid":"` + saKid + `","crit":["x"]}`,
		`{"alg":"EdDSA","typ":"sa+jwt","kid":"` + saKid + `","jku":"https://x.example/jwks"}`,
		`{"alg":"EdDSA","typ":"sa+jwt","kid":"` + saKid + `","jwk":{}}`,
	}
	for _, header := range headers {
		keys := &countingKeys{StaticKeys: saCorpusKeys(t, m, false)}
		v, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: m.Issuer, Audience: "dev-health-api", Keys: keys})
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

func TestServiceAccountRefusesOtherTokenClasses(t *testing.T) {
	s := newSigner(t)
	keys := StaticKeys{"sa-a": s.pub}
	now := time.Unix(1760000100, 0)
	sa := s.token(saHeader("sa-a"), saPayload(1760000000, 1760000000, 1760000300))
	if _, err := saVerifier(t, keys, 0).Verify(sa, now); err != nil {
		t.Fatalf("control: refused by its own verifier: %v", ReasonOf(err))
	}
	user := userShapedToken(s, "sa-a")
	workload := s.token(workloadHeader("sa-a"), workloadPayload(1760000000, 1760000000, 1760000300))
	for name, tok := range map[string]string{"user": user, "workload": workload} {
		if _, err := saVerifier(t, keys, 0).Verify(tok, now); ReasonOf(err) != ReasonContract {
			t.Errorf("%s token at the service account verifier gave %q", name, ReasonOf(err))
		}
	}
	for _, audience := range []string{"auth-service", "dev-health-api"} {
		u, err := NewVerifier(VerifierConfig{Issuer: rtIssuer, Audience: audience, Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := u.Verify(sa, now); ReasonOf(err) != ReasonContract {
			t.Errorf("service account token at the user verifier (%s) gave %q", audience, ReasonOf(err))
		}
		w, err := NewWorkloadVerifier(WorkloadVerifierConfig{Issuer: rtIssuer, Audience: audience, Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Verify(sa, now); ReasonOf(err) != ReasonContract {
			t.Errorf("service account token at the workload verifier (%s) gave %q", audience, ReasonOf(err))
		}
	}
}

type saResolver struct {
	keys  StaticKeys
	fail  bool
	calls int
}

func (r *saResolver) PublicKey(kid string) (ed25519.PublicKey, bool) {
	k, ok := r.keys[kid]
	return k, ok
}

func (r *saResolver) ResolveKey(kid string) (ed25519.PublicKey, error) {
	r.calls++
	if r.fail {
		return nil, errors.New("down")
	}
	return r.keys[kid], nil
}

func TestServiceAccountKeySourceOutageIsUnavailable(t *testing.T) {
	s := newSigner(t)
	res := &saResolver{keys: StaticKeys{"sa-a": s.pub}, fail: true}
	v := saVerifier(t, res, 0)
	tok := s.token(saHeader("sa-a"), saPayload(1760000000, 1760000000, 1760000300))
	_, err := v.Verify(tok, time.Unix(1760000100, 0))
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || ReasonOf(err) != "" || errors.Is(err, ErrRefused) || !errors.Is(err, ErrKeySourceUnavailable) {
		t.Fatalf("outage gave %v (reason %q)", err, ReasonOf(err))
	}
	res.fail = false
	unknown := s.token(saHeader("other"), saPayload(1760000000, 1760000000, 1760000300))
	if _, err := v.Verify(unknown, time.Unix(1760000100, 0)); ReasonOf(err) != ReasonUnknownKid || errors.Is(err, ErrKeySourceUnavailable) {
		t.Fatalf("unknown kid on a healthy source gave %v", err)
	}
}

func TestServiceAccountContractStepUsesNoKey(t *testing.T) {
	s := newSigner(t)
	res := &saResolver{keys: StaticKeys{"sa-a": s.pub}, fail: true}
	v := saVerifier(t, res, 0)
	_, err := v.Verify(s.token(saHeader("sa-a"), `{"iss":"x"}`), time.Unix(1760000100, 0))
	if ReasonOf(err) != ReasonContract || res.calls != 0 {
		t.Fatalf("reason %q, resolver calls %d", ReasonOf(err), res.calls)
	}
}

func TestServiceAccountDeniedKidRefusedEvenWhenPublished(t *testing.T) {
	s := newSigner(t)
	v, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: rtIssuer, Audience: "dev-health-api", Keys: StaticKeys{"sa-a": s.pub}, DeniedKids: []string{"sa-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(s.token(saHeader("sa-a"), saPayload(1760000000, 1760000000, 1760000300)), time.Unix(1760000100, 0)); ReasonOf(err) != ReasonUnknownKid {
		t.Fatalf("denied kid gave %q", ReasonOf(err))
	}
}

func TestServiceAccountErrorsCarryNoTokenOrKeyBytes(t *testing.T) {
	m := loadSAManifest(t)
	for _, row := range m.Vectors {
		vec := readSAVector(t, row.File)
		_, err := saCorpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0))
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
		for _, secret := range []string{vec.SignatureHex, "auth-example", "prn_", "crd_", "sa+jwt"} {
			if secret != "" && strings.Contains(text, secret) {
				t.Errorf("%s: error text carries token material", row.File)
			}
		}
	}
}

func TestServiceAccountVerifierAudienceDefaultsToDevHealthAPI(t *testing.T) {
	s := newSigner(t)
	keys := StaticKeys{"sa-a": s.pub}
	tok := s.token(saHeader("sa-a"), saPayload(1760000000, 1760000000, 1760000300))
	now := time.Unix(1760000100, 0)
	v, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: rtIssuer, Keys: keys})
	if err != nil {
		t.Fatalf("empty audience refused: %v", err)
	}
	if claims, err := v.Verify(tok, now); err != nil || claims == nil {
		t.Fatalf("default verifier refused a dev-health-api token: %v", ReasonOf(err))
	}
	other, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: rtIssuer, Audience: "auth-service", Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(tok, now); ReasonOf(err) != ReasonAudience {
		t.Fatalf("auth-service verifier gave %q", ReasonOf(err))
	}
}

func TestServiceAccountVerifierRefusesUnknownAudience(t *testing.T) {
	for _, audience := range []string{"query-api", "acr-api", "acr-mcp", "other", "Dev-Health-Api", "dev-health-api ", "auth-service,dev-health-api"} {
		if _, err := NewServiceAccountVerifier(ServiceAccountVerifierConfig{Issuer: rtIssuer, Audience: audience, Keys: StaticKeys{}}); err == nil {
			t.Errorf("audience %q accepted", audience)
		}
	}
}
