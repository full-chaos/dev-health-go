package pep

import (
	"bytes"
	"testing"
)

func TestRepositoryUnresolvedDenyIsAPDPDenyNotAFault(t *testing.T) {
	raw := readFile(t, "authorization_decision", "decision-valid-reason-repository-unresolved.json")
	control := bytes.ReplaceAll(raw, []byte("repository_unresolved"), []byte("repository_not_granted"))
	for name, tc := range map[string]struct {
		body   []byte
		reason string
	}{
		"repository_unresolved": {raw, "repository_unresolved"},
		"control":               {control, "repository_not_granted"},
	} {
		t.Run(name, func(t *testing.T) {
			d := decideBody(t, tc.body, baseRequest())
			if d.Allowed() || d.Local() || string(d.Reason()) != tc.reason {
				t.Fatalf("allowed=%v local=%v reason=%q, want a PDP deny %q", d.Allowed(), d.Local(), d.Reason(), tc.reason)
			}
		})
	}
}
