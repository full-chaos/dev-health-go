package authverify

import (
	"context"
	"encoding/base64"
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
// ErrScopeNotGranted, ErrIssuedLifetimeExceedsCeiling, ErrIssuedLifetimeUnknown, ErrIssuedLifetimeMismatch, or an opaque infrastructure error -- callers map
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
	if err := checkIssuedLifetime(issued); err != nil {
		return WorkloadTokenExchangeResult{}, err
	}
	expiresIn := 0
	if remaining := time.Until(*issued.ExpiresAt); remaining > 0 {
		expiresIn = int(remaining / time.Second)
	}
	return WorkloadTokenExchangeResult{AccessToken: issued.Token, ExpiresIn: expiresIn, Scope: scope}, nil
}

// checkIssuedLifetime holds an issued token to WorkloadTokenLifetimeCeiling,
// reading the token's own iat and exp when it is a compact JWT and the issuer's
// metadata otherwise (see IssuedToken).
func checkIssuedLifetime(issued IssuedToken) error {
	if issued.IssuedAt == nil || issued.ExpiresAt == nil || !issued.ExpiresAt.After(*issued.IssuedAt) {
		return ErrIssuedLifetimeUnknown
	}
	lifetime := issued.ExpiresAt.Sub(*issued.IssuedAt)
	if _, payload, _, jwtShaped := splitCompact(issued.Token); jwtShaped {
		iat, exp, ok := tokenLifetimeClaims(payload)
		if !ok {
			return ErrIssuedLifetimeUnknown
		}
		if exp-iat > int64(WorkloadTokenLifetimeCeiling/time.Second) {
			return ErrIssuedLifetimeExceedsCeiling
		}
		if iat != issued.IssuedAt.Unix() || exp != issued.ExpiresAt.Unix() {
			return ErrIssuedLifetimeMismatch
		}
		return nil
	}
	if lifetime > WorkloadTokenLifetimeCeiling {
		return ErrIssuedLifetimeExceedsCeiling
	}
	return nil
}

func tokenLifetimeClaims(payloadPart string) (iat, exp int64, ok bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payloadPart)
	if err != nil {
		return 0, 0, false
	}
	object, ok := decodeObject(raw)
	if !ok {
		return 0, 0, false
	}
	iat, okIat := numericDate(object, "iat")
	exp, okExp := numericDate(object, "exp")
	if !okIat || !okExp || exp <= iat {
		return 0, 0, false
	}
	return iat, exp, true
}
