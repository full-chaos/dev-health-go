package authverify

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const (
	typAccessToken  = "at+jwt"
	algEdDSA        = "EdDSA"
	maxNumericDate  = 253402300799
	maxTokenBytes   = 8192
	lifetimeCeiling = 60 * 60
)

var (
	kidPattern      = regexp.MustCompile(`^[!-~]{1,256}$`)
	issuerPattern   = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9_~-][A-Za-z0-9._~-]{0,63}){0,8}$`)
	subjectPattern  = regexp.MustCompile(`^prn_[A-Za-z0-9_-]+$`)
	sessionPattern  = regexp.MustCompile(`^ses_[A-Za-z0-9_-]{22,64}$`)
	jtiPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{22,64}$`)
	orgIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	digitsOnly      = regexp.MustCompile(`^[0-9]+$`)
	scopeByAudience = map[string]string{"dev-health-api": "api:access", "auth-service": "auth:session"}
	headerMembers   = []string{"alg", "typ", "kid"}
	claimsMembers   = []string{"iss", "aud", "sub", "principal_type", "sid", "jti", "iat", "nbf", "exp", "org_id", "scope"}
	claimsRequired  = []string{"iss", "aud", "sub", "principal_type", "sid", "jti", "iat", "nbf", "exp", "scope"}
	headerRequired  = headerMembers
)

// decodeObject parses raw as exactly one JSON object with no repeated member
// name (case-folded, at any depth), keeping numbers as text so a decimal form
// stays visible.
func decodeObject(raw []byte) (map[string]any, bool) {
	if !singleDocumentWithoutDuplicateMembers(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func closedSet(object map[string]any, allowed, required []string) bool {
	for name := range object {
		if !contains(allowed, name) {
			return false
		}
	}
	for _, name := range required {
		if _, ok := object[name]; !ok {
			return false
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func stringMember(object map[string]any, name string, pattern *regexp.Regexp) (string, bool) {
	value, ok := object[name].(string)
	if !ok || (pattern != nil && !pattern.MatchString(value)) {
		return "", false
	}
	return value, true
}

func numericDate(object map[string]any, name string) (int64, bool) {
	number, ok := object[name].(json.Number)
	if !ok || !digitsOnly.MatchString(number.String()) || len(number.String()) > 12 {
		return 0, false
	}
	value, err := number.Int64()
	if err != nil || value < 1 || value > maxNumericDate {
		return 0, false
	}
	return value, true
}

type accessHeader struct{ kid string }

// parseAccessHeader checks the protected header against access-token.v1:
// exactly alg EdDSA, typ at+jwt and a kid; nothing that lets the token pick
// its own key (jku, jwk, x5u, x5c, crit) is in the closed set.
func parseAccessHeader(raw []byte) (accessHeader, bool) {
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, headerMembers, headerRequired) {
		return accessHeader{}, false
	}
	if alg, ok := stringMember(object, "alg", nil); !ok || alg != algEdDSA {
		return accessHeader{}, false
	}
	if typ, ok := stringMember(object, "typ", nil); !ok || typ != typAccessToken {
		return accessHeader{}, false
	}
	kid, ok := stringMember(object, "kid", kidPattern)
	return accessHeader{kid: kid}, ok
}

// parseAccessClaims checks the payload against access-token.v1: the closed
// claim set and patterns, integer NumericDates, iat <= nbf < exp and a
// lifetime of at most one hour.
func parseAccessClaims(raw []byte) (AccessClaims, bool) {
	var claims AccessClaims
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, claimsMembers, claimsRequired) {
		return claims, false
	}
	var good [6]bool
	claims.Issuer, good[0] = stringMember(object, "iss", issuerPattern)
	claims.Audience, good[1] = stringMember(object, "aud", nil)
	claims.Subject, good[2] = stringMember(object, "sub", subjectPattern)
	claims.SessionID, good[3] = stringMember(object, "sid", sessionPattern)
	claims.TokenID, good[4] = stringMember(object, "jti", jtiPattern)
	claims.Scope, good[5] = stringMember(object, "scope", nil)
	for _, g := range good {
		if !g {
			return AccessClaims{}, false
		}
	}
	if scope, known := scopeByAudience[claims.Audience]; !known || claims.Scope != scope {
		return AccessClaims{}, false
	}
	if value, ok := stringMember(object, "principal_type", nil); !ok || value != "human" {
		return AccessClaims{}, false
	}
	if _, present := object["org_id"]; present {
		org, ok := stringMember(object, "org_id", orgIDPattern)
		if !ok {
			return AccessClaims{}, false
		}
		claims.OrgID = &org
	}
	var dates [3]bool
	claims.IssuedAt, dates[0] = numericDate(object, "iat")
	claims.NotBefore, dates[1] = numericDate(object, "nbf")
	claims.ExpiresAt, dates[2] = numericDate(object, "exp")
	for _, d := range dates {
		if !d {
			return AccessClaims{}, false
		}
	}
	if claims.NotBefore < claims.IssuedAt || claims.ExpiresAt <= claims.NotBefore || claims.ExpiresAt-claims.IssuedAt > lifetimeCeiling {
		return AccessClaims{}, false
	}
	return claims, true
}

func splitCompact(token string) (header, payload, signature string, ok bool) {
	if len(token) > maxTokenBytes || strings.TrimSpace(token) != token {
		return "", "", "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
