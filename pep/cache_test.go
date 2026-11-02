package pep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: testNow} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// verdict is what the scripted PDP answers for one question. A zero expires is
// now + 10 s on the test clock.
type verdict struct {
	allow       bool
	reason      string
	rev         uint64
	expires     time.Time
	obligations string
}

// cachePDP is a scripted PDP: answer picks the verdict per question; status,
// when not 200, fails every call; hold, when set, blocks a call until it is
// closed.
type cachePDP struct {
	t       *testing.T
	clock   *testClock
	srv     *httptest.Server
	mu      sync.Mutex
	answer  func(r wireRequest) verdict
	status  int
	singles atomic.Int32
	batches atomic.Int32
	sent    [][]string
	arrived chan string
}

func newCachePDP(t *testing.T, clock *testClock) *cachePDP {
	t.Helper()
	p := &cachePDP{t: t, clock: clock, status: http.StatusOK, arrived: make(chan string, 256)}
	p.answer = func(wireRequest) verdict { return verdict{allow: true, rev: 5} }
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *cachePDP) set(answer func(r wireRequest) verdict) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = answer
}

func (p *cachePDP) fail(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

func (p *cachePDP) note(key string) {
	select {
	case p.arrived <- key:
	default:
	}
}

func (p *cachePDP) calls() int { return int(p.singles.Load() + p.batches.Load()) }

func (p *cachePDP) doc(r wireRequest, v verdict) map[string]any {
	reason := v.reason
	if reason == "" {
		reason = "allowed_by_role"
		if !v.allow {
			reason = "role_lacks_action"
		}
	}
	expires := v.expires
	if expires.IsZero() {
		expires = p.clock.Now().Add(10 * time.Second)
	}
	var org any
	if r.Resource.OrganizationID != "" {
		org = r.Resource.OrganizationID
	}
	d := map[string]any{
		"schema_version": "authorization_decision.v1", "decision_id": "dec_" + strings.Repeat("A", 25) + strconv.Itoa(int(p.calls()%10)),
		"allowed": v.allow, "reason_code": reason, "principal_id": "prn_EXAMPLE", "organization_id": org,
		"policy_revision": json.Number(strconv.FormatUint(v.rev, 10)), "expires_at": expires.UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
	}
	if v.obligations != "" {
		d["obligations"] = json.RawMessage(v.obligations)
	}
	return d
}

func (p *cachePDP) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	answer, status := p.answer, p.status
	p.mu.Unlock()
	var out any
	if r.URL.Path == PathAuthorizeBatch {
		p.batches.Add(1)
		var b wireBatchRequest
		if err := json.Unmarshal(body, &b); err != nil {
			p.t.Errorf("batch body: %v", err)
		}
		var keys []string
		items := []map[string]any{}
		for i, q := range b.Requests {
			keys = append(keys, q.Resource.Key)
			items = append(items, map[string]any{"index": i, "decision": p.doc(q, answer(q))})
		}
		p.mu.Lock()
		p.sent = append(p.sent, keys)
		p.mu.Unlock()
		p.note("batch")
		out = map[string]any{"schema_version": "authorization_batch_response.v1", "decisions": items}
	} else {
		p.singles.Add(1)
		var q wireRequest
		if err := json.Unmarshal(body, &q); err != nil {
			p.t.Errorf("body: %v", err)
		}
		p.note(q.Resource.Key)
		out = p.doc(q, answer(q))
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	raw, err := json.Marshal(out)
	if err != nil {
		p.t.Errorf("marshal: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (p *cachePDP) cached(t *testing.T, cfg CacheConfig) *CachedClient {
	t.Helper()
	c, err := New(Config{BaseURL: p.srv.URL, AllowPlainHTTP: true, HTTPClient: p.srv.Client(), ClientService: "dev-health-web", Timeout: 5 * time.Second, Now: p.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Now == nil {
		cfg.Now = p.clock.Now
	}
	cc, err := NewCachedClient(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func requestKey(key string) Request {
	r := baseRequest()
	r.ResourceKey = key
	return r
}

// stored reports a live entry for r without touching its recency.
func stored(c *CachedClient, r Request) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[cacheKey(r)]
	return ok && el.Value.(*cacheEntry).live
}

func wantCalls(t *testing.T, p *cachePDP, want int) {
	t.Helper()
	if got := p.calls(); got != want {
		t.Fatalf("PDP calls %d, want %d", got, want)
	}
}

func wantAllow(t *testing.T, d Decision) {
	t.Helper()
	if !d.Allowed() || d.Local() {
		t.Fatalf("decision %s (local %v), want an allow", d.Reason(), d.Local())
	}
}

func wantDeny(t *testing.T, d Decision, reason Reason) {
	t.Helper()
	if d.Allowed() || d.Reason() != reason {
		t.Fatalf("decision allowed=%v reason %s, want a deny %s", d.Allowed(), d.Reason(), reason)
	}
}

// heldCall starts Decide for r with the PDP blocked on its answer until the
// returned release is called; it waits until the call reached the PDP.
func heldCall(t *testing.T, p *cachePDP, c *CachedClient, r Request, v verdict) (release func() Decision) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	prev := p.answer
	p.set(func(q wireRequest) verdict {
		<-gate
		return v
	})
	done := make(chan Decision, 1)
	go func() { done <- c.Decide(context.Background(), r) }()
	for got := ""; got != r.ResourceKey; got = <-p.arrived {
	}
	p.set(prev)
	return func() Decision {
		open()
		return <-done
	}
}

func TestCacheAllowHitSkipsPDP(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	first := c.Decide(context.Background(), baseRequest())
	wantAllow(t, first)
	second := c.Decide(context.Background(), baseRequest())
	wantAllow(t, second)
	if second.DecisionID() != first.DecisionID() {
		t.Fatalf("hit %s, want the stored %s", second.DecisionID(), first.DecisionID())
	}
	wantCalls(t, p, 1)
	if c.Len() != 1 {
		t.Fatalf("Len %d, want 1", c.Len())
	}
}

func TestCacheDenyNeverStored(t *testing.T) {
	for _, reason := range []string{"role_lacks_action", "no_membership"} {
		t.Run(reason, func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			p.set(func(wireRequest) verdict {
				return verdict{reason: reason, rev: 5, expires: clock.Now().Add(20 * time.Second)}
			})
			c := p.cached(t, CacheConfig{})
			d := c.Decide(context.Background(), baseRequest())
			if !d.Cacheable() {
				t.Fatal("fixture: the deny should carry a future expiry")
			}
			wantDeny(t, d, Reason(reason))
			wantDeny(t, c.Decide(context.Background(), baseRequest()), Reason(reason))
			wantCalls(t, p, 2)
			if c.Len() != 0 {
				t.Fatalf("Len %d after denies", c.Len())
			}
		})
	}
}

func TestCacheLocalDenyNeverStored(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			p.fail(status)
			c := p.cached(t, CacheConfig{})
			if d := c.Decide(context.Background(), baseRequest()); d.Allowed() || !d.Local() {
				t.Fatalf("decision %s, want a local deny", d.Reason())
			}
			p.fail(http.StatusOK)
			wantAllow(t, c.Decide(context.Background(), baseRequest()))
			wantCalls(t, p, 2)
		})
	}
}

func TestCacheNotCacheableUsedOnce(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5, expires: clock.Now()} })
	c := p.cached(t, CacheConfig{})
	d := c.Decide(context.Background(), baseRequest())
	wantAllow(t, d)
	if d.Cacheable() {
		t.Fatal("an allow with expires_at = now is cacheable")
	}
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	wantCalls(t, p, 2)
	if c.Len() != 0 {
		t.Fatalf("Len %d, want 0", c.Len())
	}
}

