package pep

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRejectDuplicates(t *testing.T) {
	for in, ok := range map[string]bool{
		`{}`:                                  true,
		`{"a":1,"b":2}`:                       true,
		`{"a":{"a":1},"b":[{"a":1},{"a":2}]}`: true,
		`[1,2,[3]]`:                           true,
		`{"a":1,"a":2}`:                       false,
		`{"a":1,"\u0061":2}`:                  false,
		`{"a":{"b":1,"b":2}}`:                 false,
		`{"a":[{"b":1,"b":2}]}`:               false,
		`{"a":1} {}`:                          false,
		`{"a":1}x`:                            false,
		`{"a":1`:                              false,
		``:                                    false,
		`{"A":1,"a":2}`:                       true,
	} {
		err := rejectDuplicates([]byte(in))
		if (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", in, err, ok)
		}
	}
	deep := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	if rejectDuplicates([]byte(deep)) == nil {
		t.Error("a document nested past the depth bound was accepted")
	}
}

func TestPolicyRevisionToken(t *testing.T) {
	for in, ok := range map[string]bool{"0": true, "42": true, "9223372036854775807": true, "42.0": false, "4.2e1": false, "-1": false, "042": false, "9223372036854775808": false, "": false, "1 ": false, "true": false, `"1"`: false, "+1": false, "1e0": false} {
		_, err := parseRevision([]byte(in))
		if (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", in, err, ok)
		}
	}
}

func TestExpiresAtPattern(t *testing.T) {
	for in, ok := range map[string]bool{
		"2026-09-03T05:00:30Z":         true,
		"2026-09-03T05:00:30.123456Z":  true,
		"2026-09-03T05:00:30+02:00":    true,
		"2026-09-03T05:00:30.1234567Z": false,
		"2026-09-03T24:00:00Z":         false,
		"2026-09-03T05:00:30+24:00":    false,
		"0000-01-01T00:00:00Z":         false,
		"2026-02-30T05:00:30Z":         false,
		"2026-09-03 05:00:30Z":         false,
		"2026-09-03T05:00:30":          false,
		"2026-09-03T05:00:3Z":          false,
		"0001-01-01T00:00:00+23:59":    false,
		"2026-09-03T05:00:30z":         false,
		"٢٠٢٦-09-03T05:00:30Z":         false,
		"2026-09-03T05:00:30Z\n":       false,
		" 2026-09-03T05:00:30Z":        false,
		"9999-12-31T23:59:59Z":         true,
		"9999-12-31T23:59:59-00:01":    false,
	} {
		_, err := parseExpiry(in)
		if (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", in, err, ok)
		}
	}
}

func FuzzParseDecision(f *testing.F) {
	for _, set := range []string{"authorization_decision"} {
		entries, err := os.ReadDir(filepath.Join("testdata", set))
		if err != nil {
			f.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && e.Name() != "manifest.json" {
				f.Add(readFile(f, set, e.Name()))
			}
		}
	}
	f.Add([]byte(`{"allowed":true}`))
	f.Add([]byte(``))
	req := baseRequest()
	now := testNow
	f.Fuzz(func(t *testing.T, body []byte) {
		d := parseDecision(body, req, now)
		if !d.Allowed() {
			return
		}
		// An allow escaped parsing: it must hold the contract in full.
		if d.Local() || d.Err() != nil {
			t.Fatalf("an allow is local or carries an error: %q", body)
		}
		if err := rejectDuplicates(body); err != nil {
			t.Fatalf("an allow came from a document with a duplicate member or trailing data: %q", body)
		}
		if d.Cacheable() != d.ExpiresAt().After(now) {
			t.Fatalf("cacheable disagrees with the expiry: %q", body)
		}
		if !allowReasons[d.Reason()] {
			t.Fatalf("an allow with reason %q escaped: %q", d.Reason(), body)
		}
		if org, ok := d.OrganizationID(); !ok || org != req.OrganizationID {
			t.Fatalf("an allow for another organization escaped: %q", body)
		}
		if !decisionIDPattern.MatchString(d.DecisionID()) || !principalIDPattern.MatchString(d.PrincipalID()) {
			t.Fatalf("an allow with a bad id escaped: %q", body)
		}
		if !strings.Contains(string(body), `"allowed"`) {
			t.Fatalf("an allow without an allowed member escaped: %q", body)
		}
	})
}

func FuzzParseBatch(f *testing.F) {
	entries, err := os.ReadDir(filepath.Join("testdata", "authorization_batch_response"))
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != "manifest.json" {
			f.Add(readFile(f, "authorization_batch_response", e.Name()))
		}
	}
	reqs := batchRequests(f, readManifest(f, "authorization_batch_response"))
	f.Fuzz(func(t *testing.T, body []byte) {
		got := parseBatch(body, reqs, testNow)
		if len(got) != len(reqs) {
			t.Fatalf("%d decisions for %d requests: %q", len(got), len(reqs), body)
		}
		allowed, local := 0, 0
		for _, d := range got {
			if d.Allowed() {
				allowed++
			}
			if d.Local() {
				local++
			}
		}
		if local != 0 && local != len(got) {
			t.Fatalf("a batch is part refused: %q", body)
		}
		if allowed > 0 && rejectDuplicates(body) != nil {
			t.Fatalf("an allow came from a bad document: %q", body)
		}
	})
}

func TestParseDecisionNeverPanicsOnEmptyRequest(t *testing.T) {
	if parseDecision(nil, Request{}, time.Time{}).Allowed() {
		t.Fatal("allowed")
	}
	_ = context.Background()
}
