package pep

import "testing"

// A PDP that sends allowed_by_platform_admin ahead of the contract gets its allow through, with the reason it sent.
func TestADecisionWithTheAcceptAheadReasonIsAnAllow(t *testing.T) {
	body := readFile(t, "authorization_decision", "decision-valid-reason-allowed-by-role.json")
	body = replaceOnce(t, body, `"reason_code": "allowed_by_role"`, `"reason_code": "allowed_by_platform_admin"`)
	d := decideBody(t, body, baseRequest())
	if !d.Allowed() || d.Local() || d.Err() != nil || d.Reason() != ReasonAllowedByPlatformAdmin {
		t.Fatalf("allowed=%v local=%v err=%v reason=%s, want an allow with allowed_by_platform_admin", d.Allowed(), d.Local(), d.Err(), d.Reason())
	}
}

// The accept-ahead reason is an allow reason only: a deny that names it is malformed, as a deny naming allowed_by_role is.
func TestADenyWithTheAcceptAheadReasonIsMalformed(t *testing.T) {
	body := readFile(t, "authorization_decision", "decision-valid-deny.json")
	body = replaceOnce(t, body, `"reason_code": "grant_absent"`, `"reason_code": "allowed_by_platform_admin"`)
	d := decideBody(t, body, baseRequest())
	if d.Allowed() || !d.Local() || d.Reason() != ReasonMalformed {
		t.Fatalf("allowed=%v local=%v reason=%s, want a local %s deny", d.Allowed(), d.Local(), d.Reason(), ReasonMalformed)
	}
}

// A code outside both the contract and the accept-ahead set is still malformed.
func TestAnAllowWithAnUnknownReasonIsStillMalformed(t *testing.T) {
	body := readFile(t, "authorization_decision", "decision-valid-reason-allowed-by-role.json")
	body = replaceOnce(t, body, `"reason_code": "allowed_by_role"`, `"reason_code": "allowed_by_superuser"`)
	if d := decideBody(t, body, baseRequest()); d.Allowed() || d.Reason() != ReasonMalformed {
		t.Fatalf("allowed=%v reason=%s, want a local %s deny", d.Allowed(), d.Reason(), ReasonMalformed)
	}
}