func TestCacheExpiryBoundary(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5, expires: testNow.Add(5 * time.Second)} })
	c := p.cached(t, CacheConfig{})
	c.Decide(context.Background(), baseRequest())
	clock.Set(testNow.Add(5*time.Second - time.Nanosecond))
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	wantCalls(t, p, 1)
	clock.Set(testNow.Add(5 * time.Second))
	c.Decide(context.Background(), baseRequest())
	wantCalls(t, p, 2)
}

func TestCacheMaxTTLCapsExpiry(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		cap  time.Duration
	}{{"default", 0, 30 * time.Second}, {"one second", time.Second, time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5, expires: testNow.Add(10 * time.Minute)} })
			c := p.cached(t, CacheConfig{MaxTTL: tc.ttl})
			c.Decide(context.Background(), baseRequest())
			clock.Set(testNow.Add(tc.cap - time.Nanosecond))
			c.Decide(context.Background(), baseRequest())
			wantCalls(t, p, 1)
			clock.Set(testNow.Add(tc.cap))
			c.Decide(context.Background(), baseRequest())
			wantCalls(t, p, 2)
		})
	}
}

func TestCacheClockStepBackIsAMiss(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	c.Decide(context.Background(), baseRequest())
	clock.Set(testNow.Add(-time.Second))
	c.Decide(context.Background(), baseRequest())
	wantCalls(t, p, 2)
}

