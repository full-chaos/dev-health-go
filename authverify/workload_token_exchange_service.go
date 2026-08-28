package authverify

import (
	"context"
	"errors"
	"time"
)

// WorkloadTokenExchangeResult is the successful outcome of an RFC 8693
// exchange: the minted opaque access token, its remaining lifetime in
// seconds, and the (possibly narrowed) granted scope.
type WorkloadTokenExchangeResult struct {
	AccessToken string
	ExpiresIn   int
	Scope       []string
}

// WorkloadTokenExchangeService composes the three seams
// (SubjectTokenValidator, GrantResolver, AccessTokenIssuer) into the one
// operation an HTTP handler needs.
type WorkloadTokenExchangeService struct {
	validator SubjectTokenValidator
	resolver  GrantResolver
	issuer    AccessTokenIssuer
}

func NewWorkloadTokenExchangeService(validator SubjectTokenValidator, resolver GrantResolver, issuer AccessTokenIssuer) (*WorkloadTokenExchangeService, error) {
	if validator == nil || resolver == nil || issuer == nil {
		return nil, errors.New("workload token exchange requires a validator, resolver, and issuer")
	}
	return &WorkloadTokenExchangeService{validator: validator, resolver: resolver, issuer: issuer}, nil
}

// Exchange runs the full RFC 8693 flow: validate the subject token
// (TokenReview), resolve its declarative binding, narrow scope, and issue
// an access token capped at the subject token's own expiry. Every error
// this returns is one of ErrSubjectTokenInvalid, ErrWorkloadBindingNotFound,
// or ErrScopeNotGranted, or an opaque infrastructure error -- callers map
// these to their own wire error codes.
func (s *WorkloadTokenExchangeService) Exchange(ctx context.Context, subjectToken string, requestedScope []string) (WorkloadTokenExchangeResult, error) {
	identity, err := s.validator.Validate(ctx, subjectToken)
	if err != nil {
		return WorkloadTokenExchangeResult{}, err
	}
	binding, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		return WorkloadTokenExchangeResult{}, err
	}
	scope, err := ResolveRequestedScope(binding, requestedScope)
	if err != nil {
		return WorkloadTokenExchangeResult{}, err
	}
	issued, err := s.issuer.Issue(ctx, binding, scope, identity.ExpiresAt)
	if err != nil {
		return WorkloadTokenExchangeResult{}, err
	}
	expiresIn := 0
	if issued.ExpiresAt != nil {
		if remaining := time.Until(*issued.ExpiresAt); remaining > 0 {
			expiresIn = int(remaining / time.Second)
		}
	}
	return WorkloadTokenExchangeResult{AccessToken: issued.Token, ExpiresIn: expiresIn, Scope: scope}, nil
}
