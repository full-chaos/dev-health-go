package authverify

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"regexp"
	"time"
)

const (
	typServiceAccountToken = "sa+jwt"
	// serviceAccountTokenAudience is the one audience a service account token
	// names (service-account-token.v1).
	serviceAccountTokenAudience = "dev-health-api"
	// serviceAccountLifetimeCeiling is the ceiling on exp - iat, in seconds.
	serviceAccountLifetimeCeiling = int64(WorkloadTokenLifetimeCeiling / time.Second)
)

// MaxServiceAccountLeeway bounds the clock skew a ServiceAccountVerifier accepts.
const MaxServiceAccountLeeway = 30 * time.Second

var (
	serviceAccountHeaderMembers = []string{"alg", "typ", "kid"}
	serviceAccountClaimsMembers = []string{"iss", "aud", "sub", "principal_type", "org_id", "cid", "jti", "iat", "nbf", "exp"}
	credentialIDPattern         = regexp.MustCompile(`^crd_[A-Za-z0-9_-]{1,64}$`)
)

// serviceAccountVerifierAudiences are the services that may run a verifier. A
// token names dev-health-api only, so a verifier that is another service
// refuses every valid token with reason audience.
var serviceAccountVerifierAudiences = map[string]struct{}{"auth-service": {}, "dev-health-api": {}}

// ServiceAccountClaims are the claims of a verified service-account-token.v1
// token. They name the account, its organization and the key row the token
// was exchanged for; they grant nothing. A relying party re-reads the live
// account and key row and never trusts OrgID over it.
type ServiceAccountClaims struct {
	Issuer       string
	Audience     string
	Subject      string
	OrgID        string
	CredentialID string
	TokenID      string
	IssuedAt     int64
	NotBefore    int64
	ExpiresAt    int64
}

// ServiceAccountVerifierConfig configures a ServiceAccountVerifier.
type ServiceAccountVerifierConfig struct {
	// Issuer must equal the token's iss exactly.
	Issuer string
	// Audience is the one service this verifier is: dev-health-api (the
	// default when empty) or auth-service. A token names dev-health-api only,
	// so auth-service refuses it with reason audience. Any other value is
	// refused at construction.
	Audience string
	// Keys resolves kid to a public key.
	Keys KeySource
	// DeniedKids are refused as unknown even if Keys still holds them.
	DeniedKids []string
	// Leeway is the clock skew allowed on nbf and exp, in whole seconds, 0 to
	// MaxServiceAccountLeeway. It never lifts the 300 s lifetime ceiling.
	Leeway time.Duration
}

// ServiceAccountVerifier is the canonical service-account-token.v1 verifier:
// Ed25519 only, typ sa+jwt only, fail closed. The token cannot show the
// signing key's expiry, so the rule that a token never outlives its key stays
// with the issuer; the verifier enforces the 300 s ceiling.
type ServiceAccountVerifier struct {
	issuer, audience string
	keys             KeySource
	denied           map[string]struct{}
	leeway           int64
}

// NewServiceAccountVerifier validates cfg and returns a ServiceAccountVerifier.
func NewServiceAccountVerifier(cfg ServiceAccountVerifierConfig) (*ServiceAccountVerifier, error) {
	if cfg.Keys == nil || !issuerPattern.MatchString(cfg.Issuer) {
		return nil, errors.New("authverify: service account verifier needs an https issuer and a key source")
	}
	if cfg.Audience == "" {
		cfg.Audience = serviceAccountTokenAudience
	}
	if _, ok := serviceAccountVerifierAudiences[cfg.Audience]; !ok {
		return nil, errors.New("authverify: service account verifier audience is not a service audience")
	}
	if cfg.Leeway < 0 || cfg.Leeway > MaxServiceAccountLeeway || cfg.Leeway%time.Second != 0 {
		return nil, errors.New("authverify: service account verifier leeway is out of bounds")
	}
	denied := make(map[string]struct{}, len(cfg.DeniedKids))
	for _, kid := range cfg.DeniedKids {
		denied[kid] = struct{}{}
	}
	return &ServiceAccountVerifier{issuer: cfg.Issuer, audience: cfg.Audience, keys: cfg.Keys, denied: denied, leeway: int64(cfg.Leeway / time.Second)}, nil
}