func TestCacheConfigRefused(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	good, err := New(Config{BaseURL: p.srv.URL, AllowPlainHTTP: true, HTTPClient: p.srv.Client(), ClientService: "dev-health-web"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		c   *Client
		cfg CacheConfig
	}{
		"negative entries":    {good, CacheConfig{MaxEntries: -1}},
		"negative ttl":        {good, CacheConfig{MaxTTL: -time.Nanosecond}},
		"ttl over 30s":        {good, CacheConfig{MaxTTL: DefaultCacheTTL + time.Nanosecond}},
		"nil client":          {nil, CacheConfig{}},
		"unconfigured client": {&Client{}, CacheConfig{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if c, err := NewCachedClient(tc.c, tc.cfg); err == nil || c != nil {
				t.Fatalf("NewCachedClient accepted %+v", tc.cfg)
			}
		})
	}
	if _, err := NewCachedClient(good, CacheConfig{MaxTTL: DefaultCacheTTL, MaxEntries: 1}); err != nil {
		t.Fatalf("MaxTTL of exactly 30s refused: %v", err)
	}
}

func TestCacheKeyFields(t *testing.T) {
	org := func(r Request) Request { r.ResourceType, r.ResourceKey = "organization", "acme"; return r }
	variants := map[string]func(Request) Request{
		"principal reference": func(r Request) Request { r.PrincipalReference = "prf_EXAMPLE0000000000000002"; return r },
		"action":              func(r Request) Request { r.Action = "acr.context.write"; return r },
		"resource key":        func(r Request) Request { r.ResourceKey = "full-chaos/other"; return r },
		"organization":        func(r Request) Request { r.OrganizationID = "org_EXAMPLE0000000000000002"; return r },
		"platform":            func(r Request) Request { r.OrganizationID = ""; return r },
	}
	for name, change := range variants {
		t.Run(name, func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			c := p.cached(t, CacheConfig{})
			wantAllow(t, c.Decide(context.Background(), baseRequest()))
			wantAllow(t, c.Decide(context.Background(), change(baseRequest())))
			wantCalls(t, p, 2)
		})
	}
	t.Run("resource type", func(t *testing.T) {
		clock := newClock()
		p := newCachePDP(t, clock)
		c := p.cached(t, CacheConfig{})
		c.Decide(context.Background(), org(baseRequest()))
		other := org(baseRequest())
		other.ResourceType = "membership"
		c.Decide(context.Background(), other)
		wantCalls(t, p, 2)
	})
	t.Run("request id is not in the key", func(t *testing.T) {
		clock := newClock()
		p := newCachePDP(t, clock)
		c := p.cached(t, CacheConfig{})
		c.Decide(context.Background(), baseRequest())
		r := baseRequest()
		r.RequestID = "req_EXAMPLE0000000000000000002"
		wantAllow(t, c.Decide(context.Background(), r))
		r.RequestID = ""
		wantAllow(t, c.Decide(context.Background(), r))
		wantCalls(t, p, 1)
	})
}

func TestCacheKeyEncodingUnambiguous(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	a := baseRequest()
	a.PrincipalReference, a.Action = "prf_EXAMPLE000000000000a", "cr.context.read"
	b := baseRequest()
	b.PrincipalReference, b.Action = "prf_EXAMPLE000000000000", "acr.context.read"
	c.Decide(context.Background(), a)
	c.Decide(context.Background(), b)
	wantCalls(t, p, 2)
}

