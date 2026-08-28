package authverify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSubjectJWT builds a syntactically valid (unsigned) JWT with the given
// exp claim, matching what unverifiedJWTExpiry decodes. Its signature is
// never checked by this package -- TokenReview is the sole authority --
// but a two-dot three-segment shape is still required.
func fakeSubjectJWT(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, err := json.Marshal(map[string]any{"exp": expiresAt.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return header + "." + payload + ".sig"
}

func newTokenReviewServer(t *testing.T, respond func(w http.ResponseWriter, decoded tokenReviewRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenReviewPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer reviewer-token" {
			t.Fatalf("Authorization header = %q, want the reviewer token", got)
		}
		var decoded tokenReviewRequest
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		respond(w, decoded)
	}))
}

func newValidator(t *testing.T, serverURL string, now func() time.Time) *KubernetesTokenReviewValidator {
	t.Helper()
	validator, err := NewKubernetesTokenReviewValidator(KubernetesTokenReviewOptions{
		APIServerURL: serverURL, Audience: "dev-health-acr-token-exchange", TrustDomain: "cluster.local",
		ReviewerToken: func() (string, error) { return "reviewer-token", nil }, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func TestKubernetesTokenReviewValidator_authenticatedGrantsIdentity(t *testing.T) {
	fixedNow := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	server := newTokenReviewServer(t, func(w http.ResponseWriter, decoded tokenReviewRequest) {
		if decoded.Spec.Audiences[0] != "dev-health-acr-token-exchange" {
			t.Fatalf("requested audience = %v", decoded.Spec.Audiences)
		}
		var response tokenReviewResponse
		response.Status.Authenticated = true
		response.Status.Audiences = []string{"dev-health-acr-token-exchange"}
		response.Status.User.Username = "system:serviceaccount:panel-ns:panel-read"
		response.Status.User.UID = "sa-uid-123"
		_ = json.NewEncoder(w).Encode(response)
	})
	defer server.Close()

	validator := newValidator(t, server.URL, func() time.Time { return fixedNow })
	subjectToken := fakeSubjectJWT(t, fixedNow.Add(time.Hour))
	identity, err := validator.Validate(context.Background(), subjectToken)
	if err != nil {
		t.Fatal(err)
	}
	if identity.TrustDomain != "cluster.local" || identity.Namespace != "panel-ns" || identity.ServiceAccountName != "panel-read" || identity.ServiceAccountUID != "sa-uid-123" {
		t.Fatalf("identity = %#v", identity)
	}
	if !identity.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("expiresAt = %v", identity.ExpiresAt)
	}
}

func TestKubernetesTokenReviewValidator_unauthenticatedIsRejected(t *testing.T) {
	server := newTokenReviewServer(t, func(w http.ResponseWriter, _ tokenReviewRequest) {
		var response tokenReviewResponse
		response.Status.Authenticated = false
		_ = json.NewEncoder(w).Encode(response)
	})
	defer server.Close()

	validator := newValidator(t, server.URL, nil)
	_, err := validator.Validate(context.Background(), fakeSubjectJWT(t, time.Now().Add(time.Hour)))
	if !errors.Is(err, ErrSubjectTokenInvalid) {
		t.Fatalf("error = %v, want ErrSubjectTokenInvalid", err)
	}
}

func TestKubernetesTokenReviewValidator_wrongAudienceIsRejected(t *testing.T) {
	server := newTokenReviewServer(t, func(w http.ResponseWriter, _ tokenReviewRequest) {
		var response tokenReviewResponse
		response.Status.Authenticated = true
		response.Status.Audiences = []string{"some-other-audience"}
		response.Status.User.Username = "system:serviceaccount:ns:sa"
		response.Status.User.UID = "uid"
		_ = json.NewEncoder(w).Encode(response)
	})
	defer server.Close()

	validator := newValidator(t, server.URL, nil)
	_, err := validator.Validate(context.Background(), fakeSubjectJWT(t, time.Now().Add(time.Hour)))
	if !errors.Is(err, ErrSubjectTokenInvalid) {
		t.Fatalf("error = %v, want ErrSubjectTokenInvalid", err)
	}
}

func TestKubernetesTokenReviewValidator_malformedUsernameIsRejected(t *testing.T) {
	server := newTokenReviewServer(t, func(w http.ResponseWriter, _ tokenReviewRequest) {
		var response tokenReviewResponse
		response.Status.Authenticated = true
		response.Status.Audiences = []string{"dev-health-acr-token-exchange"}
		response.Status.User.Username = "not-a-service-account"
		response.Status.User.UID = "uid"
		_ = json.NewEncoder(w).Encode(response)
	})
	defer server.Close()

	validator := newValidator(t, server.URL, nil)
	_, err := validator.Validate(context.Background(), fakeSubjectJWT(t, time.Now().Add(time.Hour)))
	if !errors.Is(err, ErrSubjectTokenInvalid) {
		t.Fatalf("error = %v, want ErrSubjectTokenInvalid", err)
	}
}

func TestKubernetesTokenReviewValidator_expiredSubjectTokenIsRejected(t *testing.T) {
	fixedNow := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	server := newTokenReviewServer(t, func(w http.ResponseWriter, _ tokenReviewRequest) {
		var response tokenReviewResponse
		response.Status.Authenticated = true
		response.Status.Audiences = []string{"dev-health-acr-token-exchange"}
		response.Status.User.Username = "system:serviceaccount:ns:sa"
		response.Status.User.UID = "uid"
		_ = json.NewEncoder(w).Encode(response)
	})
	defer server.Close()

	validator := newValidator(t, server.URL, func() time.Time { return fixedNow })
	subjectToken := fakeSubjectJWT(t, fixedNow.Add(-time.Minute))
	_, err := validator.Validate(context.Background(), subjectToken)
	if !errors.Is(err, ErrSubjectTokenInvalid) {
		t.Fatalf("error = %v, want ErrSubjectTokenInvalid", err)
	}
}

func TestKubernetesTokenReviewValidator_emptySubjectTokenIsRejectedWithoutARequest(t *testing.T) {
	server := newTokenReviewServer(t, func(http.ResponseWriter, tokenReviewRequest) {
		t.Fatal("no request should be sent for an empty subject token")
	})
	defer server.Close()

	validator := newValidator(t, server.URL, nil)
	if _, err := validator.Validate(context.Background(), ""); !errors.Is(err, ErrSubjectTokenInvalid) {
		t.Fatalf("error = %v, want ErrSubjectTokenInvalid", err)
	}
}

func TestKubernetesTokenReviewValidator_oversizedResponseIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxTokenReviewResponseBytes+1)))
	}))
	defer server.Close()

	validator := newValidator(t, server.URL, nil)
	if _, err := validator.Validate(context.Background(), fakeSubjectJWT(t, time.Now().Add(time.Hour))); err == nil {
		t.Fatal("expected an error for an oversized response")
	}
}

