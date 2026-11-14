package pep

import (
	"bytes"
	"context"
	"testing"
)

// Vectors for expires_at equal to the decision time (testNow). These sit beside
// the synced corpus, not in it: testdata is a byte-for-byte copy of the ops
// corpus and a sync would erase them.
const atNow = `2026-09-03T05:00:00Z`

func TestExpiresAtEqualToDecisionTimeIsAcceptedNotCacheable(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		allowed bool
	}{
		{"allow", replaceOnce(t, goodAllow(t), `2026-09-03T05:00:30Z`, atNow), true},
		{"deny", replaceOnce(t, readFile(t, "authorization_decision", "decision-valid-deny.json"), `2026-09-03T05:00:30Z`, atNow), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideBody(t, c.body, baseRequest())
			if d.Allowed() != c.allowed || d.Local() || d.Cacheable() {
				t.Fatalf("allowed=%v local=%v cacheable=%v", d.Allowed(), d.Local(), d.Cacheable())
			}
		})
	}
}

func TestBatchExpiresAtEqualToDecisionTimeIsAcceptedNotCacheable(t *testing.T) {
	raw := readFile(t, "authorization_batch_response", "batch-response-valid-item-expired-not-cacheable.json")
	raw = replaceOnce(t, raw, `2000-01-01T00:00:00Z`, atNow)
	raw = bytes.Replace(raw, []byte(`2026-09-03T05:00:30Z`), []byte(atNow), 1)
	if bytes.Contains(raw, []byte(`2000-01-01`)) || bytes.Contains(raw, []byte(`05:00:30Z`)) {
		t.Fatal("fixture still holds another expires_at")
	}
	m := readManifest(t, "authorization_batch_response")
	got := newPDP(t, replyJSON(raw)).client(t).DecideBatch(context.Background(), batchRequests(t, m))
	if len(got) != 2 {
		t.Fatalf("%d decisions", len(got))
	}
	for i, want := range []bool{true, false} {
		if got[i].Local() || got[i].Allowed() != want || got[i].Cacheable() {
			t.Fatalf("item %d: allowed=%v local=%v cacheable=%v", i, got[i].Allowed(), got[i].Local(), got[i].Cacheable())
		}
	}
}
