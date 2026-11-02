package pep

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// handlerTransport answers in memory: no listener.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func newMemCachePDP(t *testing.T, clock *testClock, cfg CacheConfig) (*cachePDP, *CachedClient) {
	t.Helper()
	p := &cachePDP{t: t, clock: clock, status: http.StatusOK, arrived: make(chan string, 256)}
	p.answer = func(wireRequest) verdict { return verdict{allow: true, rev: 5} }
	hc := &http.Client{Transport: handlerTransport{http.HandlerFunc(p.serve)}}
	c, err := New(Config{BaseURL: "http://pdp.invalid", AllowPlainHTTP: true, HTTPClient: hc, ClientService: "dev-health-web", Timeout: 5 * time.Second, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Now == nil {
		cfg.Now = clock.Now
	}
	cc, err := NewCachedClient(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, cc
}

func sentBatches(p *cachePDP) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var s []string
	for _, b := range p.sent {
		s = append(s, "["+strings.Join(b, " ")+"]")
	}
	return strings.Join(s, " ")
}

// A batch must not return a cached allow below the watermark that its own miss
// answers raised: at revision 6 the PDP denies the cached keys.
func TestCacheBatchHitBelowWatermarkReasked(t *testing.T) {
	hit0, hit1, miss := requestKey("full-chaos/hit-0"), requestKey("full-chaos/hit-1"), requestKey("full-chaos/miss")
	for _, tc := range []struct {
		name  string
		reqs  []Request
		hits  []int
		sent  string
		allow int
	}{
		{"hit_before_miss", []Request{hit0, miss}, []int{0}, "[full-chaos/miss] [full-chaos/hit-0]", 1},
		{"hit_after_miss", []Request{miss, hit0}, []int{1}, "[full-chaos/miss] [full-chaos/hit-0]", 0},
		{"two_hits_around_miss", []Request{hit0, miss, hit1}, []int{0, 2}, "[full-chaos/miss] [full-chaos/hit-0 full-chaos/hit-1]", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c := newMemCachePDP(t, newClock(), CacheConfig{})
			wantAllow(t, c.Decide(context.Background(), hit0))
			wantAllow(t, c.Decide(context.Background(), hit1))
			p.set(func(q wireRequest) verdict { return verdict{allow: q.Resource.Key == miss.ResourceKey, rev: 6} })
			out := c.DecideBatch(context.Background(), tc.reqs)
			if len(out) != len(tc.reqs) {
				t.Fatalf("%d decisions", len(out))
			}
			for _, i := range tc.hits {
				if out[i].Allowed() {
					t.Errorf("returned cached allow at index %d with revision %d after observing revision 6", i, out[i].PolicyRevision())
				}
				wantDeny(t, out[i], ReasonRoleLacksAction)
			}
			wantAllow(t, out[tc.allow])
			if got := sentBatches(p); got != tc.sent {
				t.Fatalf("batches sent %s, want %s", got, tc.sent)
			}
		})
	}
}

// The re-ask of the hits is one more PDP call: when it fails, the whole batch
// is denied, as for the miss call.
func TestCacheBatchReaskFailureDeniesAll(t *testing.T) {
	hit, miss := requestKey("full-chaos/hit-0"), requestKey("full-chaos/miss")
	p, c := newMemCachePDP(t, newClock(), CacheConfig{})
	wantAllow(t, c.Decide(context.Background(), hit))
	p.set(func(q wireRequest) verdict {
		p.fail(http.StatusServiceUnavailable)
		return verdict{allow: true, rev: 6}
	})
	for i, d := range c.DecideBatch(context.Background(), []Request{hit, miss}) {
		if d.Allowed() {
			t.Errorf("index %d allowed (revision %d) after the re-ask failed", i, d.PolicyRevision())
		}
		wantDeny(t, d, ReasonUnavailable)
	}
	if got := sentBatches(p); got != "[full-chaos/miss] [full-chaos/hit-0]" {
		t.Fatalf("batches sent %s", got)
	}
}

// A higher revision seen by another call while the batch waits on its miss
// call makes the batch's hit stale too.
func TestCacheBatchConcurrentWatermarkCell(t *testing.T) {
	hit, miss, other := requestKey("full-chaos/hit-0"), requestKey("full-chaos/miss"), requestKey("full-chaos/other")
	p, c := newMemCachePDP(t, newClock(), CacheConfig{})
	wantAllow(t, c.Decide(context.Background(), hit))
	reached, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	p.set(func(q wireRequest) verdict {
		if q.Resource.Key == miss.ResourceKey {
			close(reached)
			<-gate
			return verdict{allow: true, rev: 5}
		}
		return verdict{rev: 6}
	})
	done := make(chan []Decision, 1)
	go func() { done <- c.DecideBatch(context.Background(), []Request{hit, miss}) }()
	<-reached
	wantDeny(t, c.Decide(context.Background(), other), ReasonRoleLacksAction)
	once.Do(func() { close(gate) })
	out := <-done
	if out[0].Allowed() {
		t.Fatalf("returned cached allow at revision %d after concurrent watermark reached 6", out[0].PolicyRevision())
	}
	wantDeny(t, out[0], ReasonRoleLacksAction)
	wantAllow(t, out[1])
	if got := sentBatches(p); got != "[full-chaos/miss] [full-chaos/hit-0]" {
		t.Fatalf("batches sent %s", got)
	}
}

// Race variant: the PDP revision climbs while batches mix hits and a fresh
// miss. The miss is first, so the PDP answers it before every other item of
// the batch. No returned item may carry a revision below that answer: a hit is
// at or above the watermark the batch observed, and every other answer comes
// later.
func TestCacheBatchConcurrentWatermarkRace(t *testing.T) {
	p, c := newMemCachePDP(t, newClock(), CacheConfig{MaxEntries: 64})
	var rev atomic.Uint64
	rev.Store(5)
	p.set(func(wireRequest) verdict { return verdict{allow: true, rev: rev.Load()} })
	shared := make([]Request, 4)
	for i := range shared {
		shared[i] = requestKey(fmt.Sprintf("full-chaos/shared-%d", i))
	}
	stop := make(chan struct{})
	var bumps sync.WaitGroup
	bumps.Add(1)
	go func() {
		defer bumps.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rev.Add(1)
			time.Sleep(50 * time.Microsecond)
		}
	}()
	var wg sync.WaitGroup
	var stale atomic.Int32
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m := requestKey(fmt.Sprintf("full-chaos/miss-%d-%d", g, i))
				reqs := []Request{m, shared[(g+i)%4], shared[(g+i+1)%4]}
				c.Decide(context.Background(), shared[i%4])
				out := c.DecideBatch(context.Background(), reqs)
				floor := out[0].PolicyRevision()
				for j, d := range out {
					if d.Allowed() && d.PolicyRevision() < floor {
						stale.Add(1)
						t.Errorf("goroutine %d batch %d: index %d allowed at revision %d below the miss answer %d", g, i, j, d.PolicyRevision(), floor)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	bumps.Wait()
	if stale.Load() != 0 {
		t.Fatalf("%d batches returned a stale cached allow", stale.Load())
	}
}
