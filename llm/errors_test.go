package llm

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyFollowsThePythonOrder(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantClass ErrorClass
		retryable bool
	}{
		{"quota before auth", &statusError{statusCode: 429, body: `{"error":{"code":"insufficient_quota"}}`}, ClassAuth, false},
		{"model not found", &statusError{statusCode: 404, body: `{"error":{"code":"model_not_found"}}`}, ClassModelNotFound, false},
		{"401", &statusError{statusCode: 401, body: `{}`}, ClassAuth, false},
		{"429", &statusError{statusCode: 429, body: `{}`}, ClassRateLimit, true},
		{"context length", &statusError{statusCode: 400, body: `{"error":{"code":"context_length_exceeded"}}`}, ClassContextLength, false},
		{"400", &statusError{statusCode: 400, body: `{}`}, ClassInvalidRequest, false},
		{"422", &statusError{statusCode: 422, body: `{}`}, ClassInvalidRequest, false},
		{"500", &statusError{statusCode: 500, body: `{}`}, ClassServer, true},
		{"529 overloaded", &statusError{statusCode: 529, body: `{"type":"overloaded_error"}`}, ClassServer, true},
		{"no response", &transportError{cause: errors.New("dial tcp: lookup api.example: no such host")}, ClassTransport, true},
		{"403 is auth (past errors.py, which left it generic)", &statusError{statusCode: 403, body: `{}`}, ClassAuth, false},
		{"402 is auth", &statusError{statusCode: 402, body: `{}`}, ClassAuth, false},
		{"404 is model not found", &statusError{statusCode: 404, body: `{}`}, ClassModelNotFound, false},
		{"408 is a timeout", &statusError{statusCode: 408, body: `{}`}, ClassTimeout, true},
		{"413 is an invalid request", &statusError{statusCode: 413, body: `{}`}, ClassInvalidRequest, false},
		{"413 stays invalid whatever its body says", &statusError{statusCode: 413, body: `request too big: too many tokens`}, ClassInvalidRequest, false},
		{"404 stays model not found whatever its body says", &statusError{statusCode: 404, body: `invalid request path`}, ClassModelNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err, KindOpenAI, "m")
			if got.Class != tc.wantClass || got.Retryable() != tc.retryable {
				t.Errorf("class = %q retryable = %v, want %q %v", got.Class, got.Retryable(), tc.wantClass, tc.retryable)
			}
			if got.Provider != KindOpenAI || got.Model != "m" {
				t.Errorf("provider/model = %q/%q", got.Provider, got.Model)
			}
		})
	}
}

func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	for raw, want := range map[string]time.Duration{"2": 2 * time.Second, "86400": 60 * time.Second, "-5": 0, "soon": 0} {
		got := classify(&statusError{statusCode: 429, header: http.Header{"Retry-After": {raw}}}, KindOpenAI, "m")
		if got.RetryAfter != want {
			t.Errorf("Retry-After %q = %v, want %v", raw, got.RetryAfter, want)
		}
	}
}

func TestProviderBodyNeverReachesTheErrorText(t *testing.T) {
	body := `{"error":{"message":"bad key sk-live-abcdefghijklmnopqrstuvwx, Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123"}}`
	got := classify(&statusError{statusCode: 418, body: body}, KindOpenAI, "m")
	var statusErr *statusError
	if !errors.As(got, &statusErr) {
		t.Fatal("the cause is not reachable")
	}
	for _, text := range []string{got.Error(), statusErr.body} {
		if strings.Contains(text, "abcdefghijklmnopqrstuvwx") {
			t.Errorf("a credential survived: %q", text)
		}
	}
}

func TestDelayUsesRetryAfterOnlyForARateLimit(t *testing.T) {
	if got := delayFor(&Error{Class: ClassRateLimit, RetryAfter: 3 * time.Second}, 0); got != 3*time.Second {
		t.Errorf("rate limit delay = %v", got)
	}
	if got := delayFor(&Error{Class: ClassServer, RetryAfter: 3 * time.Second}, 1); got != time.Second {
		t.Errorf("server delay = %v, want the backoff 1s", got)
	}
}
