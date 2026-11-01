package authverify

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func retireAfterDoc(t *testing.T, members ...string) (string, ed25519.PublicKey) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	extra := ""
	for _, member := range members {
		extra += "," + member
	}
	doc := fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"old","alg":"EdDSA","x":%q%s}]}`,
		base64.RawURLEncoding.EncodeToString(public), extra)
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, public
}

func TestKeySetMemberAbsentHasNoRetireTime(t *testing.T) {
	path, public := retireAfterDoc(t)
	set, err := NewEd25519JWKSVerifier(path).KeySet()
	if err != nil {
		t.Fatal(err)
	}
	key, ok := set["old"]
	if !ok || len(set) != 1 || !key.Public.Equal(public) || key.RetireAfter != nil {
		t.Fatalf("key set = %#v", set)
	}
}

func TestKeySetParsesRFC3339UTCMember(t *testing.T) {
	path, _ := retireAfterDoc(t, `"retire_after":"2026-11-02T10:30:00Z"`)
	set, err := NewEd25519JWKSVerifier(path).KeySet()
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 2, 10, 30, 0, 0, time.UTC)
	got := set["old"].RetireAfter
	if got == nil || !got.Equal(want) {
		t.Fatalf("retire_after = %v, want %v", got, want)
	}
}

func TestKeySetRefusesABadMemberForTheWholeDocument(t *testing.T) {
	cases := map[string]string{
		"not rfc3339":      `"retire_after":"tomorrow"`,
		"date only":        `"retire_after":"2026-11-02"`,
		"empty string":     `"retire_after":""`,
		"number":           `"retire_after":1793615400`,
		"null":             `"retire_after":null`,
		"bool":             `"retire_after":true`,
		"duplicate member": `"retire_after":"2026-11-02T10:30:00Z","retire_after":"2026-11-03T10:30:00Z"`,
	}
	for name, member := range cases {
		t.Run(name, func(t *testing.T) {
			path, _ := retireAfterDoc(t, member)
			verifier := NewEd25519JWKSVerifier(path)
			if _, err := verifier.KeySet(); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("KeySet err = %v, want ErrInvalidJWKS", err)
			}
			if _, err := verifier.Keys(); !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("Keys err = %v, want ErrInvalidJWKS", err)
			}
		})
	}
}

func TestKeySetReturnsEveryKeyWithItsRetireTime(t *testing.T) {
	first, _, _ := ed25519.GenerateKey(rand.Reader)
	second, _, _ := ed25519.GenerateKey(rand.Reader)
	path := writeJWKS(t, map[string]any{"keys": []map[string]string{
		testJWK("old", first, map[string]string{"retire_after": "2026-11-02T10:30:00Z"}),
		testJWK("new", second, nil),
	}})
	set, err := NewEd25519JWKSVerifier(path).KeySet()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 2 || set["old"].RetireAfter == nil || set["new"].RetireAfter != nil ||
		!set["old"].Public.Equal(first) || !set["new"].Public.Equal(second) {
		t.Fatalf("key set = %#v", set)
	}
}

func TestKeysKeepsItsResultAndAppliesNoTimeCheck(t *testing.T) {
	past := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	path, public := retireAfterDoc(t, fmt.Sprintf(`"retire_after":%q`, past))
	keys, err := NewEd25519JWKSVerifier(path).Keys()
	if err != nil {
		t.Fatal(err)
	}
	var typed map[string]ed25519.PublicKey = keys
	if len(typed) != 1 || !typed["old"].Equal(public) {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestHTTPPathStillRefusesTheMember(t *testing.T) {
	path, _ := retireAfterDoc(t, `"retire_after":"2026-11-02T10:30:00Z"`)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseJWKS(raw); !errors.Is(err, ErrInvalidJWKS) {
		t.Fatalf("parseJWKS err = %v, want ErrInvalidJWKS", err)
	}
	s := newIssuerServer(t, raw)
	c := s.client(t, newClock(), nil)
	if _, ok := c.PublicKey("old"); ok {
		t.Fatal("a JWKSClient served a key from a document carrying retire_after")
	}
}
