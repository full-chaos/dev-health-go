// Package workloadtoken is the client half of the dev-health workload-token
// exchange (RFC 8693, POST /internal/v1/token/exchange on auth-service): a
// workload presents its own subject token, receives a short-lived
// workload-token.v1 access token for one audience, caches it and exchanges
// again before it expires.
//
// Why it is shaped this way:
//
//   - One credential class per deployment. A Config holds exactly one
//     Subject and no static or fallback credential, so a failed exchange can
//     never be papered over by sending some other secret.
//   - Fail closed. A token is sent only inside its usable window; after it, a
//     failed exchange is ErrNoUsableToken and the RoundTripper sends nothing,
//     neither an old token nor a request without one.
//   - Early re-exchange. Tokens live at most 300 s; the client exchanges again
//     at a fraction of the lifetime, so an exchange outage shorter than the
//     rest of the window costs nothing, and stops using a token SafetyMargin
//     before its end, which covers the verifier's clock skew allowance.
//   - The window is measured on this process's own clock from the moment the
//     request was sent, using only expires_in: a difference between this
//     host's wall clock and the issuer's never moves it.
//   - An explicit transport. Every request carries a credential, so the
//     caller hands in the transport and this package refuses
//     http.DefaultTransport and any *http.Transport with a Proxy: the process
//     environment's proxy never sees a subject or an access token.
//
// The package carries no audience or scope vocabulary and no principal type;
// the exchange decides what a subject may have.
package workloadtoken

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Wire values of the exchange.
const (
	ExchangePath           = "/internal/v1/token/exchange"
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	TokenTypeJWT           = "urn:ietf:params:oauth:token-type:jwt"
	// MaxLifetime is the workload-token.v1 lifetime ceiling; a longer
	// expires_in is not a workload token answer.
	MaxLifetime = 300 * time.Second
)

// Defaults and bounds of a Config.
const (
	DefaultTimeout         = 5 * time.Second
	MaxTimeout             = 30 * time.Second
	minTimeout             = 10 * time.Millisecond
	DefaultRefreshFraction = 0.5
	DefaultSafetyMargin    = 30 * time.Second
	MaxSafetyMargin        = 120 * time.Second
	DefaultBackoffFloor    = time.Second
	DefaultBackoffCeiling  = 30 * time.Second
	maxBackoff             = 5 * time.Minute

	maxSubjectBytes     = 4 << 10
	maxAnswerBytes      = 16 << 10
	maxAccessTokenBytes = 8 << 10
	maxAudienceBytes    = 64
	maxScopeActions     = 64
	maxActionBytes      = 128
)

type subjectKind int

const (
	subjectNone subjectKind = iota
	subjectKubernetes
)

// Subject is the one credential a deployment presents to the exchange.
type Subject struct {
	kind subjectKind
	path string
}

// KubernetesTokenFile is a projected ServiceAccount token at path, minted for
// the exchange's review audience. The file is read again for every exchange
// (the kubelet rotates it); the projected volume's symlink is followed.
func KubernetesTokenFile(path string) Subject {
	return Subject{kind: subjectKubernetes, path: path}
}

// Config configures a Client. Zero durations take the defaults.
type Config struct {
	// ExchangeURL is the base URL of auth-service (scheme, host and port);
	// the client appends ExchangePath. https, or http with AllowPlainHTTP.
	ExchangeURL string
	// AllowPlainHTTP admits an http ExchangeURL (a cluster-internal service).
	AllowPlainHTTP bool
	// Subject is the deployment's one credential. Required.
	Subject Subject
	// Transport carries the exchange requests. Required: never
	// http.DefaultTransport, never an *http.Transport with a Proxy.
	Transport http.RoundTripper
	// Timeout bounds one exchange. Default 5 s, at most 30 s.
	Timeout time.Duration
	// RefreshFraction of the lifetime after which the client exchanges
	// again. Default 0.5; must be in (0, 1).
	RefreshFraction float64
	// SafetyMargin before the end of the lifetime from which a token is no
	// longer sent. Default 30 s, at most 120 s.
	SafetyMargin time.Duration
	// BackoffFloor and BackoffCeiling bound the wait after a failed exchange
	// (doubling from the floor). Defaults 1 s and 30 s.
	BackoffFloor   time.Duration
	BackoffCeiling time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger receives the one startup line naming the exchange scheme and
	// host (never a token or a subject). Default slog.Default().
	Logger *slog.Logger
}

// CheckTransport is the explicit-transport rule this package applies to
// every transport it is given: not nil, not http.DefaultTransport, and not
// an *http.Transport with a Proxy (which would send credentials through the
// proxy the process environment names).
func CheckTransport(rt http.RoundTripper) error {
	if rt == nil {
		return errors.New("workloadtoken: a transport is required")
	}
	if rt == http.DefaultTransport {
		return errors.New("workloadtoken: http.DefaultTransport is refused: it uses the environment proxy")
	}
	if t, ok := rt.(*http.Transport); ok && t.Proxy != nil {
		return errors.New("workloadtoken: a transport with a Proxy is refused")
	}
	return nil
}

func endpointFor(raw string, allowPlainHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return nil, errors.New("workloadtoken: the exchange URL must be an absolute URL with a host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPlainHTTP {
			return nil, errors.New("workloadtoken: an http exchange URL needs AllowPlainHTTP")
		}
	default:
		return nil, errors.New("workloadtoken: the exchange URL scheme must be https or http")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("workloadtoken: the exchange URL must hold only a scheme, a host and a port")
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: ExchangePath}, nil
}