// Verify checks a compact service account token at now and returns its
// claims. It decides in the order of the service-account-token.v1 corpus:
// contract (header and claims shape, no key used), key by kid, signature,
// issuer, audience, nbf, exp. Any failure returns a *RefusalError wrapping
// ErrRefused, or an *UnavailableError when the key source could not be read.
func (v *ServiceAccountVerifier) Verify(token string, now time.Time) (*ServiceAccountClaims, error) {
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
	kid, ok := parseServiceAccountHeader(headerRaw)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	claims, ok := parseServiceAccountClaims(payloadRaw)
	if !ok {
		return nil, refuse(ReasonContract)
	}
	if _, denied := v.denied[kid]; denied {
		return nil, refuse(ReasonUnknownKid)
	}
	key, known, err := lookupKey(v.keys, kid)
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

// parseServiceAccountHeader checks the protected header against
// service-account-token.v1: exactly alg EdDSA, typ sa+jwt (exact bytes) and a
// kid; nothing that lets the token pick its own key is in the closed set.
func parseServiceAccountHeader(raw []byte) (string, bool) {
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, serviceAccountHeaderMembers, serviceAccountHeaderMembers) {
		return "", false
	}
	if alg, ok := stringMember(object, "alg", nil); !ok || alg != algEdDSA {
		return "", false
	}
	if typ, ok := stringMember(object, "typ", nil); !ok || typ != typServiceAccountToken {
		return "", false
	}
	return stringMember(object, "kid", kidPattern)
}

// parseServiceAccountClaims checks the payload against service-account-token.v1:
// the closed claim set (no sid, scope, audience list or act), aud
// dev-health-api, principal_type service_account, integer NumericDates,
// iat <= nbf < exp and a lifetime of at most serviceAccountLifetimeCeiling seconds.
func parseServiceAccountClaims(raw []byte) (ServiceAccountClaims, bool) {
	var claims ServiceAccountClaims
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, serviceAccountClaimsMembers, serviceAccountClaimsMembers) {
		return claims, false
	}
	var good [6]bool
	claims.Issuer, good[0] = stringMember(object, "iss", issuerPattern)
	claims.Audience, good[1] = stringMember(object, "aud", nil)
	claims.Subject, good[2] = stringMember(object, "sub", subjectPattern)
	claims.OrgID, good[3] = stringMember(object, "org_id", orgIDPattern)
	claims.CredentialID, good[4] = stringMember(object, "cid", credentialIDPattern)
	claims.TokenID, good[5] = stringMember(object, "jti", jtiPattern)
	for _, g := range good {
		if !g {
			return ServiceAccountClaims{}, false
		}
	}
	if claims.Audience != serviceAccountTokenAudience {
		return ServiceAccountClaims{}, false
	}
	if value, ok := stringMember(object, "principal_type", nil); !ok || value != "service_account" {
		return ServiceAccountClaims{}, false
	}
	var dates [3]bool
	claims.IssuedAt, dates[0] = numericDate(object, "iat")
	claims.NotBefore, dates[1] = numericDate(object, "nbf")
	claims.ExpiresAt, dates[2] = numericDate(object, "exp")
	for _, d := range dates {
		if !d {
			return ServiceAccountClaims{}, false
		}
	}
	if claims.NotBefore < claims.IssuedAt || claims.ExpiresAt <= claims.NotBefore || claims.ExpiresAt-claims.IssuedAt > serviceAccountLifetimeCeiling {
		return ServiceAccountClaims{}, false
	}
	return claims, true
}
