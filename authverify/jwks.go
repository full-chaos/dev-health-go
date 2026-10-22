package authverify

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// ErrInvalidJWKS is returned when the JWKS document at an
// Ed25519JWKSVerifier's configured path is missing structure, malformed,
// empty, or contains a key that fails validation (wrong key type/curve/
// algorithm, invalid or duplicate key id, or a wrong-size public key).
var ErrInvalidJWKS = errors.New("invalid jwks document")

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyType string `json:"kty"`
	Curve   string `json:"crv"`
	KeyID   string `json:"kid"`
	Use     string `json:"use"`
	Alg     string `json:"alg"`
	X       string `json:"x"`
}

// Ed25519JWKSVerifier loads and validates an Ed25519 JWKS document from a
// fixed path on disk. It is standalone and carries no dependency on any
// particular claim schema or wire format -- callers use the returned key
// map to verify their own signature/keyID scheme.
type Ed25519JWKSVerifier struct {
	path string
}

// NewEd25519JWKSVerifier builds a verifier that loads its JWKS document
// from path on every Keys call (never cached), so a rotated JWKS file on
// disk is picked up without a restart.
func NewEd25519JWKSVerifier(path string) *Ed25519JWKSVerifier {
	return &Ed25519JWKSVerifier{path: path}
}

// Keys loads and parses the JWKS document, returning every valid Ed25519
// signing key keyed by its key id. A key must be type OKP, curve Ed25519,
// algorithm EdDSA, have an empty or "sig" use, a valid (non-empty, <=256
// byte) key id unique within the document, and a correctly-sized Ed25519
// public key -- any violation, or an empty/malformed document, is
// reported as ErrInvalidJWKS. A file-read failure is returned unwrapped.
func (v *Ed25519JWKSVerifier) Keys() (map[string]ed25519.PublicKey, error) {
	encoded, err := os.ReadFile(v.path)
	if err != nil {
		return nil, err
	}
	return parseJWKS(encoded)
}

// parseJWKS is the one strict JWKS parser: the file reader and the HTTP
// client both use it, so the two paths cannot disagree about a document.
func parseJWKS(encoded []byte) (map[string]ed25519.PublicKey, error) {
	if !singleDocumentWithoutDuplicateMembers(encoded) {
		return nil, ErrInvalidJWKS
	}
	var document jwksDocument
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || len(document.Keys) == 0 {
		return nil, ErrInvalidJWKS
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, candidate := range document.Keys {
		key, err := base64.RawURLEncoding.DecodeString(candidate.X)
		if err != nil || candidate.KeyType != "OKP" || candidate.Curve != "Ed25519" || candidate.Alg != "EdDSA" ||
			(candidate.Use != "" && candidate.Use != "sig") || !validKeyID(candidate.KeyID) || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalidJWKS
		}
		if _, duplicate := keys[candidate.KeyID]; duplicate {
			return nil, ErrInvalidJWKS
		}
		keys[candidate.KeyID] = ed25519.PublicKey(key)
	}
	return keys, nil
}

func validKeyID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256
}

// singleDocumentWithoutDuplicateMembers scans the raw bytes token by token
// and reports whether they hold exactly one JSON value, followed only by
// whitespace, in which no object repeats a member name. Names compare with
// simple case folding because encoding/json binds members that way.
func singleDocumentWithoutDuplicateMembers(encoded []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if !scanValue(decoder) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func scanValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '[':
		for decoder.More() {
			if !scanValue(decoder) {
				return false
			}
		}
	case '{':
		var seen []string
		for decoder.More() {
			nameToken, err := decoder.Token()
			name, isString := nameToken.(string)
			if err != nil || !isString {
				return false
			}
			for _, previous := range seen {
				if strings.EqualFold(previous, name) {
					return false
				}
			}
			seen = append(seen, name)
			if !scanValue(decoder) {
				return false
			}
		}
	default:
		return false
	}
	_, err = decoder.Token()
	return err == nil
}
