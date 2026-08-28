package authverify

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeJWKS ports the single-key JWKS fixture shape from acr's own
// internal/auth/web_assertion_test.go (writeTestJWKS) and
// internal/api/web_assertion_test.go (writeAPIJWKS) -- the only
// JWKS-shaped test fixture acr had; acr never exercised keys()'s
// malformed-document validation rules directly (only indirectly, through
// WebAssertionVerifier.Verify's end-to-end signature checks), so the
// rejection cases below are written fresh against the ported validation
// logic itself.
func writeJWKS(t *testing.T, doc map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testJWK(kid string, public ed25519.PublicKey, overrides map[string]string) map[string]string {
	key := map[string]string{
		"kty": "OKP", "crv": "Ed25519", "kid": kid, "alg": "EdDSA",
		"x": base64.RawURLEncoding.EncodeToString(public),
	}
	for k, v := range overrides {
		key[k] = v
	}
	return key
}

func TestEd25519JWKSVerifier_returnsValidKeys(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := writeJWKS(t, map[string]any{"keys": []map[string]string{testJWK("current", public, nil)}})
	verifier := NewEd25519JWKSVerifier(path)

	keys, err := verifier.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %#v, want exactly one entry", keys)
	}
	if !keys["current"].Equal(public) {
		t.Fatal("returned public key does not match the JWKS document's key")
	}
}

func TestEd25519JWKSVerifier_acceptsSigUse(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := writeJWKS(t, map[string]any{"keys": []map[string]string{testJWK("current", public, map[string]string{"use": "sig"})}})
	if _, err := NewEd25519JWKSVerifier(path).Keys(); err != nil {
		t.Fatalf("use=sig should be accepted: %v", err)
	}
}

func TestEd25519JWKSVerifier_returnsMultipleKeys(t *testing.T) {
	publicA, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicB, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := writeJWKS(t, map[string]any{"keys": []map[string]string{
		testJWK("a", publicA, nil), testJWK("b", publicB, nil),
	}})
	keys, err := NewEd25519JWKSVerifier(path).Keys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || !keys["a"].Equal(publicA) || !keys["b"].Equal(publicB) {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestEd25519JWKSVerifier_rejectsMissingFile(t *testing.T) {
	verifier := NewEd25519JWKSVerifier(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if _, err := verifier.Keys(); err == nil {
		t.Fatal("expected an error for a missing JWKS file")
	} else if errors.Is(err, ErrInvalidJWKS) {
		t.Fatal("a missing file should surface the raw read error, not ErrInvalidJWKS")
	}
}

func TestEd25519JWKSVerifier_rejectsEmptyKeySet(t *testing.T) {
	path := writeJWKS(t, map[string]any{"keys": []map[string]string{}})
	if _, err := NewEd25519JWKSVerifier(path).Keys(); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error = %v, want ErrInvalidJWKS", err)
	}
}

func TestEd25519JWKSVerifier_rejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEd25519JWKSVerifier(path).Keys(); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error = %v, want ErrInvalidJWKS", err)
	}
}

func TestEd25519JWKSVerifier_rejectsDuplicateKeyID(t *testing.T) {
	publicA, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicB, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := writeJWKS(t, map[string]any{"keys": []map[string]string{
		testJWK("dup", publicA, nil), testJWK("dup", publicB, nil),
	}})
	if _, err := NewEd25519JWKSVerifier(path).Keys(); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("error = %v, want ErrInvalidJWKS for a duplicate key id", err)
	}
}

func TestEd25519JWKSVerifier_rejectsInvalidKeys(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		overrides map[string]string
	}{
		{"wrong key type", map[string]string{"kty": "RSA"}},
		{"wrong curve", map[string]string{"crv": "P-256"}},
		{"wrong algorithm", map[string]string{"alg": "RS256"}},
		{"unsupported use", map[string]string{"use": "enc"}},
		{"empty key id", map[string]string{"kid": ""}},
		{"oversized key id", map[string]string{"kid": string(make([]byte, 257))}},
		{"undecodable key material", map[string]string{"x": "not-base64url!!"}},
		{"wrong-size key material", map[string]string{"x": base64.RawURLEncoding.EncodeToString([]byte("too-short"))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeJWKS(t, map[string]any{"keys": []map[string]string{testJWK("current", public, test.overrides)}})
			if _, err := NewEd25519JWKSVerifier(path).Keys(); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("error = %v, want ErrInvalidJWKS", err)
			}
		})
	}
}
