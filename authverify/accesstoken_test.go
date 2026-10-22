package authverify

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const corpusDir = "testdata/access-token"

type corpusManifest struct {
	Issuer        string `json:"issuer"`
	LeewaySeconds int64  `json:"leeway_seconds"`
	RemovedKids   []string
	RemovedKeys   []corpusKey `json:"removed_public_keys"`
	Vectors       []corpusRow `json:"vectors"`
}

type corpusKey struct {
	Kid string `json:"kid"`
	X   string `json:"x"`
}

type corpusRow struct {
	File     string `json:"file"`
	Now      int64  `json:"now"`
	Audience string `json:"verify_as_audience"`
	KeySet   string `json:"key_set"`
	Expect   string `json:"expect"`
	Reason   string `json:"reason"`
}

type corpusVector struct {
	HeaderJSON   string `json:"header_json"`
	PayloadJSON  string `json:"payload_json"`
	SignatureHex string `json:"signature_hex"`
}

func (v corpusVector) compact() string {
	enc := base64.RawURLEncoding
	signature, _ := hex.DecodeString(v.SignatureHex)
	return enc.EncodeToString([]byte(v.HeaderJSON)) + "." + enc.EncodeToString([]byte(v.PayloadJSON)) + "." + enc.EncodeToString(signature)
}

