package authverify

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeRawJWKS(t *testing.T, raw string) *Ed25519JWKSVerifier {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewEd25519JWKSVerifier(path)
}

func rawKeyObject(t *testing.T, extra string) (string, string) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x := base64.RawURLEncoding.EncodeToString(public)
	return `{"kty":"OKP","crv":"Ed25519","kid":"a","alg":"EdDSA","x":"` + x + `"` + extra + `}`, x
}

func TestEd25519JWKSVerifier_rawBoundary(t *testing.T) {
	obj, x := rawKeyObject(t, "")
	valid := `{"keys":[` + obj + `]}`
	dupKid, _ := rawKeyObject(t, `,"kid":"b"`)
	dupX, _ := rawKeyObject(t, `,"x":"`+x+`"`)
	rejected := map[string]string{
		"trailing document":    valid + `{"keys":[],"d":"private"}`,
		"trailing garbage":     valid + ` x`,
		"duplicate kid member": `{"keys":[` + dupKid + `]}`,
		"duplicate x member":   `{"keys":[` + dupX + `]}`,
		"duplicate keys":       `{"keys":[` + obj + `],"keys":[` + obj + `]}`,
	}
	for name, raw := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			keys, err := writeRawJWKS(t, raw).Keys()
			if !errors.Is(err, ErrInvalidJWKS) {
				t.Fatalf("err = %v keys = %d, want ErrInvalidJWKS", err, len(keys))
			}
		})
	}
	for name, raw := range map[string]string{
		"plain":                     valid,
		"trailing newline":          valid + "\n",
		"trailing whitespace mixed": valid + " \t\r\n",
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			keys, err := writeRawJWKS(t, raw).Keys()
			if err != nil || len(keys) != 1 {
				t.Fatalf("err = %v keys = %d, want one key", err, len(keys))
			}
		})
	}
}
