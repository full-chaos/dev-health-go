package pep

import (
	"strings"
	"testing"
)

func TestRequestAcceptsNestedRepositoryKeys(t *testing.T) {
	for key, want := range map[string]bool{
		"full-chaos/dev-health-ops":    true,
		"group/sub/repo":               true,
		strings.Repeat("g/", 19) + "r": true,
		strings.Repeat("g/", 20) + "r": false,
		"*":                            true,
		"group/Sub/repo":               false,
		"group/sub/repo/":              false,
		"onlyone":                      false,
	} {
		r := baseRequest()
		r.ResourceKey = key
		if got := r.valid(); got != want {
			t.Errorf("valid() with key %q = %v, want %v", key, got, want)
		}
	}
}

func TestObligationAllowlistAcceptsNestedKeys(t *testing.T) {
	o := decideAllow(t, `"obligations": {"repository_allowlist": ["group/sub/repo"]}`).Obligations()
	if !o.RepositoryAllowed("group/sub/repo") || o.RepositoryAllowed("group/sub") {
		t.Fatal("nested allowlist entry lost")
	}
}