func readJSON(t testing.TB, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func loadManifest(t testing.TB) corpusManifest {
	var m corpusManifest
	readJSON(t, filepath.Join(corpusDir, "manifest.json"), &m)
	var removed struct {
		Kids []string `json:"removed_kids"`
	}
	readJSON(t, filepath.Join(corpusDir, "manifest.json"), &removed)
	m.RemovedKids = removed.Kids
	return m
}

func corpusKeys(t testing.TB, m corpusManifest, withRemoved bool) StaticKeys {
	t.Helper()
	keys, err := NewEd25519JWKSVerifier(filepath.Join(corpusDir, "jwks.json")).Keys()
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

func corpusVerifier(t testing.TB, m corpusManifest, row corpusRow) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{
		Issuer: m.Issuer, Audience: row.Audience, DeniedKids: m.RemovedKids,
		Keys: corpusKeys(t, m, row.KeySet == "published_and_removed"), Leeway: time.Duration(m.LeewaySeconds) * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCorpusSourceIsPinned(t *testing.T) {
	file, err := os.Open("testdata/SOURCE")
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
			t.Fatalf("bad SOURCE line")
		}
		pinned[name] = sum
	}
	wantPaths := []string{"contracts/auth/v1/golden/access-token", "contracts/auth/v1/examples/discovery", "contracts/auth/v1/examples/jwks"}
	if !sawCommit || strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatal("SOURCE lacks ops_commit or the three ops_path lines")
	}
	var onDisk []string
	for _, set := range []string{"access-token", "discovery", "jwks"} {
		err = filepath.Walk(filepath.Join("testdata", set), func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				rel, _ := filepath.Rel("testdata", path)
				onDisk = append(onDisk, rel)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(onDisk)
	if len(onDisk) != len(pinned) || len(onDisk) < 51+20+40 {
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

func TestCorpusDecisions(t *testing.T) {
	m := loadManifest(t)
	if len(m.Vectors) == 0 {
		t.Fatal("empty manifest")
	}
	for _, row := range m.Vectors {
		t.Run(strings.TrimSuffix(row.File, ".json"), func(t *testing.T) {
			var vec corpusVector
			readJSON(t, filepath.Join(corpusDir, "vectors", row.File), &vec)
			claims, err := corpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0))
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

type countingKeys struct {
	StaticKeys
	lookups int
}

func (c *countingKeys) PublicKey(kid string) (ed25519.PublicKey, bool) {
	c.lookups++
	return c.StaticKeys.PublicKey(kid)
}

func TestBadHeaderRefusedBeforeKeyLookup(t *testing.T) {
	m := loadManifest(t)
	var base corpusVector
	readJSON(t, filepath.Join(corpusDir, "vectors", "valid-api.json"), &base)
	headers := []string{
		`{"alg":"none","typ":"at+jwt","kid":"auth-example-2026-10"}`,
		`{"alg":"HS256","typ":"at+jwt","kid":"auth-example-2026-10"}`,
		`{"alg":"eddsa","typ":"at+jwt","kid":"auth-example-2026-10"}`,
		`{"alg":"EdDSA","typ":"JWT","kid":"auth-example-2026-10"}`,
		`{"alg":"EdDSA","typ":"at+jwt"}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":""}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":"auth-example-2026-10","crit":["x"]}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":"auth-example-2026-10","jku":"https://x.example/jwks"}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":"auth-example-2026-10","jwk":{}}`,
		`{"alg":"EdDSA","typ":"at+jwt","kid":"auth-example-2026-10","x5u":"https://x.example/c"}`,
	}
	for _, header := range headers {
		keys := &countingKeys{StaticKeys: corpusKeys(t, m, false)}
		v, err := NewVerifier(VerifierConfig{Issuer: m.Issuer, Audience: "dev-health-api", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		vec := base
		vec.HeaderJSON = header
		_, err = v.Verify(vec.compact(), time.Unix(1760000300, 0))
		if ReasonOf(err) != ReasonContract || keys.lookups != 0 {
			t.Errorf("header %q: reason %q, lookups %d", header, ReasonOf(err), keys.lookups)
		}
	}
}

func TestErrorsCarryNoTokenOrKeyBytes(t *testing.T) {
	m := loadManifest(t)
	for _, row := range m.Vectors {
		var vec corpusVector
		readJSON(t, filepath.Join(corpusDir, "vectors", row.File), &vec)
		_, err := corpusVerifier(t, m, row).Verify(vec.compact(), time.Unix(row.Now, 0))
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
		for _, secret := range []string{vec.SignatureHex, "auth-example", "prn_", "ses_"} {
			if secret != "" && strings.Contains(text, secret) {
				t.Errorf("%s: error text carries token material", row.File)
			}
		}
	}
}

func TestNonCompactTokensRefused(t *testing.T) {
	m := loadManifest(t)
	var vec corpusVector
	readJSON(t, filepath.Join(corpusDir, "vectors", "valid-api.json"), &vec)
	good := vec.compact()
	row := corpusRow{Audience: "dev-health-api"}
	v := corpusVerifier(t, m, row)
	if _, err := v.Verify(good, time.Unix(1760000300, 0)); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	for _, junk := range []string{"\r\n", "\n", "\r", " ", "\t", "=", "+", "/", "\u00e9"} {
		for i := range parts {
			mutated := append([]string(nil), parts...)
			mutated[i] = mutated[i][:3] + junk + mutated[i][3:]
			_, err := v.Verify(strings.Join(mutated, "."), time.Unix(1760000300, 0))
			if ReasonOf(err) != ReasonContract || !errors.Is(err, ErrRefused) {
				t.Errorf("segment %d with %q: reason %q", i, junk, ReasonOf(err))
			}
		}
	}
	if _, err := v.Verify(good+"\n", time.Unix(1760000300, 0)); !errors.Is(err, ErrRefused) {
		t.Error("trailing newline accepted")
	}
}

func TestVerifierConfigBounds(t *testing.T) {
	keys := StaticKeys{}
	good := VerifierConfig{Issuer: "https://auth.example.test", Audience: "dev-health-api", Keys: keys}
	if _, err := NewVerifier(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*VerifierConfig){
		"no keys":       func(c *VerifierConfig) { c.Keys = nil },
		"http issuer":   func(c *VerifierConfig) { c.Issuer = "http://auth.example.test" },
		"audience":      func(c *VerifierConfig) { c.Audience = "other" },
		"leeway over":   func(c *VerifierConfig) { c.Leeway = MaxLeeway + time.Second },
		"leeway frac":   func(c *VerifierConfig) { c.Leeway = 500 * time.Millisecond },
		"leeway negate": func(c *VerifierConfig) { c.Leeway = -time.Second },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewVerifier(cfg); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

func TestLeewayShiftsClockEdges(t *testing.T) {
	m := loadManifest(t)
	var vec corpusVector
	readJSON(t, filepath.Join(corpusDir, "vectors", "valid-api.json"), &vec)
	cfg := VerifierConfig{Issuer: m.Issuer, Audience: "dev-health-api", Keys: corpusKeys(t, m, false), Leeway: 5 * time.Second}
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[int64]Reason{1759999994: ReasonNotYetValid, 1759999995: "", 1760000604: "", 1760000605: ReasonExpired} {
		if _, err := v.Verify(vec.compact(), time.Unix(at, 0)); ReasonOf(err) != want {
			t.Errorf("at %d: got %q want %q", at, ReasonOf(err), want)
		}
	}
}

func FuzzVerify(f *testing.F) {
	m := loadManifest(f)
	for _, row := range m.Vectors {
		var vec corpusVector
		readJSON(f, filepath.Join(corpusDir, "vectors", row.File), &vec)
		f.Add(vec.compact())
	}
	v, err := NewVerifier(VerifierConfig{Issuer: m.Issuer, Audience: "dev-health-api", Keys: corpusKeys(f, m, false), DeniedKids: m.RemovedKids})
	if err != nil {
		f.Fatal(err)
	}
	var valid corpusVector
	readJSON(f, filepath.Join(corpusDir, "vectors", "valid-api.json"), &valid)
	goodToken := valid.compact()
	f.Fuzz(func(t *testing.T, token string) {
		claims, err := v.Verify(token, time.Unix(1760000300, 0))
		if (err == nil) != (claims != nil) {
			t.Fatal("claims and error disagree")
		}
		if err != nil && ReasonOf(err) == "" {
			t.Fatal("unclassified error")
		}
		if err == nil && token != goodToken {
			// Any other accepted token must be a valid signed vector of the corpus.
			accepted := false
			for _, row := range m.Vectors {
				if row.Expect != "accept" || row.Audience != "dev-health-api" {
					continue
				}
				var vec corpusVector
				readJSON(t, filepath.Join(corpusDir, "vectors", row.File), &vec)
				if vec.compact() == token {
					accepted = true
				}
			}
			if !accepted {
				t.Fatal("accepted a token that is not a corpus accept vector")
			}
		}
	})
}
