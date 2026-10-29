package authverify

import (
	"context"
	"errors"
	"testing"
	"time"
)

type jwtCell struct {
	tokenLifetime int64
	metaLifetime  int64
}

func exchangeSignedJWT(t *testing.T, cell jwtCell) (WorkloadTokenExchangeResult, error, error) {
	t.Helper()
	s := newSigner(t)
	iat := time.Now().Unix()
	token := s.token(workloadHeader("wl-a"), workloadPayload(iat, iat, iat+cell.tokenLifetime))
	issuedAt := time.Unix(iat, 0)
	expiresAt := time.Unix(iat+cell.metaLifetime, 0)
	service, err := NewWorkloadTokenExchangeService(
		fakeSubjectTokenValidator{identity: SubjectIdentity{ExpiresAt: time.Now().Add(time.Hour)}},
		fakeGrantResolver{binding: WorkloadBinding{BindingID: "wlb_1", GrantedScopes: []string{"a"}}},
		&fakeAccessTokenIssuer{issued: IssuedToken{Token: token, IssuedAt: &issuedAt, ExpiresAt: &expiresAt}},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, exchangeErr := service.Exchange(context.Background(), "subject-token", nil)
	_, verifyErr := rtVerifier(t, StaticKeys{"wl-a": s.pub}, 0).Verify(token, issuedAt)
	return result, exchangeErr, verifyErr
}

func TestWorkloadExchangeAndVerifierAgreeOnASignedToken(t *testing.T) {
	if result, exchangeErr, verifyErr := exchangeSignedJWT(t, jwtCell{300, 300}); exchangeErr != nil || verifyErr != nil || result.AccessToken == "" {
		t.Fatalf("300 s: exchange = %v, verifier = %v", exchangeErr, verifyErr)
	}
	result, exchangeErr, verifyErr := exchangeSignedJWT(t, jwtCell{301, 301})
	if !errors.Is(exchangeErr, ErrIssuedLifetimeExceedsCeiling) || ReasonOf(verifyErr) != ReasonContract {
		t.Fatalf("301 s: exchange = %v, verifier = %q; both must refuse", exchangeErr, ReasonOf(verifyErr))
	}
	if result.AccessToken != "" {
		t.Fatal("a refused exchange returned a token")
	}
}

func TestWorkloadExchangeReadsTheSignedTokenNotTheMetadata(t *testing.T) {
	_, exchangeErr, verifyErr := exchangeSignedJWT(t, jwtCell{tokenLifetime: 302, metaLifetime: 300})
	if !errors.Is(exchangeErr, ErrIssuedLifetimeExceedsCeiling) || ReasonOf(verifyErr) != ReasonContract {
		t.Fatalf("token 302 s, metadata 300 s: exchange = %v, verifier = %q; both must refuse", exchangeErr, ReasonOf(verifyErr))
	}
}

func TestWorkloadExchangeRefusesMetadataThatDisagreesWithTheToken(t *testing.T) {
	if _, exchangeErr, _ := exchangeSignedJWT(t, jwtCell{tokenLifetime: 300, metaLifetime: 200}); !errors.Is(exchangeErr, ErrIssuedLifetimeMismatch) {
		t.Fatalf("token 300 s, metadata 200 s: error = %v, want ErrIssuedLifetimeMismatch", exchangeErr)
	}
	if _, exchangeErr, _ := exchangeSignedJWT(t, jwtCell{tokenLifetime: 200, metaLifetime: 300}); !errors.Is(exchangeErr, ErrIssuedLifetimeMismatch) {
		t.Fatalf("token 200 s, metadata 300 s: error = %v, want ErrIssuedLifetimeMismatch", exchangeErr)
	}
}

func TestWorkloadExchangeFailsClosedOnAJWTShapedTokenWithoutReadableClaims(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Minute)
	for name, token := range map[string]string{
		"payload not json":      "aaaa.bbbb.cccc",
		"payload no dates":      newSigner(t).token(workloadHeader("wl-a"), `{"iss":"x"}`),
		"payload not an object": newSigner(t).token(workloadHeader("wl-a"), `[1,2]`),
	} {
		service, err := NewWorkloadTokenExchangeService(
			fakeSubjectTokenValidator{identity: SubjectIdentity{ExpiresAt: now.Add(time.Hour)}},
			fakeGrantResolver{binding: WorkloadBinding{BindingID: "wlb_1", GrantedScopes: []string{"a"}}},
			&fakeAccessTokenIssuer{issued: IssuedToken{Token: token, IssuedAt: &now, ExpiresAt: &exp}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Exchange(context.Background(), "subject-token", nil); !errors.Is(err, ErrIssuedLifetimeUnknown) {
			t.Fatalf("%s: error = %v, want ErrIssuedLifetimeUnknown", name, err)
		}
	}
}
