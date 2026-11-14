// Package pep is the Go policy enforcement point client for the auth service's
// policy decision point (POST /v1/authorize and /v1/authorize:batch).
//
// WHY EVERY FAILURE IS A DENY. A PEP that cannot get a valid decision must not
// let the action through (TRD section 8: network, SDK, malformed-policy or PDP
// errors never return allow). So Decide and DecideBatch return a Decision, not
// a (Decision, error): there is no error a caller can drop on the floor and
// then read a zero value as an allow. The zero Decision denies, a Decision has
// no exported field, and the only allowed Decision is one built from a decision
// document that passed every check here. A deny the client makes itself has a
// closed local reason starting "pdp_" (LocalReasons) and a typed Error from
// Decision.Err; the transport error never reaches the caller.
//
// WHAT IS CHECKED, from the authorization_decision.v1 and
// authorization_batch_response.v1 contracts and their reject_by_client goldens:
// the response is a strict JSON document (no repeated member at any depth, no
// data after it, no member in another case, none the contract lacks); allowed
// is a JSON boolean and agrees with a reason_code from the closed set; the
// policy revision and the batch index are integer tokens (42.0 is refused);
// expires_at is present and a valid timestamp in the contract's range, for an
// allow and a deny alike; it bounds reuse only, so a decision whose expires_at
// is not after now (past, or equal to it) is accepted for its own request and
// is not cacheable (Decision.Cacheable is false, a deny stays a PDP deny);
// obligations are closed (an obligation or redaction class or assurance
// level this client does not know denies the whole decision); the decision's
// organization is the request's. The response carries no request id (the
// contract never echoes it), so correlation is the organization for a single
// call and the count, position and explicit index for a batch, where one bad
// binding or one bad item denies every item.
package pep

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// PathAuthorize and PathAuthorizeBatch are the PDP routes.
	PathAuthorize      = "/v1/authorize"
	PathAuthorizeBatch = "/v1/authorize:batch"

	defaultTimeout = 2 * time.Second
	maxSingleBody  = 256 << 10
	maxBatchBody   = 4 << 20
)

// Config configures a Client.
type Config struct {
	// BaseURL is the auth service origin: a scheme, a host and a port only.
	BaseURL string
	// AllowPlainHTTP permits an http BaseURL. For tests and loopback only.
	AllowPlainHTTP bool
	// HTTPClient sends the calls. It must carry the caller's workload token
	// for the auth-service audience (for example workloadtoken.Client.HTTPClient).
	// New copies it and refuses redirects on the copy; the caller's value is
	// not changed.
	HTTPClient *http.Client
	// ClientService names the calling service, from the contract's closed set.
	// It labels metrics and audit rows and confers nothing.
	ClientService string
	// Timeout bounds one call, on top of the caller's context. Default 2s.
	Timeout time.Duration
	// Now is the clock that decides whether a decision is Cacheable. Default time.Now.
	Now func() time.Time
}

// Client asks the PDP. The zero value and a nil *Client deny.
type Client struct {
	single  string
	batch   string
	http    *http.Client
	service string
	timeout time.Duration
	now     func() time.Time
}

var _ Decider = (*Client)(nil)

// New checks the configuration. A bad one is a startup error, not a runtime
// deny, so a misconfigured PEP fails before it serves.
func New(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("pep: an HTTP client is required")
	}
	if !contains(clientServices, cfg.ClientService) {
		return nil, errors.New("pep: ClientService is not in the contract's service set")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("pep: Timeout must not be negative")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.Opaque != "" {
		return nil, errors.New("pep: BaseURL must be an absolute URL with a host")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && cfg.AllowPlainHTTP:
	default:
		return nil, errors.New("pep: BaseURL must be https (http needs AllowPlainHTTP)")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("pep: BaseURL must hold only a scheme, a host and a port")
	}
	hc := *cfg.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{single: u.Scheme + "://" + u.Host + PathAuthorize, batch: u.Scheme + "://" + u.Host + PathAuthorizeBatch, http: &hc, service: cfg.ClientService, timeout: cfg.Timeout, now: cfg.Now}
	if c.timeout == 0 {
		c.timeout = defaultTimeout
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

func (c *Client) ready() bool { return c != nil && c.http != nil && c.now != nil && c.single != "" }

// Decide asks the PDP one question. Every failure is a local deny.
func (c *Client) Decide(ctx context.Context, req Request) Decision {
	if !c.ready() {
		return deny(fail(ReasonNotConfigured))
	}
	if ctx == nil || !req.valid() {
		return deny(fail(ReasonInvalidRequest))
	}
	body, err := c.singleBody(req)
	if err != nil {
		return deny(fail(ReasonInvalidRequest))
	}
	raw, e := c.post(ctx, c.single, body, maxSingleBody)
	if e != nil {
		return deny(e)
	}
	return parseDecision(raw, req, c.now())
}

// DecideBatch asks the PDP up to MaxBatch independent questions and returns one
// Decision per request, in order. An empty batch returns an empty slice and
// makes no call. A batch over MaxBatch, or with one invalid request, is denied
// whole and never sent. Any failure, and any bad binding or bad item in the
// response, denies every item.
func (c *Client) DecideBatch(ctx context.Context, reqs []Request) []Decision {
	if len(reqs) == 0 {
		return []Decision{}
	}
	if !c.ready() {
		return denyAll(fail(ReasonNotConfigured), len(reqs))
	}
	if ctx == nil || len(reqs) > MaxBatch {
		return denyAll(fail(ReasonInvalidRequest), len(reqs))
	}
	for _, r := range reqs {
		if !r.valid() {
			return denyAll(fail(ReasonInvalidRequest), len(reqs))
		}
	}
	body, err := c.batchBody(reqs)
	if err != nil {
		return denyAll(fail(ReasonInvalidRequest), len(reqs))
	}
	raw, e := c.post(ctx, c.batch, body, maxBatchBody)
	if e != nil {
		return denyAll(e, len(reqs))
	}
	return parseBatch(raw, reqs, c.now())
}

func (c *Client) post(ctx context.Context, target string, body []byte, limit int64) ([]byte, *Error) {
	if err := ctx.Err(); err != nil {
		return nil, classify(ctx, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fail(ReasonInvalidRequest)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, failStatus(ReasonRateLimited, resp.StatusCode)
	case resp.StatusCode >= 500:
		return nil, failStatus(ReasonUnavailable, resp.StatusCode)
	default:
		return nil, failStatus(ReasonRefused, resp.StatusCode)
	}
	if media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return nil, fail(ReasonMalformed)
	}
	if resp.ContentLength > limit {
		return nil, fail(ReasonTooLarge)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, classify(ctx, err)
	}
	if int64(len(raw)) > limit {
		return nil, fail(ReasonTooLarge)
	}
	return raw, nil
}

// classify maps a transport failure to a local reason, dropping the error
// itself: a transport error can hold the URL and the host.
func classify(ctx context.Context, err error) *Error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return fail(ReasonCanceled)
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return fail(ReasonTimeout)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fail(ReasonTimeout)
	}
	return fail(ReasonUnreachable)
}
