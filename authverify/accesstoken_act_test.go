package authverify

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const (
	actIssuer  = "https://auth.example.test"
	actKid     = "act-test-key"
	actSubject = "prn_AAAAAAAAAAAAAAAAAAAAAA"
	actActor   = "prn_BBBBBBBBBBBBBBBBBBBBBB"
)

// actPayload is an otherwise valid payload with extra as its last members
// (each with a leading comma), so a row states the exact member text.
func actPayload(audience, scope, extra string) string {
	return `{"iss":"` + actIssuer + `","aud":"` + audience + `","sub":"` + actSubject +
		`","principal_type":"human","sid":"ses_AAAAAAAAAAAAAAAAAAAAAQ","jti":"000000000000000000000001",` +
		`"iat":1760000000,"nbf":1760000000,"exp":1760000600,"scope":"` + scope + `"` + extra + `}`
}

func signedToken(t *testing.T, private ed25519.PrivateKey, payload string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(`{"alg":"EdDSA","typ":"at+jwt","kid":"`+actKid+`"}`)) + "." + enc.EncodeToString([]byte(payload))
	return input + "." + enc.EncodeToString(ed25519.Sign(private, []byte(input)))
}

func actKeys(t *testing.T) (*countingKeys, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &countingKeys{StaticKeys: StaticKeys{actKid: public}}, private
}

func actVerify(t *testing.T, audience, scope, extra string) (*AccessClaims, int, error) {
	t.Helper()
	keys, private := actKeys(t)
	v, err := NewVerifier(VerifierConfig{Issuer: actIssuer, Audience: audience, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := v.Verify(signedToken(t, private, actPayload(audience, scope, extra)), time.Unix(1760000300, 0))
	return claims, keys.lookups, err
}

func TestActAccepted(t *testing.T) {
	for name, row := range map[string]struct{ audience, scope string }{
		"valid_api":          {"dev-health-api", "api:access"},
		"valid_auth_service": {"auth-service", "auth:session"},
	} {
		t.Run(name, func(t *testing.T) {
			claims, _, err := actVerify(t, row.audience, row.scope, `,"act":{"sub":"`+actActor+`"}`)
			if err != nil {
				t.Fatalf("want accept, got %q", ReasonOf(err))
			}
			if claims.ActorSubject == nil || *claims.ActorSubject != actActor {
				t.Fatalf("ActorSubject = %v, want %s", claims.ActorSubject, actActor)
			}
			if claims.Subject != actSubject {
				t.Fatalf("Subject = %s, want the effective subject %s", claims.Subject, actSubject)
			}
		})
	}
}

func TestActAbsent(t *testing.T) {
	claims, _, err := actVerify(t, "dev-health-api", "api:access", "")
	if err != nil {
		t.Fatalf("want accept, got %q", ReasonOf(err))
	}
	if claims.ActorSubject != nil {
		t.Fatalf("ActorSubject = %q, want nil", *claims.ActorSubject)
	}
}

func TestActRefused(t *testing.T) {
	for name, extra := range map[string]string{
		"null":               `,"act":null`,
		"string":             `,"act":"` + actActor + `"`,
		"array":              `,"act":[{"sub":"` + actActor + `"}]`,
		"number":             `,"act":1`,
		"empty_object":       `,"act":{}`,
		"extra_member":       `,"act":{"sub":"` + actActor + `","reason":"support"}`,
		"nested_act":         `,"act":{"sub":"` + actActor + `","act":{"sub":"` + actSubject + `"}}`,
		"sub_integer":        `,"act":{"sub":1}`,
		"sub_usr_prefix":     `,"act":{"sub":"usr_BBBBBBBBBBBBBBBBBBBBBB"}`,
		"sub_empty":          `,"act":{"sub":""}`,
		"repeated_sub":       `,"act":{"sub":"` + actActor + `","sub":"` + actSubject + `"}`,
		"case_variant_sub":   `,"act":{"SUB":"` + actActor + `"}`,
		"case_variant_claim": `,"Act":{"sub":"` + actActor + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			claims, lookups, err := actVerify(t, "dev-health-api", "api:access", extra)
			if claims != nil || ReasonOf(err) != ReasonContract {
				t.Fatalf("want reject contract, got claims %v, reason %q", claims != nil, ReasonOf(err))
			}
			if lookups != 0 {
				t.Fatalf("a contract refusal read %d keys", lookups)
			}
			if strings.Contains(err.Error(), "prn_") {
				t.Fatal("the refusal carries claim bytes")
			}
		})
	}
}
