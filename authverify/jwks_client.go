package authverify

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrJWKSUnavailable is wrapped by every failed discovery or JWKS refresh. Its
// text never carries a URL, a response body or key bytes.
var ErrJWKSUnavailable = errors.New("jwks refresh failed")

// ErrRefreshTooSoon is returned by Refresh inside the refresh floor.
var ErrRefreshTooSoon = errors.New("jwks refresh inside the floor interval")

const (
	discoveryPath = "/.well-known/openid-configuration"
	jwksPath      = "/.well-known/jwks.json"
)

var discoveryMembers = []string{"issuer", "jwks_uri", "id_token_signing_alg_values_supported"}

// JWKSClientConfig configures a JWKSClient. Zero values take the defaults.
type JWKSClientConfig struct {
	// Issuer is the pinned issuer URL: the only issuer whose discovery
	// document and JWKS this client accepts. Required, https.
	Issuer string
	// HTTPClient carries the requests; redirects are always refused and the
	// timeout is set from Timeout. Default: a fresh http.Client.
	HTTPClient *http.Client
	// Timeout bounds one refresh (discovery plus JWKS). Default 5s, max 30s.
	Timeout time.Duration
	// MaxBodyBytes caps each response body. Default 64 KiB.
	MaxBodyBytes int64
	// MaxKeys caps the keys one JWKS may hold. Default 32.
	MaxKeys int
	// CacheTTL is how long keys are served without a refresh. Default 5m,
	// max 1h.
	CacheTTL time.Duration
	// RefreshFloor is the minimum time between two fetch attempts, success or
	// failure. Default 30s, at most CacheTTL.
	RefreshFloor time.Duration
	// OutageGrace is how long past CacheTTL the last good keys stay usable
	// when refreshes fail (the rotation overlap window). Default 10m, max 1h.
	OutageGrace time.Duration
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// JWKSClient resolves access-token keys from one pinned issuer over HTTPS. It
// is a KeySource. Fail closed: an unknown kid causes at most one shared
// refresh per floor interval, and a kid still unknown afterwards is refused.
type JWKSClient struct {
	cfg  JWKSClientConfig
	http *http.Client

	mu          sync.Mutex
	keys        map[string]ed25519.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	attempted   bool
	inflight    *refreshFlight
}

type refreshFlight struct {
	done chan struct{}
	err  error
}

// NewJWKSClient validates cfg and returns a client. It performs no I/O.
func NewJWKSClient(cfg JWKSClientConfig) (*JWKSClient, error) {
	if !issuerPattern.MatchString(cfg.Issuer) {
		return nil, errors.New("authverify: jwks client needs an https issuer")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 64 << 10
	}
	if cfg.MaxKeys == 0 {
		cfg.MaxKeys = 32
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.RefreshFloor == 0 {
		cfg.RefreshFloor = 30 * time.Second
	}
	if cfg.OutageGrace == 0 {
		cfg.OutageGrace = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Timeout < 0 || cfg.Timeout > 30*time.Second || cfg.MaxBodyBytes < 0 || cfg.MaxKeys < 0 ||
		cfg.CacheTTL < 0 || cfg.CacheTTL > time.Hour || cfg.RefreshFloor < 0 || cfg.RefreshFloor > cfg.CacheTTL ||
		cfg.OutageGrace < 0 || cfg.OutageGrace > time.Hour {
		return nil, errors.New("authverify: jwks client limits are out of bounds")
	}
	var client http.Client
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	client.Timeout = cfg.Timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	return &JWKSClient{cfg: cfg, http: &client}, nil
}

// PublicKey implements KeySource.
func (c *JWKSClient) PublicKey(kid string) (ed25519.PublicKey, bool) {
	if key, ok := c.cached(kid, c.cfg.Now(), false); ok {
		return key, true
	}
	// Unknown kid, or the cache is past its TTL: one shared refresh, then
	// decide on what the cache holds.
	_ = c.refresh()
	return c.cached(kid, c.cfg.Now(), true)
}

// Refresh fetches discovery and the JWKS now, unless a fetch is in flight
// (it then waits for that one) or the floor has not elapsed.
func (c *JWKSClient) Refresh(_ context.Context) error { return c.refresh() }

// cached returns the key for kid. Inside CacheTTL it serves the cache; past
// it, only after a refresh was tried (afterRefresh) and only inside the
// outage grace.
func (c *JWKSClient) cached(kid string, now time.Time, afterRefresh bool) (ed25519.PublicKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys == nil {
		return nil, false
	}
	age := now.Sub(c.fetchedAt)
	if age >= c.cfg.CacheTTL && (!afterRefresh || age >= c.cfg.CacheTTL+c.cfg.OutageGrace) {
		return nil, false
	}
	key, ok := c.keys[kid]
	return key, ok && len(key) == ed25519.PublicKeySize
}

func (c *JWKSClient) refresh() error {
	c.mu.Lock()
	if flight := c.inflight; flight != nil {
		c.mu.Unlock()
		<-flight.done
		return flight.err
	}
	now := c.cfg.Now()
	if c.attempted && now.Sub(c.lastAttempt) < c.cfg.RefreshFloor {
		c.mu.Unlock()
		return ErrRefreshTooSoon
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.inflight = flight
	c.lastAttempt = now
	c.attempted = true
	c.mu.Unlock()

	keys, err := c.fetch()

	c.mu.Lock()
	if err == nil {
		c.keys = keys
		c.fetchedAt = c.cfg.Now()
	}
	c.inflight = nil
	flight.err = err
	c.mu.Unlock()
	close(flight.done)
	return err
}

func (c *JWKSClient) fetch() (map[string]ed25519.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	discovery, err := c.get(ctx, c.cfg.Issuer+discoveryPath)
	if err != nil {
		return nil, err
	}
	if !c.discoveryValid(discovery) {
		return nil, ErrJWKSUnavailable
	}
	body, err := c.get(ctx, c.cfg.Issuer+jwksPath)
	if err != nil {
		return nil, err
	}
	keys, err := parseJWKS(body)
	if err != nil || len(keys) > c.cfg.MaxKeys {
		return nil, ErrJWKSUnavailable
	}
	return keys, nil
}

func (c *JWKSClient) get(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrJWKSUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.cfg.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > c.cfg.MaxBodyBytes {
		return nil, ErrJWKSUnavailable
	}
	return body, nil
}

// discoveryValid applies the discovery.v1 contract: the closed member set (the
// schema sets additionalProperties false), the issuer equal to the pinned one,
// jwks_uri equal to the issuer plus the fixed path, and exactly EdDSA advertised.
func (c *JWKSClient) discoveryValid(raw []byte) bool {
	object, ok := decodeObject(raw)
	if !ok || !closedSet(object, discoveryMembers, discoveryMembers) {
		return false
	}
	if issuer, ok := stringMember(object, "issuer", nil); !ok || issuer != c.cfg.Issuer {
		return false
	}
	if uri, ok := stringMember(object, "jwks_uri", nil); !ok || uri != c.cfg.Issuer+jwksPath {
		return false
	}
	algs, ok := object["id_token_signing_alg_values_supported"].([]any)
	if !ok || len(algs) != 1 {
		return false
	}
	alg, ok := algs[0].(string)
	return ok && alg == algEdDSA
}
