package authverify

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Reason names why Verify refused a token. The values are the decision names
// of the access-token.v1 golden corpus; Verify decides in that order and the
// first refusal wins.
type Reason string

const (
	ReasonContract    Reason = "contract"
	ReasonUnknownKid  Reason = "unknown_kid"
	ReasonSignature   Reason = "signature"
	ReasonIssuer      Reason = "issuer"
	ReasonAudience    Reason = "audience"
	ReasonNotYetValid Reason = "not_yet_valid"
	ReasonExpired     Reason = "expired"
)

// ErrRefused is wrapped by every refusal of Verify. The error text names the
// reason only: it never carries token, header, claim or key bytes.
var ErrRefused = errors.New("access token refused")

// RefusalError is the refusal Verify returns.
type RefusalError struct{ Reason Reason }

func (e *RefusalError) Error() string { return fmt.Sprintf("%s: %s", ErrRefused, e.Reason) }
func (e *RefusalError) Unwrap() error { return ErrRefused }

// ReasonOf returns the refusal reason of err, or "" when err is not a refusal.
func ReasonOf(err error) Reason {
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

func refuse(reason Reason) error { return &RefusalError{Reason: reason} }

// MaxLeeway bounds the clock skew a Verifier accepts.
const MaxLeeway = 60 * time.Second

// KeySource resolves a key id to a trusted Ed25519 public key. It is the only
// way a Verifier obtains a key: a token never carries one.
type KeySource interface {
	PublicKey(kid string) (ed25519.PublicKey, bool)
}

// KeyResolver is an optional KeySource extension that tells a kid the source
// does not hold (nil key, nil error) from a source that could not be read
// (ErrKeySourceUnavailable). Verifiers prefer it over PublicKey.
type KeyResolver interface {
	ResolveKey(kid string) (ed25519.PublicKey, error)
}

// ErrKeySourceUnavailable is wrapped when a key source could not be read, so
// a verifier cannot tell an unknown kid from an outage. It is not a refusal.
var ErrKeySourceUnavailable = errors.New("key source unavailable")

// UnavailableError is returned by Verify when the key source is unavailable.
// It is not a *RefusalError: ReasonOf returns "" and callers answer 503.
type UnavailableError struct{}

func (*UnavailableError) Error() string { return "token key source unavailable" }
func (*UnavailableError) Unwrap() error { return ErrKeySourceUnavailable }

// lookupKey resolves kid through keys. A non-nil error is always an
// *UnavailableError.
func lookupKey(keys KeySource, kid string) (ed25519.PublicKey, bool, error) {
	if resolver, ok := keys.(KeyResolver); ok {
		key, err := resolver.ResolveKey(kid)
		if err != nil {
			return nil, false, &UnavailableError{}
		}
		return key, key != nil, nil
	}
	key, known := keys.PublicKey(kid)
	return key, known, nil
}

// StaticKeys is a KeySource over a fixed key map, such as the result of
// Ed25519JWKSVerifier.Keys.
type StaticKeys map[string]ed25519.PublicKey

// PublicKey implements KeySource.
func (s StaticKeys) PublicKey(kid string) (ed25519.PublicKey, bool) {
	key, ok := s[kid]
	return key, ok && len(key) == ed25519.PublicKeySize
}

// AccessClaims are the claims of a verified access-token.v1 token.
type AccessClaims struct {
	Issuer    string
	Audience  string
	Subject   string
	SessionID string
	TokenID   string
	Scope     string
	OrgID     *string
	// ActorSubject is the real actor of a delegated token, the sub of its act
	// claim; Subject stays the effective subject (G-49). Nil means not delegated.
	ActorSubject *string
	IssuedAt     int64
	NotBefore    int64
	ExpiresAt    int64
}

// VerifierConfig configures a Verifier.
type VerifierConfig struct {
	// Issuer must equal the token's iss exactly.
	Issuer string
	// Audience must equal the token's aud exactly.
	Audience string
	// Keys resolves kid to a public key.
	Keys KeySource
	// DeniedKids are refused as unknown even if Keys still holds them (a
	// stale cache after a retirement or compromise).
	DeniedKids []string
	// Leeway is the clock skew allowed on nbf and exp, in whole seconds,
	// 0 to MaxLeeway; a fractional value is refused by NewVerifier.
	Leeway time.Duration
}

// Verifier is the canonical access-token.v1 verifier: Ed25519 only, fail closed.
type Verifier struct {
	issuer, audience string
	keys             KeySource
	denied           map[string]struct{}
	leeway           int64
}

// NewVerifier validates cfg and returns a Verifier.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.Keys == nil || !issuerPattern.MatchString(cfg.Issuer) {
		return nil, errors.New("authverify: verifier needs an https issuer and a key source")
	}
	if _, ok := scopeByAudience[cfg.Audience]; !ok {
		return nil, errors.New("authverify: verifier audience is not an access-token.v1 audience")
	}
	if cfg.Leeway < 0 || cfg.Leeway > MaxLeeway || cfg.Leeway%time.Second != 0 {
		return nil, errors.New("authverify: verifier leeway is out of bounds")
	}
	denied := make(map[string]struct{}, len(cfg.DeniedKids))
	for _, kid := range cfg.DeniedKids {
		denied[kid] = struct{}{}
	}
	return &Verifier{issuer: cfg.Issuer, audience: cfg.Audience, keys: cfg.Keys, denied: denied, leeway: int64(cfg.Leeway / time.Second)}, nil
}

// Verify checks a compact access token at now and returns its claims. It
// decides in order: contract (header and claims shape, no key used), key by
// kid, signature, issuer, audience, nbf, exp. Any failure returns a
// *RefusalError wrapping ErrRefused, or an *UnavailableError when the key
// source could not be read.
func (v *Verifier) Verify(token string, now time.Time) (*AccessClaims, error) {
	headerPart, payloadPart, signaturePart, ok := splitCompact(token)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	enc := base64.RawURLEncoding.Strict()
	headerRaw, err1 := enc.DecodeString(headerPart)
	payloadRaw, err2 := enc.DecodeString(payloadPart)
	if err1 != nil || err2 != nil {
		return nil, refuse(ReasonContract)
	}
	header, ok := parseAccessHeader(headerRaw)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	claims, ok := parseAccessClaims(payloadRaw)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	if _, denied := v.denied[header.kid]; denied {
		return nil, refuse(ReasonUnknownKid)
	}
	key, known, err := lookupKey(v.keys, header.kid)
	if err != nil {
		return nil, err
	}
	if !known || len(key) != ed25519.PublicKeySize {
		return nil, refuse(ReasonUnknownKid)
	}
	signature, err := enc.DecodeString(signaturePart)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(headerPart+"."+payloadPart), signature) {
		return nil, refuse(ReasonSignature)
	}
	if claims.Issuer != v.issuer {
		return nil, refuse(ReasonIssuer)
	}
	if claims.Audience != v.audience {
		return nil, refuse(ReasonAudience)
	}
	seconds := now.Unix()
	if seconds+v.leeway < claims.NotBefore {
		return nil, refuse(ReasonNotYetValid)
	}
	if seconds-v.leeway >= claims.ExpiresAt {
		return nil, refuse(ReasonExpired)
	}
	return &claims, nil
}