func FuzzCacheKeyEncoding(f *testing.F) {
	f.Add("a", "b,c", "d", "e", "f", "a,b", "c", "d", "e", "f")
	f.Add(`"`, `","`, "", "", "", `","`, `"`, "", "", "")
	f.Fuzz(func(t *testing.T, a1, a2, a3, a4, a5, b1, b2, b3, b4, b5 string) {
		a := Request{PrincipalReference: a1, Action: a2, ResourceType: a3, ResourceKey: a4, OrganizationID: a5}
		b := Request{PrincipalReference: b1, Action: b2, ResourceType: b3, ResourceKey: b4, OrganizationID: b5}
		if a != b && cacheKey(a) == cacheKey(b) {
			t.Fatalf("distinct requests share a key: %q %q", []string{a1, a2, a3, a4, a5}, []string{b1, b2, b3, b4, b5})
		}
	})
}

func TestCacheKeyNoRawReference(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	r := baseRequest()
	c.Decide(context.Background(), r)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.entries {
		if strings.Contains(fmt.Sprintf("%x %+v", k, *el.Value.(*cacheEntry)), r.PrincipalReference) {
			t.Fatal("an entry holds the raw principal reference")
		}
	}
}

func TestCacheRevisionBumpEvicts(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	c.Decide(context.Background(), baseRequest())
	p.set(func(wireRequest) verdict { return verdict{rev: 6} })
	c.Decide(context.Background(), requestKey("full-chaos/other"))
	if c.Len() != 0 || stored(c, baseRequest()) {
		t.Fatalf("Len %d after a revision bump, want 0", c.Len())
	}
	c.Decide(context.Background(), baseRequest())
	wantCalls(t, p, 3)
}

func TestCacheOlderRevisionNotStored(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	p.set(func(wireRequest) verdict { return verdict{rev: 6} })
	c.Decide(context.Background(), requestKey("full-chaos/other"))
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5} })
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	if c.Len() != 0 {
		t.Fatalf("an allow below the watermark was stored")
	}
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	wantCalls(t, p, 3)
}

func TestCacheWatermarkNeverLowers(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	p.set(func(wireRequest) verdict { return verdict{rev: 6} })
	c.Decide(context.Background(), requestKey("full-chaos/six"))
	p.set(func(wireRequest) verdict { return verdict{rev: 4} })
	c.Decide(context.Background(), requestKey("full-chaos/four"))
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5} })
	c.Decide(context.Background(), baseRequest())
	if c.Len() != 0 {
		t.Fatal("the watermark went down: an allow at 5 was stored after 6")
	}
}

func TestCacheNewerDenyTombstonesOlderAllow(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	release := heldCall(t, p, c, baseRequest(), verdict{allow: true, rev: 5})
	p.set(func(wireRequest) verdict { return verdict{rev: 5} })
	wantDeny(t, c.Decide(context.Background(), baseRequest()), ReasonRoleLacksAction)
	wantAllow(t, release())
	if c.Len() != 0 || stored(c, baseRequest()) {
		t.Fatal("an allow older than a deny for its key was stored")
	}
}

func TestCacheNotCacheableAllowRemovesEntry(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	release := heldCall(t, p, c, baseRequest(), verdict{allow: true, rev: 5, expires: testNow})
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	if !stored(c, baseRequest()) {
		t.Fatal("fixture: the cacheable allow was not stored")
	}
	wantAllow(t, release())
	if c.Len() != 0 || stored(c, baseRequest()) {
		t.Fatal("a newer not-cacheable allow left the older entry")
	}
}

func TestCacheInvalidateDropsAll(t *testing.T) {
	for name, invalidate := range map[string]func(*CachedClient) error{
		"Invalidate":    func(c *CachedClient) error { return c.Invalidate(context.Background(), "grant", "grt_1", 3) },
		"InvalidateAll": func(c *CachedClient) error { return c.InvalidateAll(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			c := p.cached(t, CacheConfig{})
			c.Decide(context.Background(), baseRequest())
			c.Decide(context.Background(), requestKey("full-chaos/other"))
			if err := invalidate(c); err != nil {
				t.Fatal(err)
			}
			if c.Len() != 0 {
				t.Fatalf("Len %d after %s", c.Len(), name)
			}
			c.Decide(context.Background(), baseRequest())
			wantCalls(t, p, 3)
		})
	}
}

