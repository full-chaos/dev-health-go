package authverify

import (
	"context"
	"errors"
	"time"
)

// WorkloadAccessTokenLifetime is the RFC 8693 access token lifetime for a
// workload-exchanged token: a short TTL capped at the subject token's own
// expiry, with no refresh tokens -- callers re-exchange the projected
// token instead.
const WorkloadAccessTokenLifetime = 10 * time.Minute

var (
	// ErrSubjectTokenInvalid is returned by a SubjectTokenValidator (and
	// wraps any failure downstream of it) for a subject_token that fails
	// validation: expired, wrong audience, not authenticated, or otherwise
	// rejected by Kubernetes TokenReview.
	ErrSubjectTokenInvalid = errors.New("subject token failed validation")
	// ErrWorkloadBindingNotFound is returned when a validated subject
	// identity resolves to no binding, or to a disabled one. It is
	// deliberately indistinguishable from "not found" to a caller: a
	// disabled binding must not leak its own existence.
	ErrWorkloadBindingNotFound = errors.New("workload binding not found or disabled")
	// ErrScopeNotGranted is returned when a request's scope parameter asks
	// for more than the resolved binding grants -- RFC 8693 scope may only
	// narrow, never widen, a grant.
	ErrScopeNotGranted = errors.New("requested scope exceeds the workload binding's grant")
)

// SubjectIdentity is the validated k8s ServiceAccount identity a subject
// token asserts, established ONLY via Kubernetes TokenReview -- never by
// decoding the JWT's own claims directly, which would trust a value the
// API server has not itself vouched for.
type SubjectIdentity struct {
	TrustDomain        string
	Namespace          string
	ServiceAccountName string
	ServiceAccountUID  string
	// ExpiresAt is the subject token's own expiry, used to cap the issued
	// access token's lifetime at WorkloadAccessTokenLifetime (see
	// AccessTokenIssuer).
	ExpiresAt time.Time
}

// SubjectTokenValidator validates an RFC 8693 subject_token and returns
// the SubjectIdentity it asserts. KubernetesTokenReviewValidator is the
// only production implementation; this seam exists so a future control
// plane can supply a different validator without the token-exchange
// handler changing.
type SubjectTokenValidator interface {
	Validate(ctx context.Context, subjectToken string) (SubjectIdentity, error)
}

// WorkloadBinding is the resolved grant for a validated subject identity:
// which organization it belongs to, which scopes it has been granted, and
// which repositories it may read. GrantedScopes is caller-defined --
// this package carries no opinion on scope vocabulary or role-to-scope
// policy; each caller (acr, query-api, ...) resolves its own scopes and
// passes them in.
type WorkloadBinding struct {
	BindingID        string
	OrgID            string
	GrantedScopes    []string
	RepositoryScopes []string
}

// GrantResolver resolves a validated SubjectIdentity to its declarative
// WorkloadBinding. Implementations must resolve ONLY from the {trust
// domain, namespace, service account name, service account uid} tuple --
// never from anything request-supplied.
type GrantResolver interface {
	Resolve(ctx context.Context, identity SubjectIdentity) (WorkloadBinding, error)
}

// IssuedToken is the neutral result of minting an access token for a
// resolved workload binding -- just the opaque token and its expiry, with
// no dependency on any particular credential wire schema.
type IssuedToken struct {
	Token     string
	ExpiresAt *time.Time
}

// AccessTokenIssuer issues an opaque access token for a resolved workload
// binding.
type AccessTokenIssuer interface {
	// Issue mints a token scoped to scope (already resolved/narrowed
	// against binding by the caller -- see ResolveRequestedScope),
	// expiring at min(now+WorkloadAccessTokenLifetime, subjectExpiresAt).
	Issue(ctx context.Context, binding WorkloadBinding, scope []string, subjectExpiresAt time.Time) (IssuedToken, error)
}

// ResolveRequestedScope narrows binding.GrantedScopes by an RFC 8693
// requested scope list. RFC 8693 scope may only narrow a grant, never
// widen it: an empty requested list returns the binding's full granted
// scope unchanged; a non-empty list must be a non-empty subset of it, or
// ErrScopeNotGranted. An empty GrantedScopes set (e.g. an unrecognized
// binding) is treated as ErrWorkloadBindingNotFound rather than granting
// nothing silently.
func ResolveRequestedScope(binding WorkloadBinding, requested []string) ([]string, error) {
	granted := binding.GrantedScopes
	if len(granted) == 0 {
		return nil, ErrWorkloadBindingNotFound
	}
	if len(requested) == 0 {
		return granted, nil
	}
	grantedSet := make(map[string]struct{}, len(granted))
	for _, scope := range granted {
		grantedSet[scope] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	narrowed := make([]string, 0, len(requested))
	for _, scope := range requested {
		if _, ok := grantedSet[scope]; !ok {
			return nil, ErrScopeNotGranted
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}
		narrowed = append(narrowed, scope)
	}
	return narrowed, nil
}
