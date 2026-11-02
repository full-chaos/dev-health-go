package pep

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
	DefaultCacheEntries = 10000
	DefaultCacheTTL     = 30 * time.Second
)

var ErrCacheClosed = errors.New("pep: decision cache closed")

type CacheConfig struct {
	MaxEntries int
	MaxTTL     time.Duration
	Now        func() time.Time
}

// CachedClient is a stub: it stores nothing yet.
type CachedClient struct {
	client  *Client
	mu      sync.Mutex
	entries map[[32]byte]*list.Element
}

type cacheEntry struct {
	live bool
}

var _ Decider = (*CachedClient)(nil)

func NewCachedClient(c *Client, cfg CacheConfig) (*CachedClient, error) {
	return &CachedClient{client: c, entries: map[[32]byte]*list.Element{}}, nil
}

func cacheKey(r Request) [32]byte {
	fields := []string{r.PrincipalReference, r.Action, r.ResourceType, r.ResourceKey, r.OrganizationID}
	for i, f := range fields {
		fields[i] = strconv.Quote(f)
	}
	return sha256.Sum256([]byte(strings.Join(fields, ",")))
}

func (c *CachedClient) Decide(ctx context.Context, req Request) Decision {
	if c == nil {
		return Decision{}
	}
	return c.client.Decide(ctx, req)
}

func (c *CachedClient) DecideBatch(ctx context.Context, reqs []Request) []Decision {
	if c == nil {
		return nil
	}
	return c.client.DecideBatch(ctx, reqs)
}

func (c *CachedClient) Invalidate(ctx context.Context, aggregateType, aggregateID string, revision int64) error {
	return nil
}

func (c *CachedClient) InvalidateAll(context.Context) error { return nil }

func (c *CachedClient) Close() {}

func (c *CachedClient) Len() int { return 0 }