func TestCacheInvalidateDuringCallRefusesPut(t *testing.T) {
	for name, invalidate := range map[string]func(*CachedClient) error{
		"Invalidate":    func(c *CachedClient) error { return c.Invalidate(context.Background(), "membership", "mem_1", 9) },
		"InvalidateAll": func(c *CachedClient) error { return c.InvalidateAll(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			c := p.cached(t, CacheConfig{})
			release := heldCall(t, p, c, baseRequest(), verdict{allow: true, rev: 5})
			if err := invalidate(c); err != nil {
				t.Fatal(err)
			}
			wantAllow(t, release())
			if c.Len() != 0 {
				t.Fatalf("an allow asked before %s was stored", name)
			}
		})
	}
}

func TestCacheSizeBoundLRU(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{MaxEntries: 3})
	a, b, cc, d := requestKey("full-chaos/a"), requestKey("full-chaos/b"), requestKey("full-chaos/c"), requestKey("full-chaos/d")
	for _, r := range []Request{a, b, cc} {
		c.Decide(context.Background(), r)
	}
	c.Decide(context.Background(), a)
	wantCalls(t, p, 3)
	c.Decide(context.Background(), d)
	if c.Len() != 3 {
		t.Fatalf("Len %d, want the bound 3", c.Len())
	}
	for _, r := range []Request{a, cc, d} {
		if !stored(c, r) {
			t.Fatalf("%s was evicted; b is the least recently used", r.ResourceKey)
		}
	}
	if stored(c, b) {
		t.Fatal("the least recently used entry b survived")
	}
}

func TestCacheEvictedTombstoneFloor(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{MaxEntries: 1})
	release := heldCall(t, p, c, baseRequest(), verdict{allow: true, rev: 5})
	p.set(func(q wireRequest) verdict {
		return verdict{allow: q.Resource.Key != baseRequest().ResourceKey, rev: 5}
	})
	wantDeny(t, c.Decide(context.Background(), baseRequest()), ReasonRoleLacksAction)
	wantAllow(t, c.Decide(context.Background(), requestKey("full-chaos/other")))
	if !stored(c, requestKey("full-chaos/other")) {
		t.Fatal("fixture: the other allow was not stored")
	}
	wantAllow(t, release())
	if stored(c, baseRequest()) {
		t.Fatal("an allow older than an evicted tombstone was stored")
	}
}

func TestCacheCloseStopsCaching(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	c.Decide(context.Background(), baseRequest())
	c.Close()
	if c.Len() != 0 {
		t.Fatalf("Len %d after Close", c.Len())
	}
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	wantAllow(t, c.Decide(context.Background(), baseRequest()))
	wantCalls(t, p, 3)
	if c.Len() != 0 || stored(c, baseRequest()) {
		t.Fatalf("an allow was stored after Close (Len %d)", c.Len())
	}
	if err := c.Invalidate(context.Background(), "grant", "grt_1", 1); !errors.Is(err, ErrCacheClosed) {
		t.Fatalf("Invalidate after Close: %v", err)
	}
	if err := c.InvalidateAll(context.Background()); !errors.Is(err, ErrCacheClosed) {
		t.Fatalf("InvalidateAll after Close: %v", err)
	}
}

func TestCacheZeroAndNilDeny(t *testing.T) {
	var zero CachedClient
	var nilClient *CachedClient
	for name, c := range map[string]*CachedClient{"zero": &zero, "nil": nilClient} {
		t.Run(name, func(t *testing.T) {
			wantDeny(t, c.Decide(context.Background(), baseRequest()), ReasonNotConfigured)
			out := c.DecideBatch(context.Background(), []Request{baseRequest(), baseRequest()})
			if len(out) != 2 {
				t.Fatalf("%d decisions", len(out))
			}
			for _, d := range out {
				wantDeny(t, d, ReasonNotConfigured)
			}
			if out := c.DecideBatch(context.Background(), nil); out == nil || len(out) != 0 {
				t.Fatalf("empty batch: %v", out)
			}
			if c.Len() != 0 || !errors.Is(c.InvalidateAll(context.Background()), ErrCacheClosed) || !errors.Is(c.Invalidate(context.Background(), "a", "b", 1), ErrCacheClosed) {
				t.Fatal("a zero cache is not closed")
			}
			c.Close()
		})
	}
}

