package authverify

import (
	"path/filepath"
	"testing"
	"time"
)

// TestEachAudienceVerifierAcceptsOnlyItsOwnAudience (CHAOS-12249): over the
// corpus's four valid tokens (one per access-token.v1 audience, each differing
// from valid-api in aud and scope only), a verifier for each audience accepts
// exactly its own token and refuses the other three for the audience. So a
// verifier for acr-api refuses the acr-mcp and dev-health-api tokens, a
// verifier for acr-mcp the reverse, and the platform verifiers (dev-health-api,
// auth-service) refuse both ACR tokens.
func TestEachAudienceVerifierAcceptsOnlyItsOwnAudience(t *testing.T) {
	m := loadManifest(t)
	tokens := map[string]string{
		"dev-health-api": "valid-api.json",
		"auth-service":   "valid-auth-service.json",
		"acr-api":        "valid-acr-api.json",
		"acr-mcp":        "valid-acr-mcp.json",
	}
	now := map[string]int64{}
	for _, row := range m.Vectors {
		for _, file := range tokens {
			if row.File == file {
				now[file] = row.Now
			}
		}
	}
	for verifierAudience := range tokens {
		v, err := NewVerifier(VerifierConfig{Issuer: m.Issuer, Audience: verifierAudience, Keys: corpusKeys(t, m, false)})
		if err != nil {
			t.Fatalf("verifier for %s: %v", verifierAudience, err)
		}
		for tokenAudience, file := range tokens {
			at, ok := now[file]
			if !ok {
				t.Fatalf("%s is not a manifest row", file)
			}
			var vec corpusVector
			readJSON(t, filepath.Join(corpusDir, "vectors", file), &vec)
			claims, err := v.Verify(vec.compact(), time.Unix(at, 0))
			if tokenAudience == verifierAudience {
				if err != nil || claims == nil || claims.Audience != verifierAudience {
					t.Errorf("verifier %s refused its own token: %v", verifierAudience, ReasonOf(err))
				}
				continue
			}
			if claims != nil || ReasonOf(err) != ReasonAudience {
				t.Errorf("verifier %s on the %s token: claims %v, reason %q; want an audience refusal", verifierAudience, tokenAudience, claims != nil, ReasonOf(err))
			}
		}
	}
}

// TestAVerifierIsBuiltForTheFourAccessTokenAudiencesOnly: NewVerifier builds
// for dev-health-api, auth-service, acr-api and acr-mcp, and still refuses any
// other audience.
func TestAVerifierIsBuiltForTheFourAccessTokenAudiencesOnly(t *testing.T) {
	config := VerifierConfig{Issuer: "https://auth.example.test", Keys: StaticKeys{}}
	for _, audience := range []string{"dev-health-api", "auth-service", "acr-api", "acr-mcp"} {
		config.Audience = audience
		if _, err := NewVerifier(config); err != nil {
			t.Errorf("%s: %v", audience, err)
		}
	}
	for _, audience := range []string{"", "query-api", "acr", "acr-web", "ACR-API", "acr-api ", "dev-health-workload"} {
		config.Audience = audience
		if _, err := NewVerifier(config); err == nil {
			t.Errorf("a verifier was built for %q", audience)
		}
	}
}
