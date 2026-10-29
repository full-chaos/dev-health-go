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

func exchangeWithIssued(t *testing.T, lifetime time.Duration) (WorkloadTokenExchangeResult, error) {
	t.Helper()
	expiresAt := time.Now().Add(lifetime)
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{identity: SubjectIdentity{ExpiresAt: time.Now().Add(time.Hour)}},
		fakeGrantResolver{binding: WorkloadBinding{BindingID: "wlb_1", GrantedScopes: []string{"a"}}},
		&fakeAccessTokenIssuer{issued: IssuedToken{Token: "opaque_test", ExpiresAt: &expiresAt}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service.Exchange(context.Background(), "subject-token", nil)
}

func TestWorkloadExchangeRefusesAnIssuerPastTheCeiling(t *testing.T) {
	result, err := exchangeWithIssued(t, 10*time.Minute)
	if !errors.Is(err, ErrIssuedLifetimeExceedsCeiling) {
		t.Fatalf("error = %v, want ErrIssuedLifetimeExceedsCeiling", err)
	}
	if result.AccessToken != "" {
		t.Fatal("a refused exchange returned a token")
	}
}

func TestWorkloadExchangeAcceptsALifetimeAtTheCeiling(t *testing.T) {
	result, err := exchangeWithIssued(t, WorkloadTokenLifetimeCeiling-time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresIn <= 0 || time.Duration(result.ExpiresIn)*time.Second > WorkloadTokenLifetimeCeiling {
		t.Fatalf("expires_in = %d, want 1..300", result.ExpiresIn)
	}
}

func TestWorkloadExchangeRefusesOneSecondOverTheCeiling(t *testing.T) {
	if _, err := exchangeWithIssued(t, WorkloadTokenLifetimeCeiling+2*time.Second); !errors.Is(err, ErrIssuedLifetimeExceedsCeiling) {
		t.Fatalf("error = %v, want ErrIssuedLifetimeExceedsCeiling", err)
	}
}