func TestCacheInvalidRequestTouchesNothing(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	bad := baseRequest()
	bad.Action = "bare"
	wantDeny(t, c.Decide(context.Background(), bad), ReasonInvalidRequest)
	wantDeny(t, c.Decide(nil, baseRequest()), ReasonInvalidRequest)
	for _, d := range c.DecideBatch(context.Background(), []Request{baseRequest(), bad}) {
		wantDeny(t, d, ReasonInvalidRequest)
	}
	wantCalls(t, p, 0)
}

func TestCacheBatchServesHitsSendsMisses(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(q wireRequest) verdict { return verdict{allow: !strings.Contains(q.Resource.Key, "deny"), rev: 5} })
	c := p.cached(t, CacheConfig{})
	r0, r1, r2 := requestKey("full-chaos/allow-zero"), requestKey("full-chaos/allow-one"), requestKey("full-chaos/deny-two")
	hit := c.Decide(context.Background(), r1)
	out := c.DecideBatch(context.Background(), []Request{r0, r1, r2})
	if len(out) != 3 {
		t.Fatalf("%d decisions", len(out))
	}
	wantAllow(t, out[0])
	wantAllow(t, out[1])
	wantDeny(t, out[2], ReasonRoleLacksAction)
	if out[1].DecisionID() != hit.DecisionID() {
		t.Fatal("the hit was not served from the cache")
	}
	p.mu.Lock()
	sent := p.sent
	p.mu.Unlock()
	if len(sent) != 1 || strings.Join(sent[0], " ") != "full-chaos/allow-zero full-chaos/deny-two" {
		t.Fatalf("batches sent %v, want the two misses in order", sent)
	}
}

func TestCacheBatchAllHitsNoCall(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	r0, r1 := requestKey("full-chaos/a"), requestKey("full-chaos/b")
	c.Decide(context.Background(), r0)
	c.Decide(context.Background(), r1)
	for _, d := range c.DecideBatch(context.Background(), []Request{r0, r1}) {
		wantAllow(t, d)
	}
	wantCalls(t, p, 2)
}

func TestCacheBatchFailureDeniesHitsToo(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{})
	r0, r1 := requestKey("full-chaos/a"), requestKey("full-chaos/b")
	c.Decide(context.Background(), r1)
	p.fail(http.StatusServiceUnavailable)
	out := c.DecideBatch(context.Background(), []Request{r0, r1})
	for _, d := range out {
		wantDeny(t, d, ReasonUnavailable)
	}
}

func TestCacheBatchStoresEachCacheableAllow(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(q wireRequest) verdict {
		switch q.Resource.Key {
		case "full-chaos/deny":
			return verdict{rev: 5}
		case "full-chaos/once":
			return verdict{allow: true, rev: 5, expires: clock.Now()}
		}
		return verdict{allow: true, rev: 5}
	})
	c := p.cached(t, CacheConfig{})
	reqs := []Request{requestKey("full-chaos/keep"), requestKey("full-chaos/deny"), requestKey("full-chaos/once")}
	c.DecideBatch(context.Background(), reqs)
	if c.Len() != 1 || !stored(c, reqs[0]) {
		t.Fatalf("Len %d; want only the cacheable allow stored", c.Len())
	}
}

func TestCacheConcurrentRace(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	c := p.cached(t, CacheConfig{MaxEntries: 8})
	keys := make([]Request, 12)
	for i := range keys {
		keys[i] = requestKey(fmt.Sprintf("full-chaos/k%d", i))
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				switch (g + i) % 5 {
				case 0:
					c.DecideBatch(context.Background(), []Request{keys[i%12], keys[(i+g)%12]})
				case 1:
					_ = c.InvalidateAll(context.Background())
				default:
					c.Decide(context.Background(), keys[(g*i)%12])
				}
			}
		}(g)
	}
	time.Sleep(50 * time.Millisecond)
	p.set(func(wireRequest) verdict { return verdict{rev: 7} })
	wantDeny(t, c.Decide(context.Background(), requestKey("full-chaos/flip")), ReasonRoleLacksAction)
	for round := 0; round < 20; round++ {
		for _, r := range keys {
			if d := c.Decide(context.Background(), r); d.Allowed() {
				t.Fatalf("an allow for %s after the PDP denied at a newer revision", r.ResourceKey)
			}
		}
	}
	close(stop)
	wg.Wait()
	if c.Len() > 8 {
		t.Fatalf("Len %d over the bound", c.Len())
	}
}

