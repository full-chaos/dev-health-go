package pep

// A bounded allow-only decision cache in front of a Client.
//
// WHY ALLOW ONLY. A cached deny saves a call but keeps a principal out after a
// grant; a cached allow is the risk this cache bounds. A deny is never stored,
// so a deny is never served from here: every deny is a fresh PDP verdict or a
// local deny. A PDP verdict that is not a cacheable allow (a deny, or an allow
// whose expires_at is not after the time it was read) removes the entry for
// its key and leaves a tombstone, so an older allow that is still in flight
// cannot land over it.
//
// WHY EXPIRY IS THE ONLY RISK SIGNAL. The client has no action catalog. The
// server sends expires_at = decision time for a deny and for every allow of a
// cache class none action, which is every high-risk action (the catalog
// refuses a high-risk action with any other class). Such a decision is
// Cacheable() == false and is used for its own request only, so a high-risk
// allow always reaches the PDP and its audit row. The client trusts the
// server's expiry for this; it cannot check the risk class itself.
//
// WHY MaxTTL. An entry dies at the earlier of the wire expires_at and MaxTTL
// after the request was sent. MaxTTL is at most the server's 30 s TTL; callers
// may only lower it. The send time is read before the call and the age is
// measured from it, on the monotonic clock when Now is time.Now, so a server
// clock ahead of this one cannot stretch reuse past MaxTTL. A clock that steps
// back is a miss.
//
// WHY THE KEY. The key is the principal reference (one principal and one
// session at the PDP), the action, the resource type and key and the
// organization. The request id is not in the key: it names one call, not a
// question. The key is the sha256 of the quoted fields, so the reference is not
// held in memory.
//
// REVISIONS. The client sees only policy_revision. The watermark is the
// highest revision of any PDP verdict and only moves up. When it moves up,
// every entry below it is dropped; an allow below it goes to its caller and is
// not stored. A batch whose hits are below the watermark once its misses are
// answered asks those items again. The wire carries no resource, membership or grant revision, so
// Invalidate drops every entry whatever aggregate it names, and InvalidateAll
// does the same. When nobody calls them, the expiry bounds reuse.
//
// IN-FLIGHT CALLS. Each miss takes a stamp before the PDP call. The answer is
// not stored when, after the stamp, the cache was invalidated or the key got a
// tombstone. When the size bound drops a tombstone, its stamp becomes a floor
// for every key: an allow older than a dropped tombstone is never stored.
//
// NO SINGLE-FLIGHT. Each miss is its own PDP call, so each request keeps its
// own request id and its own PDP audit row, and one caller's decision is never
// handed to another caller's request. A hit makes no PDP call.
//
// NAMED LIMITS. No risk class (see above). No resource revisions (see above).
// No audit obligation on the wire yet: an unknown obligation key denies the
// whole decision today, so an allow that carries one is never stored; when the
// client learns an audit obligation, an allow that carries it must not be
// stored.

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCacheEntries bounds a CachedClient when CacheConfig.MaxEntries is 0.
	DefaultCacheEntries = 10000
	// DefaultCacheTTL is the ceiling of MaxTTL: the server's decision cache TTL.
	DefaultCacheTTL = 30 * time.Second
)

// ErrCacheClosed is returned by Invalidate and InvalidateAll after Close, and
// by a zero or nil CachedClient.
var ErrCacheClosed = errors.New("pep: decision cache closed")

// CacheConfig bounds a CachedClient.
type CacheConfig struct {
	// MaxEntries bounds the entries, tombstones included. 0 is DefaultCacheEntries.
	MaxEntries int
	// MaxTTL bounds the reuse of one allow. 0 is DefaultCacheTTL; it may not be
	// above DefaultCacheTTL.
	MaxTTL time.Duration
	// Now is the clock of expiry checks. Default time.Now.
	Now func() time.Time
}

// CachedClient is a Client with a bounded allow-only cache. The zero value and
// a nil *CachedClient deny.
type CachedClient struct {
	client     *Client
	maxEntries int
	maxTTL     time.Duration
	now        func() time.Time

	mu        sync.Mutex
	entries   map[[32]byte]*list.Element
	lru       *list.List
	allows    int
	seq       uint64
	lastFlush uint64
	floor     uint64
	watermark uint64
	closed    bool
}

