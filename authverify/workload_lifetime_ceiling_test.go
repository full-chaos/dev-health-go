package authverify

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkloadLifetimeConstantsShareOneCeiling(t *testing.T) {
	if WorkloadTokenLifetimeCeiling != 300*time.Second {
		t.Fatalf("WorkloadTokenLifetimeCeiling = %s, want 300s", WorkloadTokenLifetimeCeiling)
	}
	if WorkloadAccessTokenLifetime > WorkloadTokenLifetimeCeiling {
		t.Fatalf("WorkloadAccessTokenLifetime %s exceeds the ceiling %s", WorkloadAccessTokenLifetime, WorkloadTokenLifetimeCeiling)
	}
	if time.Duration(workloadLifetimeCeiling)*time.Second != WorkloadTokenLifetimeCeiling {
		t.Fatalf("verifier ceiling %ds differs from WorkloadTokenLifetimeCeiling %s", workloadLifetimeCeiling, WorkloadTokenLifetimeCeiling)
	}
}

func exchangeIssued(t *testing.T, issuedAt, expiresAt *time.Time) (WorkloadTokenExchangeResult, error) {
	t.Helper()
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{identity: SubjectIdentity{ExpiresAt: time.Now().Add(time.Hour)}},
		fakeGrantResolver{binding: WorkloadBinding{BindingID: "wlb_1", GrantedScopes: []string{"a"}}},
		&fakeAccessTokenIssuer{issued: IssuedToken{Token: "opaque_test", IssuedAt: issuedAt, ExpiresAt: expiresAt}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service.Exchange(context.Background(), "subject-token", nil)
}

func exchangeLifetime(t *testing.T, issuedAgo, lifetime time.Duration) (WorkloadTokenExchangeResult, error) {
	t.Helper()
	issuedAt := time.Now().Add(-issuedAgo)
	expiresAt := issuedAt.Add(lifetime)
	return exchangeIssued(t, &issuedAt, &expiresAt)
}

func TestWorkloadExchangeRefusesAnIssuerPastTheCeiling(t *testing.T) {
	result, err := exchangeLifetime(t, 0, 10*time.Minute)
	if !errors.Is(err, ErrIssuedLifetimeExceedsCeiling) {
		t.Fatalf("error = %v, want ErrIssuedLifetimeExceedsCeiling", err)
	}
	if result.AccessToken != "" {
		t.Fatal("a refused exchange returned a token")
	}
}

func TestWorkloadExchangeLifetimeBoundaryIsExact(t *testing.T) {
	if _, err := exchangeLifetime(t, 0, WorkloadTokenLifetimeCeiling); err != nil {
		t.Fatalf("exp-iat = 300s refused: %v", err)
	}
	if _, err := exchangeLifetime(t, 0, WorkloadTokenLifetimeCeiling+time.Second); !errors.Is(err, ErrIssuedLifetimeExceedsCeiling) {
		t.Fatalf("exp-iat = 301s: error = %v, want ErrIssuedLifetimeExceedsCeiling", err)
	}
}

func TestWorkloadExchangeJudgesTheTokenNotTheTimeLeftAfterIssue(t *testing.T) {
	// A slow issuer: the token lives 302 s but 3 s passed before Issue returned, so 299 s remain.
	if _, err := exchangeLifetime(t, 3*time.Second, WorkloadTokenLifetimeCeiling+2*time.Second); !errors.Is(err, ErrIssuedLifetimeExceedsCeiling) {
		t.Fatalf("error = %v, want ErrIssuedLifetimeExceedsCeiling", err)
	}
}

func TestWorkloadExchangeFailsClosedOnMissingLifetimeMetadata(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Minute)
	for name, tc := range map[string]struct{ iat, exp *time.Time }{
		"no expiry":         {iat: &now, exp: nil},
		"no issued-at":      {iat: nil, exp: &exp},
		"neither":           {iat: nil, exp: nil},
		"expiry equals iat": {iat: &now, exp: &now},
		"expiry before iat": {iat: &exp, exp: &now},
	} {
		result, err := exchangeIssued(t, tc.iat, tc.exp)
		if !errors.Is(err, ErrIssuedLifetimeUnknown) {
			t.Fatalf("%s: error = %v, want ErrIssuedLifetimeUnknown", name, err)
		}
		if result.AccessToken != "" {
			t.Fatalf("%s: a refused exchange returned a token", name)
		}
	}
}
