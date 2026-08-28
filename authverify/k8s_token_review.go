package authverify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tokenReviewPath is the fixed Kubernetes TokenReview API path. Never
// caller-supplied.
const tokenReviewPath = "/apis/authentication.k8s.io/v1/tokenreviews"

// maxTokenReviewResponseBytes bounds how much of a TokenReview response
// body this client will read. A real response is well under 4 KiB; this
// exists to bound memory against a misbehaving or compromised API server,
// not to accommodate legitimate growth.
const maxTokenReviewResponseBytes = 64 << 10 // 64 KiB

type tokenReviewRequest struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Spec       tokenReviewRequestSpec `json:"spec"`
}

type tokenReviewRequestSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences"`
}

type tokenReviewResponse struct {
	Status struct {
		Authenticated bool     `json:"authenticated"`
		Error         string   `json:"error"`
		Audiences     []string `json:"audiences"`
		User          struct {
			Username string `json:"username"`
			UID      string `json:"uid"`
		} `json:"user"`
	} `json:"status"`
}

// KubernetesTokenReviewOptions configures KubernetesTokenReviewValidator.
type KubernetesTokenReviewOptions struct {
	// HTTPClient is the client used for the TokenReview call. Nil uses
	// http.DefaultClient.
	HTTPClient *http.Client
	// APIServerURL is the Kubernetes API server origin, e.g.
	// "https://kubernetes.default.svc". Required.
	APIServerURL string
	// ReviewerToken returns the caller's OWN bearer token, authorized (via
	// TokenReview RBAC: create on tokenreviews only) to call TokenReview.
	// Invoked once per Validate call so a kubelet-rotated projected token
	// is always current.
	ReviewerToken func() (string, error)
	// Audience is the fixed audience TokenReview must confirm the
	// presented token was minted for. Required.
	Audience string
	// TrustDomain is a fixed, deployment-configured identifier for this
	// single Kubernetes cluster. Plain k8s ServiceAccount tokens (unlike
	// full SPIFFE federation) carry no trust-domain claim TokenReview
	// exposes, so this is NOT derived from the token or the TokenReview
	// response -- it is a static value this deployment's operator
	// configures once. Required.
	TrustDomain string
	Now         func() time.Time
}

// KubernetesTokenReviewValidator is the production SubjectTokenValidator:
// it validates a k8s projected ServiceAccount JWT SOLELY via the
// Kubernetes TokenReview API, never by decoding or trusting the token's
// own claims for authentication (the audience/authenticated/username/uid
// fields all come from TokenReview's own response, which reflects the API
// server's live validation against the cluster's actual signing keys and
// current ServiceAccount state -- including revocation, which a purely
// local JWT signature check could never see).
type KubernetesTokenReviewValidator struct {
	http          *http.Client
	apiServerURL  string
	reviewerToken func() (string, error)
	audience      string
	trustDomain   string
	now           func() time.Time
}

func NewKubernetesTokenReviewValidator(options KubernetesTokenReviewOptions) (*KubernetesTokenReviewValidator, error) {
	if strings.TrimSpace(options.APIServerURL) == "" {
		return nil, errors.New("kubernetes token review: api server url is required")
	}
	parsedAPIServerURL, err := url.Parse(options.APIServerURL)
	if err != nil || parsedAPIServerURL.Host == "" {
		return nil, errors.New("kubernetes token review: api server url is malformed")
	}
	// Both the reviewer token (Authorization header) and the workload's
	// subject token (request body) cross this connection -- it must never
	// be plaintext or redirectable to an unexpected origin.
	loopback := parsedAPIServerURL.Hostname() == "localhost" || func() bool {
		ip := net.ParseIP(parsedAPIServerURL.Hostname())
		return ip != nil && ip.IsLoopback()
	}()
	if parsedAPIServerURL.Scheme != "https" && !loopback {
		return nil, errors.New("kubernetes token review: api server url must use https (plain http is only allowed for a loopback origin)")
	}
	if options.ReviewerToken == nil {
		return nil, errors.New("kubernetes token review: reviewer token source is required")
	}
	if strings.TrimSpace(options.Audience) == "" {
		return nil, errors.New("kubernetes token review: audience is required")
	}
	if strings.TrimSpace(options.TrustDomain) == "" {
		return nil, errors.New("kubernetes token review: trust domain is required")
	}
	client := options.HTTPClient
	if client == nil {
		// The default client refuses redirects -- both the reviewer token
		// (Authorization header) and the subject token (request body)
		// must never follow a retargeted host. A caller-supplied
		// HTTPClient is trusted as-is (never mutated here) rather than
		// second-guessed.
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("kubernetes token review: unexpected redirect refused")
		}}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &KubernetesTokenReviewValidator{
		http: client, apiServerURL: strings.TrimRight(options.APIServerURL, "/"), reviewerToken: options.ReviewerToken,
		audience: options.Audience, trustDomain: options.TrustDomain, now: now,
	}, nil
}

