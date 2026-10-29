package workloadtoken

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Client exchanges the deployment's subject for workload tokens and caches
// one token per (audience, scope). It is safe for concurrent use.
type Client struct {
	endpoint        string
	issuer          string
	allowPlainHTTP  bool
	subject         Subject
	http            *http.Client
	timeout         time.Duration
	refreshFraction float64
	margin          time.Duration
	backoffFloor    time.Duration
	backoffCeiling  time.Duration
	now             func() time.Time
	logger          *slog.Logger

	mu    sync.Mutex
	slots map[string]*slot
}

// entry is one cached token and the two instants of its window, both on the
// clock reading taken before the request was sent.
type entry struct {
	token       string
	audience    string
	sent        time.Time
	lifetime    time.Duration
	notBefore   time.Time
	refreshAt   time.Time
	usableUntil time.Time
}

type slot struct {
	entry    *entry
	flight   chan struct{}
	retryAt  time.Time
	failures int
	lastErr  error
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.Subject.kind == subjectNone || cfg.Subject.path == "" {
		return nil, errors.New("workloadtoken: a subject is required")
	}
	if err := CheckTransport(cfg.Transport); err != nil {
		return nil, err
	}
	if !issuerShape.MatchString(cfg.Issuer) {
		return nil, errors.New("workloadtoken: the issuer must be the https token issuer URL")
	}
	endpoint, err := endpointFor(cfg.ExchangeURL, cfg.AllowPlainHTTP)
	if err != nil {
		return nil, err
	}
	c := &Client{
		endpoint: endpoint.String(), issuer: cfg.Issuer, allowPlainHTTP: cfg.AllowPlainHTTP, subject: cfg.Subject, timeout: cfg.Timeout, refreshFraction: cfg.RefreshFraction,
		margin: cfg.SafetyMargin, backoffFloor: cfg.BackoffFloor, backoffCeiling: cfg.BackoffCeiling, now: cfg.Now,
		slots: map[string]*slot{},
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.timeout < minTimeout || c.timeout > MaxTimeout {
		return nil, errors.New("workloadtoken: the timeout is out of bounds")
	}
	if c.refreshFraction == 0 {
		c.refreshFraction = DefaultRefreshFraction
	}
	if !(c.refreshFraction > 0 && c.refreshFraction < 1) {
		return nil, errors.New("workloadtoken: the refresh fraction must be in (0, 1)")
	}
	if c.margin == 0 {
		c.margin = DefaultSafetyMargin
	}
	if c.margin < 0 || c.margin > MaxSafetyMargin {
		return nil, errors.New("workloadtoken: the safety margin is out of bounds")
	}
	if c.backoffFloor == 0 {
		c.backoffFloor = DefaultBackoffFloor
	}
	if c.backoffCeiling == 0 {
		c.backoffCeiling = DefaultBackoffCeiling
	}
	if c.backoffFloor < 0 || c.backoffCeiling < c.backoffFloor || c.backoffCeiling > maxBackoff {
		return nil, errors.New("workloadtoken: the backoff bounds are out of bounds")
	}
	if c.now == nil {
		c.now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c.logger = logger
	logger.Info("workload token exchange endpoint", "scheme", endpoint.Scheme, "host", endpoint.Host)
	c.http = &http.Client{
		Transport: cfg.Transport,
		Timeout:   c.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirect
		},
	}
	return c, nil
}

var errRedirect = errors.New("workloadtoken: redirect refused")

// errNotConfigured is a nil Client or one not made by New.
var errNotConfigured = errors.New("workloadtoken: the client was not made by New")

func (c *Client) ready() bool { return c != nil && c.http != nil && c.slots != nil }

// Token returns a workload token for audience and scope (an empty scope asks
// for every action the exchange grants). It returns a cached token before its
// refresh point; from there on it exchanges again, once for all concurrent
// callers, and keeps returning the cached token while it is still inside its
// usable window. With no usable token it returns an error that wraps
// ErrNoUsableToken: the caller must not send its request.
func (c *Client) Token(ctx context.Context, audience string, scope ...string) (string, error) {
	if !c.ready() {
		return "", noUsable(errNotConfigured)
	}
	if ctx == nil {
		return "", noUsable(errors.New("workloadtoken: a context is required"))
	}
	key, actions, err := requestKey(audience, scope)
	if err != nil {
		return "", err
	}
	for {
		c.mu.Lock()
		s := c.slots[key]
		if s == nil {
			s = &slot{}
			c.slots[key] = s
		}
		now := c.now()
		if s.entry != nil && c.checkToken(s.entry, now) != nil {
			s.entry = nil
		}
		if s.entry != nil && now.Before(s.entry.refreshAt) {
			token := s.entry.token
			c.mu.Unlock()
			return token, nil
		}
		if s.flight != nil {
			if s.entry != nil {
				token := s.entry.token
				c.mu.Unlock()
				return token, nil
			}
			flight := s.flight
			c.mu.Unlock()
			select {
			case <-flight:
				continue
			case <-ctx.Done():
				return "", noUsable(ctx.Err())
			}
		}
		if now.Before(s.retryAt) {
			if s.entry != nil {
				token := s.entry.token
				c.mu.Unlock()
				return token, nil
			}
			lastErr := s.lastErr
			c.mu.Unlock()
			return "", noUsable(lastErr)
		}
		flight := make(chan struct{})
		s.flight = flight
		c.mu.Unlock()

		// The exchange is shared by every waiter: one caller's cancellation
		// must not fail it for the others (the timeout still bounds it).
		fresh, exchangeErr := c.exchange(context.WithoutCancel(ctx), audience, actions)

		c.mu.Lock()
		s.flight = nil
		close(flight)
		if exchangeErr == nil {
			now := c.now()
			if exchangeErr = c.checkToken(fresh, now); errors.Is(exchangeErr, errNotYetValid) {
				c.logger.Warn("workload token not yet valid: the issuer clock is ahead of this host",
					"audience", audience, "ahead_seconds", int64((fresh.notBefore.Sub(now)+time.Second-1)/time.Second))
			}
		}
		if exchangeErr == nil {
			s.entry, s.failures, s.retryAt, s.lastErr = fresh, 0, time.Time{}, nil
			c.mu.Unlock()
			return fresh.token, nil
		}
		s.failures++
		s.lastErr = exchangeErr
		s.retryAt = c.now().Add(c.backoff(s.failures))
		if s.entry != nil && c.checkToken(s.entry, c.now()) == nil {
			token := s.entry.token
			c.mu.Unlock()
			return token, nil
		}
		s.entry = nil
		c.mu.Unlock()
		return "", noUsable(exchangeErr)
	}
}

// Invalidate drops the cached token for audience and scope (for a caller
// whose resource server refused the token); the next Token call exchanges.
func (c *Client) Invalidate(audience string, scope ...string) {
	if !c.ready() {
		return
	}
	key, _, err := requestKey(audience, scope)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.slots[key]; s != nil {
		s.entry, s.retryAt, s.failures, s.lastErr = nil, time.Time{}, 0, nil
	}
}

func (c *Client) backoff(failures int) time.Duration {
	wait := c.backoffFloor
	for i := 1; i < failures && wait < c.backoffCeiling; i++ {
		wait *= 2
	}
	return min(wait, c.backoffCeiling)
}

func noUsable(cause error) error {
	if cause == nil {
		return ErrNoUsableToken
	}
	return fmt.Errorf("%w: %w", ErrNoUsableToken, cause)
}

// requestKey validates audience and scope and returns the cache key and the
// sorted, de-duplicated actions sent.
func requestKey(audience string, scope []string) (string, []string, error) {
	if audience == "" || len(audience) > maxAudienceBytes || !printable(audience) {
		return "", nil, errors.New("workloadtoken: the audience must be 1 to 64 printable characters without spaces")
	}
	if len(scope) > maxScopeActions {
		return "", nil, errors.New("workloadtoken: too many scope actions")
	}
	actions := slices.Clone(scope)
	for _, action := range actions {
		if action == "" || len(action) > maxActionBytes || !printable(action) {
			return "", nil, errors.New("workloadtoken: a scope action must be 1 to 128 printable characters without spaces")
		}
	}
	slices.Sort(actions)
	actions = slices.Compact(actions)
	return audience + " " + strings.Join(actions, " "), actions, nil
}

func printable(value string) bool {
	for _, r := range value {
		if r <= ' ' || r >= 0x7f {
			return false
		}
	}
	return true
}