type cacheEntry struct {
	key      [32]byte
	live     bool
	decision Decision
	asked    time.Time
	guard    uint64
}

var _ Decider = (*CachedClient)(nil)

// NewCachedClient wraps a ready Client. A bad configuration is a startup error.
func NewCachedClient(c *Client, cfg CacheConfig) (*CachedClient, error) {
	if !c.ready() {
		return nil, errors.New("pep: a configured Client is required")
	}
	if cfg.MaxEntries < 0 {
		return nil, errors.New("pep: MaxEntries must not be negative")
	}
	if cfg.MaxTTL < 0 || cfg.MaxTTL > DefaultCacheTTL {
		return nil, errors.New("pep: MaxTTL must be between 0 and 30s")
	}
	cc := &CachedClient{client: c, maxEntries: cfg.MaxEntries, maxTTL: cfg.MaxTTL, now: cfg.Now,
		entries: map[[32]byte]*list.Element{}, lru: list.New()}
	if cc.maxEntries == 0 {
		cc.maxEntries = DefaultCacheEntries
	}
	if cc.maxTTL == 0 {
		cc.maxTTL = DefaultCacheTTL
	}
	if cc.now == nil {
		cc.now = time.Now
	}
	return cc, nil
}

func (c *CachedClient) ready() bool { return c != nil && c.client.ready() && c.entries != nil }

func cacheKey(r Request) [32]byte {
	fields := []string{r.PrincipalReference, r.Action, r.ResourceType, r.ResourceKey, r.OrganizationID}
	for i, f := range fields {
		fields[i] = strconv.Quote(f)
	}
	return sha256.Sum256([]byte(strings.Join(fields, ",")))
}

// Decide answers from the cache or asks the PDP. Every failure is a local deny.
func (c *CachedClient) Decide(ctx context.Context, req Request) Decision {
	if !c.ready() {
		return deny(fail(ReasonNotConfigured))
	}
	if ctx == nil || !req.valid() {
		return deny(fail(ReasonInvalidRequest))
	}
	key := cacheKey(req)
	if d, ok := c.get(key); ok {
		return d
	}
	stamp, asked := c.stamp()
	d := c.client.Decide(ctx, req)
	c.observe(key, d, stamp, asked)
	return d
}

// DecideBatch serves the hits and sends the misses as one batch, in request
// order. As with Client.DecideBatch, a batch over MaxBatch or with one invalid
// request is denied whole, and a failure of the miss call denies every item,
// the hits too. When, after the miss answers, a hit is below the watermark,
// every hit is asked again in one more call, so no item that comes back was
// read from the cache before the watermark moved; a failure of that call also
// denies every item.
func (c *CachedClient) DecideBatch(ctx context.Context, reqs []Request) []Decision {
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
	out := make([]Decision, len(reqs))
	keys := make([][32]byte, len(reqs))
	var hits, misses []int
	for i, r := range reqs {
		keys[i] = cacheKey(r)
		if d, ok := c.get(keys[i]); ok {
			out[i] = d
			hits = append(hits, i)
		} else {
			misses = append(misses, i)
		}
	}
	if len(misses) > 0 {
		if e := c.ask(ctx, reqs, keys, misses, out); e != nil {
			return denyAll(e, len(reqs))
		}
	}
	if c.belowWatermark(out, hits) {
		if e := c.ask(ctx, reqs, keys, hits, out); e != nil {
			return denyAll(e, len(reqs))
		}
	}
	return out
}

// ask sends the items at idx as one batch and records the answers in out. A
// local deny of the call is returned as the error that denies the batch.
func (c *CachedClient) ask(ctx context.Context, reqs []Request, keys [][32]byte, idx []int, out []Decision) *Error {
	sub := make([]Request, len(idx))
	for j, i := range idx {
		sub[j] = reqs[i]
	}
	stamp, asked := c.stamp()
	got := c.client.DecideBatch(ctx, sub)
	for _, d := range got {
		if d.Local() {
			e, _ := d.Err().(*Error)
			if e == nil {
				e = fail(ReasonNoDecision)
			}
			return e
		}
	}
	for j, i := range idx {
		c.observe(keys[i], got[j], stamp, asked)
		out[i] = got[j]
	}
	return nil
}