func TestCacheHitKeepsObligations(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(wireRequest) verdict {
		return verdict{allow: true, rev: 5, obligations: `{"tenant_scope":"` + testOrg + `","repository_allowlist":["full-chaos/dev-health-ops"],"redaction":["free_text"]}`}
	})
	c := p.cached(t, CacheConfig{})
	first := c.Decide(context.Background(), baseRequest())
	list := first.Obligations().RepositoryAllowlist()
	list[0] = "full-chaos/evil"
	red := first.Obligations().Redaction()
	red[0] = RedactContactDetails
	hit := c.Decide(context.Background(), baseRequest())
	wantCalls(t, p, 1)
	if got := hit.Obligations().RepositoryAllowlist(); len(got) != 1 || got[0] != "full-chaos/dev-health-ops" {
		t.Fatalf("hit allowlist %v", got)
	}
	if !hit.Obligations().Redacts(RedactFreeText) || hit.Obligations().Redacts(RedactContactDetails) {
		t.Fatal("hit redaction changed")
	}
	if s, ok := hit.Obligations().TenantScope(); !ok || s != testOrg {
		t.Fatalf("hit tenant %q", s)
	}
}

// The audit obligation is not on the wire yet: an allow that carries an
// unknown obligation key is a local deny, so it is never stored.
func TestCacheAuditObligationIsNeverStored(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5, obligations: `{"audit":true}`} })
	c := p.cached(t, CacheConfig{})
	wantDeny(t, c.Decide(context.Background(), baseRequest()), ReasonUnknownObligation)
	wantDeny(t, c.Decide(context.Background(), baseRequest()), ReasonUnknownObligation)
	wantCalls(t, p, 2)
}

func TestCachePolicyRevisionAt2To63Denies(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 1 << 63} })
	c := p.cached(t, CacheConfig{})
	wantDeny(t, c.Decide(context.Background(), baseRequest()), ReasonMalformed)
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5} })
	c.Decide(context.Background(), baseRequest())
	if !stored(c, baseRequest()) {
		t.Fatal("a refused revision moved the watermark")
	}
}

type catalogAction struct {
	Action       string `json:"action"`
	ResourceKind string `json:"resource_kind"`
	RiskClass    string `json:"risk_class"`
	CacheClass   string `json:"cache_class"`
}

// Every high-risk action of the synced ops catalog has cache class none, and
// the server answers a class none allow with expires_at = the decision time.
// Such an allow is not Cacheable, so it is never served from the cache and
// every call reaches the PDP and its audit row.
func TestCacheHighRiskCatalogActionsAreNeverCached(t *testing.T) {
	var catalog struct {
		Actions []catalogAction `json:"actions"`
	}
	readJSON(t, &catalog, "catalog", "actions.v1.json")
	high := 0
	for _, a := range catalog.Actions {
		if a.RiskClass != "high" {
			continue
		}
		high++
		if a.CacheClass != "none" {
			t.Fatalf("high-risk action %s has cache_class %q: the PDP may give it a reusable expiry", a.Action, a.CacheClass)
		}
		t.Run(a.Action, func(t *testing.T) {
			clock := newClock()
			p := newCachePDP(t, clock)
			p.set(func(wireRequest) verdict { return verdict{allow: true, rev: 5, expires: clock.Now()} })
			c := p.cached(t, CacheConfig{})
			r := baseRequest()
			r.Action = a.Action
			switch a.ResourceKind {
			case "platform":
				r.ResourceType, r.ResourceKey, r.OrganizationID = "platform", "platform", ""
			case "organization":
				r.ResourceType, r.ResourceKey = "organization", "acme"
			}
			for i := 0; i < 2; i++ {
				d := c.Decide(context.Background(), r)
				wantAllow(t, d)
				if d.Cacheable() {
					t.Fatalf("high-risk %s came back Cacheable", a.Action)
				}
			}
			wantCalls(t, p, 2)
			if c.Len() != 0 {
				t.Fatalf("high-risk %s was stored", a.Action)
			}
		})
	}
	if high == 0 {
		t.Fatal("the catalog holds no high-risk action")
	}
}
