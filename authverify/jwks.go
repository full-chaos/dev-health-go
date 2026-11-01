package authverify

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
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

	RetireAfter retireAfterMember `json:"retire_after"`
}

// retireAfterGrammar is the RFC 3339 date-time grammar, anchored, with a zero
// offset only: date "T"|"t" time, secfrac "." 1*DIGIT, offset "Z"|"z"|"+00:00"
// |"-00:00". time.Parse is not used as the validator: it accepts a comma
// fraction (RFC 3339 refuses it) and refuses lowercase t/z and a leap second.
var retireAfterGrammar = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(?:[Zz]|\+00:00|-00:00)$`)

// retireAfterMember decodes the optional retire_after member strictly: a
// string that matches the RFC 3339 grammar with a zero UTC offset. Any other
// JSON type (number, null, bool), an empty string, a comma fraction, a
// fraction of more than 9 digits (nanosecond precision, fail closed), a
// non-zero offset or a malformed time fails the decode.
//
// A leap second is admitted only as 23:59:60 (any fraction) and maps to
// 23:59:59.999999999 UTC of that day, so a key retires no later than the
// leap second (fail closed).
type retireAfterMember struct {
	present bool
	at      time.Time
}

func (m *retireAfterMember) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return ErrInvalidJWKS
	}
	parts := retireAfterGrammar.FindStringSubmatch(text)
	if parts == nil || len(parts[5]) > 9 {
		return ErrInvalidJWKS
	}
	date, hour, minute, second, fraction := parts[1], parts[2], parts[3], parts[4], parts[5]
	clock := hour + ":" + minute + ":" + second
	if fraction != "" {
		clock += "." + fraction
	}
	if second == "60" {
		if hour != "23" || minute != "59" {
			return ErrInvalidJWKS
		}
		clock = "23:59:59.999999999"
	}
	at, err := time.Parse(time.RFC3339Nano, date+"T"+clock+"Z")
	if err != nil {
		return ErrInvalidJWKS
	}
	m.present, m.at = true, at.UTC()
	return nil
}

// JWKSKey is one key of a file JWKS document with its optional retire time.
// RetireAfter is the ops registry row's retire_after for the key: authverify
// parses it and applies no time rule; the caller decides what it means. nil
// means the member is absent.
type JWKSKey struct {
	Public      ed25519.PublicKey
	RetireAfter *time.Time
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
//
// Keys applies no retire_after rule and its result is unchanged by the
// member: use KeySet to read it. A document whose retire_after member is
// malformed is refused here as in KeySet.
func (v *Ed25519JWKSVerifier) Keys() (map[string]ed25519.PublicKey, error) {
	set, err := v.KeySet()
	if err != nil {
		return nil, err
	}
	keys := make(map[string]ed25519.PublicKey, len(set))
	for kid, key := range set {
		keys[kid] = key.Public
	}
	return keys, nil
}

// KeySet loads the JWKS document like Keys and also returns each key's
// optional retire_after member (RFC 3339 UTC string, the ops registry row's
// retire_after). authverify only parses the member; it never compares it
// with a clock. A malformed member (not RFC 3339, empty, not a string, null,
// non-UTC offset, repeated in one key) refuses the whole document with
// ErrInvalidJWKS. The member is admitted on this file path only: the HTTP
// JWKSClient still refuses a document that carries it.
func (v *Ed25519JWKSVerifier) KeySet() (map[string]JWKSKey, error) {
	encoded, err := os.ReadFile(v.path)
	if err != nil {
		return nil, err
	}
	return parseJWKSDocument(encoded, true)
}

// parseJWKS is the HTTP-path form of the strict parser: it refuses a
// document that carries retire_after.
func parseJWKS(encoded []byte) (map[string]ed25519.PublicKey, error) {
	set, err := parseJWKSDocument(encoded, false)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]ed25519.PublicKey, len(set))
	for kid, key := range set {
		keys[kid] = key.Public
	}
	return keys, nil
}

// parseJWKSDocument is the one strict JWKS parser: the file reader and the
// HTTP client both use it, so the two paths cannot disagree about a
// document. admitRetireAfter is its one option: it admits the per-key
// retire_after member (file path only).
func parseJWKSDocument(encoded []byte, admitRetireAfter bool) (map[string]JWKSKey, error) {
	if !singleDocumentWithoutDuplicateMembers(encoded) {
		return nil, ErrInvalidJWKS
	}
	var document jwksDocument
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || len(document.Keys) == 0 {
		return nil, ErrInvalidJWKS
	}
	keys := make(map[string]JWKSKey, len(document.Keys))
	for _, candidate := range document.Keys {
		key, err := base64.RawURLEncoding.DecodeString(candidate.X)
		if err != nil || candidate.KeyType != "OKP" || candidate.Curve != "Ed25519" || candidate.Alg != "EdDSA" ||
			(candidate.Use != "" && candidate.Use != "sig") || !validKeyID(candidate.KeyID) || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalidJWKS
		}
		if _, duplicate := keys[candidate.KeyID]; duplicate {
			return nil, ErrInvalidJWKS
		}
		entry := JWKSKey{Public: ed25519.PublicKey(key)}
		if candidate.RetireAfter.present {
			if !admitRetireAfter {
				return nil, ErrInvalidJWKS
			}
			at := candidate.RetireAfter.at
			entry.RetireAfter = &at
		}
		keys[candidate.KeyID] = entry
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