// belowWatermark reports that a decision read from the cache at one of idx is
// below the current watermark.
func (c *CachedClient) belowWatermark(out []Decision, idx []int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, i := range idx {
		if out[i].PolicyRevision() < c.watermark {
			return true
		}
	}
	return false
}

// Invalidate drops every entry. The wire does not say which aggregates a
// decision read, so a change to any aggregate drops them all; aggregateType,
// aggregateID and revision are not used. It matches the revision feed's
// invalidator method set.
func (c *CachedClient) Invalidate(ctx context.Context, aggregateType, aggregateID string, revision int64) error {
	return c.InvalidateAll(ctx)
}

// InvalidateAll drops every entry, and no call that started before it stores
// its answer.
func (c *CachedClient) InvalidateAll(context.Context) error {
	if !c.ready() {
		return ErrCacheClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrCacheClosed
	}
	c.seq++
	c.lastFlush = c.seq
	c.clear()
	return nil
}

// Close drops every entry. Afterwards every call goes to the PDP and nothing
// is stored.
func (c *CachedClient) Close() {
	if !c.ready() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.clear()
}

// Len is the number of stored allows.
func (c *CachedClient) Len() int {
	if !c.ready() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allows
}

func (c *CachedClient) clear() {
	clear(c.entries)
	c.lru.Init()
	c.allows = 0
}

func (c *CachedClient) stamp() (uint64, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq, c.now()
}

// fresh reports that an entry may still be served at now.
func (c *CachedClient) fresh(e *cacheEntry, now time.Time) bool {
	age := now.Sub(e.asked)
	return age >= 0 && age < c.maxTTL && now.Before(e.decision.ExpiresAt()) && e.decision.PolicyRevision() >= c.watermark
}

func (c *CachedClient) get(key [32]byte) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok || c.closed {
		return Decision{}, false
	}
	e := el.Value.(*cacheEntry)
	if !e.live {
		return Decision{}, false
	}
	if !c.fresh(e, c.now()) {
		c.kill(e)
		return Decision{}, false
	}
	c.lru.MoveToFront(el)
	return e.decision, true
}

func (c *CachedClient) kill(e *cacheEntry) {
	if e.live {
		e.live = false
		e.decision = Decision{}
		c.allows--
	}
}

// observe records the PDP's answer to a miss taken at stamp. A local deny is
// not a verdict and changes nothing.
func (c *CachedClient) observe(key [32]byte, d Decision, stamp uint64, asked time.Time) {
	if d.Local() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if rev := d.PolicyRevision(); rev > c.watermark {
		c.watermark = rev
		for el := c.lru.Front(); el != nil; el = el.Next() {
			if e := el.Value.(*cacheEntry); e.live && e.decision.PolicyRevision() < rev {
				c.kill(e)
			}
		}
	}
	el, ok := c.entries[key]
	if !(d.Allowed() && d.Cacheable()) {
		c.seq++
		if !ok {
			el = c.insert(key)
		}
		e := el.Value.(*cacheEntry)
		c.kill(e)
		e.guard = c.seq
		c.lru.MoveToFront(el)
		c.bound()
		return
	}
	if stamp < c.lastFlush || stamp < c.floor {
		return
	}
	if ok && stamp < el.Value.(*cacheEntry).guard {
		return
	}
	e := &cacheEntry{key: key, live: true, decision: d, asked: asked}
	if !c.fresh(e, c.now()) {
		return
	}
	if !ok {
		el = c.insert(key)
	}
	old := el.Value.(*cacheEntry)
	if !old.live {
		c.allows++
	}
	e.guard = old.guard
	el.Value = e
	c.lru.MoveToFront(el)
	c.bound()
}

func (c *CachedClient) insert(key [32]byte) *list.Element {
	el := c.lru.PushFront(&cacheEntry{key: key})
	c.entries[key] = el
	return el
}

func (c *CachedClient) bound() {
	for c.lru.Len() > c.maxEntries {
		el := c.lru.Back()
		e := el.Value.(*cacheEntry)
		c.kill(e)
		if e.guard > c.floor {
			c.floor = e.guard
		}
		c.lru.Remove(el)
		delete(c.entries, e.key)
	}
}