func (v *KubernetesTokenReviewValidator) Validate(ctx context.Context, subjectToken string) (SubjectIdentity, error) {
	if strings.TrimSpace(subjectToken) == "" {
		return SubjectIdentity{}, ErrSubjectTokenInvalid
	}
	reviewerToken, err := v.reviewerToken()
	if err != nil || strings.TrimSpace(reviewerToken) == "" {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: load reviewer token: %w", err)
	}
	body, err := json.Marshal(tokenReviewRequest{
		APIVersion: "authentication.k8s.io/v1", Kind: "TokenReview",
		Spec: tokenReviewRequestSpec{Token: subjectToken, Audiences: []string{v.audience}},
	})
	if err != nil {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.apiServerURL+tokenReviewPath, bytes.NewReader(body))
	if err != nil {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+reviewerToken)
	response, err := v.http.Do(request)
	if err != nil {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: request failed: %w", err)
	}
	defer response.Body.Close()
	rawBody, err := io.ReadAll(io.LimitReader(response.Body, maxTokenReviewResponseBytes+1))
	if err != nil {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: read response: %w", err)
	}
	if len(rawBody) > maxTokenReviewResponseBytes {
		return SubjectIdentity{}, errors.New("kubernetes token review: response too large")
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: unexpected status %d", response.StatusCode)
	}
	var review tokenReviewResponse
	if err := json.Unmarshal(rawBody, &review); err != nil {
		return SubjectIdentity{}, fmt.Errorf("kubernetes token review: decode response: %w", err)
	}
	if !review.Status.Authenticated {
		return SubjectIdentity{}, ErrSubjectTokenInvalid
	}
	if !slices.Contains(review.Status.Audiences, v.audience) {
		return SubjectIdentity{}, ErrSubjectTokenInvalid
	}
	namespace, name, ok := parseServiceAccountUsername(review.Status.User.Username)
	if !ok || strings.TrimSpace(review.Status.User.UID) == "" {
		return SubjectIdentity{}, ErrSubjectTokenInvalid
	}
	// TokenReview's own response carries no expiry field; the token's exp
	// claim is read directly here WITHOUT re-verifying the signature --
	// TokenReview above is the sole authentication authority. This read
	// only recovers an auxiliary timestamp (for capping the issued
	// access token's TTL, see AccessTokenIssuer), never an identity or
	// authorization claim.
	expiresAt, err := unverifiedJWTExpiry(subjectToken)
	if err != nil {
		return SubjectIdentity{}, fmt.Errorf("%w: %v", ErrSubjectTokenInvalid, err)
	}
	if !expiresAt.After(v.now().UTC()) {
		return SubjectIdentity{}, ErrSubjectTokenInvalid
	}
	return SubjectIdentity{
		TrustDomain: v.trustDomain, Namespace: namespace, ServiceAccountName: name,
		ServiceAccountUID: review.Status.User.UID, ExpiresAt: expiresAt,
	}, nil
}

// parseServiceAccountUsername parses the k8s TokenReview
// "system:serviceaccount:<namespace>:<name>" username shape.
func parseServiceAccountUsername(username string) (namespace, name string, ok bool) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

// unverifiedJWTExpiry reads a JWT's exp claim without verifying its
// signature. Callers must only use this after the token has already been
// authenticated by an independent authority (Kubernetes TokenReview) --
// see Validate's own doc comment.
func unverifiedJWTExpiry(token string) (time.Time, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return time.Time{}, errors.New("malformed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("decode JWT claims: %w", err)
	}
	seconds, err := strconv.ParseInt(claims.Exp.String(), 10, 64)
	if err != nil {
		return time.Time{}, errors.New("JWT exp claim is not a valid timestamp")
	}
	return time.Unix(seconds, 0).UTC(), nil
}
