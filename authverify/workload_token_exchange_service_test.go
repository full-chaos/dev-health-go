package authverify

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSubjectTokenValidator struct {
	identity SubjectIdentity
	err      error
}

func (f fakeSubjectTokenValidator) Validate(context.Context, string) (SubjectIdentity, error) {
	return f.identity, f.err
}

type fakeGrantResolver struct {
	binding WorkloadBinding
	err     error
}

func (f fakeGrantResolver) Resolve(context.Context, SubjectIdentity) (WorkloadBinding, error) {
	return f.binding, f.err
}

type fakeAccessTokenIssuer struct {
	issued IssuedToken
	err    error
	// lastScope/lastSubjectExpiry record the arguments Issue was called
	// with, so a test can assert the composed service passed the resolved
	// scope and identity expiry through unchanged.
	lastScope         []string
	lastSubjectExpiry time.Time
}

func (f *fakeAccessTokenIssuer) Issue(_ context.Context, _ WorkloadBinding, scope []string, subjectExpiresAt time.Time) (IssuedToken, error) {
	f.lastScope = scope
	f.lastSubjectExpiry = subjectExpiresAt
	return f.issued, f.err
}

func TestWorkloadTokenExchangeService_happyPath(t *testing.T) {
	// Exchange computes ExpiresIn via time.Until against the real wall
	// clock (it has no injectable clock of its own), so this fixture
	// anchors to time.Now(), not a fixed date, to stay reproducible
	// regardless of when the test runs.
	now := time.Now()
	subjectExpiry := now.Add(30 * time.Minute)
	expiresAt := now.Add(10 * time.Minute)
	issuer := &fakeAccessTokenIssuer{issued: IssuedToken{Token: "opaque_test", ExpiresAt: &expiresAt}}
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{identity: SubjectIdentity{ExpiresAt: subjectExpiry}},
		fakeGrantResolver{binding: WorkloadBinding{BindingID: "wlb_1", OrgID: "org1", GrantedScopes: []string{"a", "b", "c"}}},
		issuer,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Exchange(context.Background(), "subject-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "opaque_test" {
		t.Fatalf("access token = %q", result.AccessToken)
	}
	if result.ExpiresIn <= 0 || time.Duration(result.ExpiresIn)*time.Second > WorkloadAccessTokenLifetime {
		t.Fatalf("expires_in = %d, want a positive value capped at %s", result.ExpiresIn, WorkloadAccessTokenLifetime)
	}
	if len(result.Scope) != 3 {
		t.Fatalf("scope = %#v, want the full grant (no narrowing requested)", result.Scope)
	}
	if !issuer.lastSubjectExpiry.Equal(subjectExpiry) {
		t.Fatalf("issuer received subject expiry %v, want %v", issuer.lastSubjectExpiry, subjectExpiry)
	}
}

func TestWorkloadTokenExchangeService_propagatesSubjectTokenValidationFailure(t *testing.T) {
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{err: ErrSubjectTokenInvalid},
		fakeGrantResolver{},
		&fakeAccessTokenIssuer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Exchange(context.Background(), "bad-token", nil); !errors.Is(err, ErrSubjectTokenInvalid) {
		t.Fatalf("error = %v, want ErrSubjectTokenInvalid", err)
	}
}

func TestWorkloadTokenExchangeService_propagatesUnresolvedBinding(t *testing.T) {
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{},
		fakeGrantResolver{err: ErrWorkloadBindingNotFound},
		&fakeAccessTokenIssuer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Exchange(context.Background(), "token", nil); !errors.Is(err, ErrWorkloadBindingNotFound) {
		t.Fatalf("error = %v, want ErrWorkloadBindingNotFound", err)
	}
}

func TestWorkloadTokenExchangeService_propagatesScopeNarrowingFailure(t *testing.T) {
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{},
		fakeGrantResolver{binding: WorkloadBinding{GrantedScopes: []string{"a"}}},
		&fakeAccessTokenIssuer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Exchange(context.Background(), "token", []string{"b"}); !errors.Is(err, ErrScopeNotGranted) {
		t.Fatalf("error = %v, want ErrScopeNotGranted", err)
	}
}

func TestNewWorkloadTokenExchangeService_requiresAllThreeSeams(t *testing.T) {
	validator := fakeSubjectTokenValidator{}
	resolver := fakeGrantResolver{}
	issuer := &fakeAccessTokenIssuer{}
	if _, err := NewWorkloadTokenExchangeService(nil, resolver, issuer); err == nil {
		t.Fatal("expected an error for a nil validator")
	}
	if _, err := NewWorkloadTokenExchangeService(validator, nil, issuer); err == nil {
		t.Fatal("expected an error for a nil resolver")
	}
	if _, err := NewWorkloadTokenExchangeService(validator, resolver, nil); err == nil {
		t.Fatal("expected an error for a nil issuer")
	}
}