func TestNewKubernetesTokenReviewValidator_rejectsAPlainHTTPNonLoopbackAPIServerURL(t *testing.T) {
	_, err := NewKubernetesTokenReviewValidator(KubernetesTokenReviewOptions{
		APIServerURL: "http://kubernetes.example.com", Audience: "aud", TrustDomain: "cluster.local",
		ReviewerToken: func() (string, error) { return "t", nil },
	})
	if err == nil {
		t.Fatal("expected an error for a plain-http, non-loopback api server url -- the reviewer token and subject token must never be sendable in plaintext to an arbitrary host")
	}
}

func TestKubernetesTokenReviewValidator_refusesARedirectResponse(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the redirect target must never be reached: CheckRedirect should refuse the redirect first")
	}))
	defer redirectTarget.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	validator := newValidator(t, origin.URL, nil)
	if _, err := validator.Validate(context.Background(), fakeSubjectJWT(t, time.Now().Add(time.Hour))); err == nil {
		t.Fatal("expected an error: the redirect must be refused, not followed")
	}
}

func TestNewKubernetesTokenReviewValidator_requiresAllFields(t *testing.T) {
	base := KubernetesTokenReviewOptions{
		APIServerURL: "https://kubernetes.default.svc", Audience: "aud", TrustDomain: "cluster.local",
		ReviewerToken: func() (string, error) { return "t", nil },
	}
	cases := []func(KubernetesTokenReviewOptions) KubernetesTokenReviewOptions{
		func(o KubernetesTokenReviewOptions) KubernetesTokenReviewOptions { o.APIServerURL = ""; return o },
		func(o KubernetesTokenReviewOptions) KubernetesTokenReviewOptions { o.ReviewerToken = nil; return o },
		func(o KubernetesTokenReviewOptions) KubernetesTokenReviewOptions { o.Audience = ""; return o },
		func(o KubernetesTokenReviewOptions) KubernetesTokenReviewOptions { o.TrustDomain = ""; return o },
	}
	for i, mutate := range cases {
		if _, err := NewKubernetesTokenReviewValidator(mutate(base)); err == nil {
			t.Fatalf("case %d: expected an error", i)
		}
	}
}
