package authverify

import "time"

// MaxWorkloadLeeway bounds the clock skew a WorkloadVerifier accepts.
const MaxWorkloadLeeway = 30 * time.Second

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
	Issuer     string
	Audience   string
	Keys       KeySource
	DeniedKids []string
	Leeway     time.Duration
}

// WorkloadVerifier is the canonical workload-token.v1 verifier.
type WorkloadVerifier struct{}

// NewWorkloadVerifier validates cfg and returns a WorkloadVerifier.
func NewWorkloadVerifier(cfg WorkloadVerifierConfig) (*WorkloadVerifier, error) {
	return &WorkloadVerifier{}, nil
}

// Verify checks a compact workload token at now and returns its claims.
func (v *WorkloadVerifier) Verify(token string, now time.Time) (*WorkloadClaims, error) {
	return nil, refuse(ReasonContract)
}
