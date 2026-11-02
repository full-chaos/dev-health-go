package pep

import (
	"bytes"
	"context"
	"strconv"
	"testing"
)

func decideAllow(t *testing.T, obligations string) Decision {
	t.Helper()
	body := replaceOnce(t, goodAllow(t), goodObligations, obligations)
	pdp := newPDP(t, replyJSON(body))
	return pdp.client(t).Decide(context.Background(), baseRequest())
}

const goodObligations = `"obligations": {
    "minimum_assurance": "aal2",
    "redaction": [
      "personal_identifiers"
    ],
    "repository_allowlist": [
      "full-chaos/dev-health-ops"
    ],
    "step_up": false,
    "tenant_scope": "org_EXAMPLE0000000000000001"
  }`

func TestObligationAccessors(t *testing.T) {
	d := decideAllow(t, goodObligations)
	if !d.Allowed() {
		t.Fatalf("not allowed: %v", d.Err())
	}
	o := d.Obligations()
	if scope, ok := o.TenantScope(); !ok || scope != testOrg {
		t.Fatalf("tenant scope %q %v", scope, ok)
	}
	if !o.TenantAllows(testOrg) || o.TenantAllows("org_EXAMPLE0000000000000002") || o.TenantAllows("") {
		t.Fatal("tenant scope check")
	}
	if !o.RepositoryAllowed("full-chaos/dev-health-ops") || o.RepositoryAllowed("full-chaos/dev-health-web") || o.RepositoryAllowed("") || o.RepositoryAllowed("*") {
		t.Fatal("allowlist check")
	}
	if !o.RepositoryRestricted() {
		t.Fatal("allowlist not restricted")
	}
	if !o.Redacts(RedactPersonalIdentifiers) || o.Redacts(RedactContactDetails) || o.Redacts("") {
		t.Fatal("redaction check")
	}
	if a, ok := o.MinimumAssurance(); !ok || a != AssuranceAAL2 {
		t.Fatalf("assurance %q %v", a, ok)
	}
	if o.AssuranceSatisfiedBy(AssuranceAAL1) || !o.AssuranceSatisfiedBy(AssuranceAAL2) || !o.AssuranceSatisfiedBy(AssuranceAAL3) || o.AssuranceSatisfiedBy("") || o.AssuranceSatisfiedBy("aal9") {
		t.Fatal("assurance order")
	}
	if o.StepUpRequired() {
		t.Fatal("step up")
	}
}

func TestObligationAbsentMembersAreUnrestrictedButTheZeroValueIsStrict(t *testing.T) {
	d := decideAllow(t, `"obligations": {}`)
	if !d.Allowed() {
		t.Fatalf("not allowed: %v", d.Err())
	}
	o := d.Obligations()
	if _, ok := o.TenantScope(); ok || o.RepositoryRestricted() || !o.RepositoryAllowed("a/b") || o.Redacts(RedactFreeText) || o.StepUpRequired() {
		t.Fatal("an empty obligation set must be unrestricted")
	}
	if _, ok := o.MinimumAssurance(); ok || !o.AssuranceSatisfiedBy(AssuranceAAL1) {
		t.Fatal("no minimum assurance")
	}
	// The zero value is what a deny and a local deny carry: it never grants.
	var zero Obligations
	if zero.TenantAllows(testOrg) || zero.RepositoryAllowed("a/b") {
		t.Fatal("the zero Obligations grants access")
	}
}

func TestObligationEmptyAllowlistMeansNone(t *testing.T) {
	d := decideAllow(t, `"obligations": {"repository_allowlist": []}`)
	o := d.Obligations()
	if !o.RepositoryRestricted() || o.RepositoryAllowed("a/b") || o.RepositoryAllowed("*") {
		t.Fatal("an empty allowlist must allow nothing")
	}
}

func TestObligationStarAllowsEveryRepository(t *testing.T) {
	o := decideAllow(t, `"obligations": {"repository_allowlist": ["*"]}`).Obligations()
	if !o.RepositoryAllowed("a/b") || !o.RepositoryAllowed("x/y") || o.RepositoryAllowed("") {
		t.Fatal("star allowlist")
	}
}

func TestObligationStepUp(t *testing.T) {
	if !decideAllow(t, `"obligations": {"step_up": true}`).Obligations().StepUpRequired() {
		t.Fatal("step_up true was lost")
	}
}

func TestObligationReturnsCopies(t *testing.T) {
	o := decideAllow(t, goodObligations).Obligations()
	list := o.RepositoryAllowlist()
	list[0] = "evil/repo"
	if o.RepositoryAllowed("evil/repo") || !o.RepositoryAllowed("full-chaos/dev-health-ops") {
		t.Fatal("the allowlist is shared with the caller")
	}
	classes := o.Redaction()
	classes[0] = RedactFreeText
	if !o.Redacts(RedactPersonalIdentifiers) || o.Redacts(RedactFreeText) {
		t.Fatal("the redaction list is shared with the caller")
	}
}

func TestBadObligationsDenyTheWholeDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		obligations string
		want        Reason
	}{
		"unknown member":         {`"obligations": {"export_ok": true}`, ReasonUnknownObligation},
		"unknown member by case": {`"obligations": {"Step_Up": true}`, ReasonUnknownObligation},
		"unknown redaction":      {`"obligations": {"redaction": ["biometrics"]}`, ReasonUnknownObligation},
		"unknown assurance":      {`"obligations": {"minimum_assurance": "aal9"}`, ReasonUnknownObligation},
		"null obligations":       {`"obligations": null`, ReasonMalformed},
		"array obligations":      {`"obligations": []`, ReasonMalformed},
		"step up as string":      {`"obligations": {"step_up": "false"}`, ReasonMalformed},
		"step up null":           {`"obligations": {"step_up": null}`, ReasonMalformed},
		"tenant scope empty":     {`"obligations": {"tenant_scope": ""}`, ReasonMalformed},
		"tenant scope number":    {`"obligations": {"tenant_scope": 5}`, ReasonMalformed},
		"allowlist not a list":   {`"obligations": {"repository_allowlist": "a/b"}`, ReasonMalformed},
		"allowlist null":         {`"obligations": {"repository_allowlist": null}`, ReasonMalformed},
		"allowlist uppercase":    {`"obligations": {"repository_allowlist": ["A/b"]}`, ReasonMalformed},
		"allowlist duplicate":    {`"obligations": {"repository_allowlist": ["a/b","a/b"]}`, ReasonMalformed},
		"allowlist number":       {`"obligations": {"repository_allowlist": [1]}`, ReasonMalformed},
		"redaction duplicate":    {`"obligations": {"redaction": ["free_text","free_text"]}`, ReasonMalformed},
		"redaction not a list":   {`"obligations": {"redaction": "free_text"}`, ReasonMalformed},
		"assurance number":       {`"obligations": {"minimum_assurance": 2}`, ReasonMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			d := decideAllow(t, tc.obligations)
			if d.Allowed() || !d.Local() || d.Reason() != tc.want {
				t.Fatalf("allowed=%v reason=%s, want local %s", d.Allowed(), d.Reason(), tc.want)
			}
			if o := d.Obligations(); o.TenantAllows(testOrg) || o.RepositoryAllowed("a/b") {
				t.Fatal("a refused decision carries a grant")
			}
		})
	}
}

func TestDenyCarriesNoObligations(t *testing.T) {
	body := readFile(t, "authorization_decision", "decision-valid-deny.json")
	pdp := newPDP(t, replyJSON(body))
	d := pdp.client(t).Decide(context.Background(), baseRequest())
	if d.Allowed() || d.Local() || d.Reason() != ReasonGrantAbsent {
		t.Fatalf("allowed=%v local=%v reason=%s", d.Allowed(), d.Local(), d.Reason())
	}
	if o := d.Obligations(); o.TenantAllows(testOrg) || o.RepositoryAllowed("a/b") {
		t.Fatal("a deny carries a grant")
	}
}

// Parity with ops on main: authorize_request.go:142-147 (organization_id is any
// non-empty string) and principal.v1 organization_id (type string, minLength 1,
// no pattern), which authorization_decision.v1 organization_id and
// obligations.tenant_scope reference.
func TestOrganizationStringsMatchOpsAnyNonEmptyString(t *testing.T) {
	for name, org := range map[string]string{
		"unicode": "org_é世界",
		"space":   "org with space",
		"over256": "org_" + string(bytes.Repeat([]byte("a"), 300)),
		"control": "org_\ttab",
	} {
		t.Run(name, func(t *testing.T) {
			body := replaceOnce(t, goodAllow(t), `"organization_id": "org_EXAMPLE0000000000000001"`, `"organization_id": `+strconv.Quote(org))
			body = replaceOnce(t, body, `"tenant_scope": "org_EXAMPLE0000000000000001"`, `"tenant_scope": `+strconv.Quote(org))
			pdp := newPDP(t, replyJSON(body))
			req := baseRequest()
			req.OrganizationID = org
			d := pdp.client(t).Decide(context.Background(), req)
			if !d.Allowed() || d.Local() || pdp.calls.Load() != 1 {
				t.Fatalf("allowed=%v reason=%s calls=%d", d.Allowed(), d.Reason(), pdp.calls.Load())
			}
			if scope, ok := d.Obligations().TenantScope(); !ok || scope != org || !d.Obligations().TenantAllows(org) {
				t.Fatalf("tenant scope %q %v", scope, ok)
			}
		})
	}
	for name, body := range map[string][]byte{
		"empty decision organization": replaceOnce(t, goodAllow(t), `"organization_id": "org_EXAMPLE0000000000000001"`, `"organization_id": ""`),
		"empty tenant scope":          replaceOnce(t, goodAllow(t), `"tenant_scope": "org_EXAMPLE0000000000000001"`, `"tenant_scope": ""`),
	} {
		d := decideBody(t, body, baseRequest())
		if d.Allowed() || !d.Local() {
			t.Errorf("%s: allowed=%v", name, d.Allowed())
		}
	}
}
