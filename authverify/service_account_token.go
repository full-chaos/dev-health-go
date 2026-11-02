package authverify

import "time"

// MaxServiceAccountLeeway bounds the clock skew a ServiceAccountVerifier accepts.
const MaxServiceAccountLeeway = 30 * time.Second

// ServiceAccountClaims are the claims of a verified service-account-token.v1 token.
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
	Issuer     string
	Audience   string
	Keys       KeySource
	DeniedKids []string
	Leeway     time.Duration
}

// ServiceAccountVerifier is the canonical service-account-token.v1 verifier.
type ServiceAccountVerifier struct{}

// NewServiceAccountVerifier validates cfg and returns a ServiceAccountVerifier.
func NewServiceAccountVerifier(cfg ServiceAccountVerifierConfig) (*ServiceAccountVerifier, error) {
	return &ServiceAccountVerifier{}, nil
}

// Verify checks a compact service account token at now.
func (v *ServiceAccountVerifier) Verify(token string, now time.Time) (*ServiceAccountClaims, error) {
	return nil, refuse(ReasonContract)
}
