package authverify

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"time"
)

const (
	typWorkloadToken = "workload+jwt"
	// workloadLifetimeCeiling is the ceiling on exp - iat, in seconds (ACP-ADR-03).
	workloadLifetimeCeiling = 300
	// workloadAudience is the only audience a workload-token.v1 token may name.
	workloadAudience = "auth-service"
)

// MaxWorkloadLeeway bounds the clock skew a WorkloadVerifier accepts.
const MaxWorkloadLeeway = 30 * time.Second

var (
	workloadHeaderMembers = []string{"alg", "typ", "kid"}
	workloadClaimsMembers = []string{"iss", "aud", "sub", "principal_type", "jti", "iat", "nbf", "exp"}
)

// WorkloadClaims are the claims of a verified workload-token.v1 token.
type WorkloadClaims struct {
	Issuer    string
	Audience  string
	Subject   string
	TokenID   string
	IssuedAt  int64
	NotBefore int64
	ExpiresAt int64
}

// WorkloadVerifierConfig configures a WorkloadVerifier.
type WorkloadVerifierConfig struct {
	// Issuer must equal the token's iss exactly.
	Issuer string
	// Audience is the one service this verifier is: a token-audience service
	// (dev-health-api or auth-service). It must equal the token's aud exactly,
	// so a verifier that is not auth-service refuses every workload token.
	Audience string
	// Keys resolves kid to a public key.
	Keys KeySource
	// DeniedKids are refused as unknown even if Keys still holds them.
	DeniedKids []string
	// Leeway is the clock skew allowed on nbf and exp, in whole seconds, 0 to
	// MaxWorkloadLeeway. It never lifts the 300 s lifetime ceiling.
	Leeway time.Duration
}

// WorkloadVerifier is the canonical workload-token.v1 verifier: Ed25519 only,
// typ workload+jwt only, fail closed.
type WorkloadVerifier struct {
	issuer, audience string
	keys             KeySource
	denied           map[string]struct{}
	leeway           int64
}

// NewWorkloadVerifier validates cfg and returns a WorkloadVerifier.
func NewWorkloadVerifier(cfg WorkloadVerifierConfig) (*WorkloadVerifier, error) {
	if cfg.Keys == nil || !issuerPattern.MatchString(cfg.Issuer) {
		return nil, errors.New("authverify: workload verifier needs an https issuer and a key source")
	}
	if _, ok := scopeByAudience[cfg.Audience]; !ok {
		return nil, errors.New("authverify: workload verifier audience is not a token audience")
	}
	if cfg.Leeway < 0 || cfg.Leeway > MaxWorkloadLeeway || cfg.Leeway%time.Second != 0 {
		return nil, errors.New("authverify: workload verifier leeway is out of bounds")
	}
	denied := make(map[string]struct{}, len(cfg.DeniedKids))
	for _, kid := range cfg.DeniedKids {
		denied[kid] = struct{}{}
	}
	return &WorkloadVerifier{issuer: cfg.Issuer, audience: cfg.Audience, keys: cfg.Keys, denied: denied, leeway: int64(cfg.Leeway / time.Second)}, nil
}

// Verify checks a compact workload token at now and returns its claims. It
// decides in the order of the workload-token.v1 corpus: contract (header and
// claims shape, no key used), key by kid, signature, issuer, audience, nbf,
// exp. Any failure returns a *RefusalError wrapping ErrRefused.
func (v *WorkloadVerifier) Verify(token string, now time.Time) (*WorkloadClaims, error) {
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
	kid, ok := parseWorkloadHeader(headerRaw)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	claims, ok := parseWorkloadClaims(payloadRaw)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	if _, denied := v.denied[kid]; denied {
		return nil, refuse(ReasonUnknownKid)
	}
	key, known := v.keys.PublicKey(kid)
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

// parseWorkloadHeader checks the protected header against workload-token.v1:
// exactly alg EdDSA, typ workload+jwt (exact bytes) and a kid; nothing that
// lets the token pick its own key is in the closed set.
func parseWorkloadHeader(raw []byte) (string, bool) {
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, workloadHeaderMembers, workloadHeaderMembers) {
		return "", false
	}
	if alg, ok := stringMember(object, "alg", nil); !ok || alg != algEdDSA {
		return "", false
	}
	if typ, ok := stringMember(object, "typ", nil); !ok || typ != typWorkloadToken {
		return "", false
	}
	return stringMember(object, "kid", kidPattern)
}

// parseWorkloadClaims checks the payload against workload-token.v1: the closed
// claim set (no org_id, sid or scope), integer NumericDates, iat <= nbf < exp
// and a lifetime of at most workloadLifetimeCeiling seconds.
func parseWorkloadClaims(raw []byte) (WorkloadClaims, bool) {
	var claims WorkloadClaims
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, workloadClaimsMembers, workloadClaimsMembers) {
		return claims, false
	}
	var good [4]bool
	claims.Issuer, good[0] = stringMember(object, "iss", issuerPattern)
	claims.Audience, good[1] = stringMember(object, "aud", nil)
	claims.Subject, good[2] = stringMember(object, "sub", subjectPattern)
	claims.TokenID, good[3] = stringMember(object, "jti", jtiPattern)
	for _, g := range good {
		if !g {
			return WorkloadClaims{}, false
		}
	}
	if claims.Audience != workloadAudience {
		return WorkloadClaims{}, false
	}
	if value, ok := stringMember(object, "principal_type", nil); !ok || value != "workload" {
		return WorkloadClaims{}, false
	}
	var dates [3]bool
	claims.IssuedAt, dates[0] = numericDate(object, "iat")
	claims.NotBefore, dates[1] = numericDate(object, "nbf")
	claims.ExpiresAt, dates[2] = numericDate(object, "exp")
	for _, d := range dates {
		if !d {
			return WorkloadClaims{}, false
		}
	}
	if claims.NotBefore < claims.IssuedAt || claims.ExpiresAt <= claims.NotBefore || claims.ExpiresAt-claims.IssuedAt > workloadLifetimeCeiling {
		return WorkloadClaims{}, false
	}
	return claims, true
}
