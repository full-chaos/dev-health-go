package pep

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type repositoryScoped interface{ RepositoryScopeOrganization() bool }

func scopeOrganization(o Obligations) bool {
	s, ok := any(o).(repositoryScoped)
	return ok && s.RepositoryScopeOrganization()
}

const scopeOrganizationObligations = `"obligations": {"repository_scope": "organization", "tenant_scope": "org_EXAMPLE0000000000000001"}`

func TestRepositoryScopeOrganizationAllowsEveryRepository(t *testing.T) {
	d := decideAllow(t, scopeOrganizationObligations)
	if !d.Allowed() || d.Local() {
		t.Fatalf("allowed=%v reason=%s", d.Allowed(), d.Reason())
	}
	o := d.Obligations()
	if !scopeOrganization(o) {
		t.Fatal("repository_scope organization was not kept")
	}
	if !o.RepositoryAllowed("a/b") || !o.RepositoryAllowed("full-chaos/dev-health-web") || !o.RepositoryAllowed("group/sub/repo") {
		t.Fatal("an organization-wide scope did not allow a repository")
	}
	if !o.TenantAllows(testOrg) || o.TenantAllows("org_EXAMPLE0000000000000002") {
		t.Fatal("tenant scope")
	}
	if o.RepositoryRestricted() {
		t.Fatal("an organization-wide scope reads as an allowlist")
	}
}

func TestRepositoryAllowlistIsHonored(t *testing.T) {
	o := decideAllow(t, `"obligations": {"repository_allowlist": ["full-chaos/dev-health-ops", "group/sub/repo"], "tenant_scope": "org_EXAMPLE0000000000000001"}`).Obligations()
	if scopeOrganization(o) || !o.RepositoryRestricted() {
		t.Fatal("an allowlist reads as organization-wide")
	}
	if !o.RepositoryAllowed("full-chaos/dev-health-ops") || !o.RepositoryAllowed("group/sub/repo") || o.RepositoryAllowed("full-chaos/dev-health-web") || o.RepositoryAllowed("group/sub") {
		t.Fatal("the allowlist was not honored")
	}
}

func TestMissingRepositoryMemberAllowsNoRepository(t *testing.T) {
	for name, obligations := range map[string]string{
		"empty obligations": `"obligations": {}`,
		"tenant only":       `"obligations": {"tenant_scope": "org_EXAMPLE0000000000000001"}`,
		"assurance only":    `"obligations": {"minimum_assurance": "aal2"}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := decideAllow(t, obligations)
			if !d.Allowed() {
				t.Fatalf("not allowed: %v", d.Err())
			}
			o := d.Obligations()
			if scopeOrganization(o) || o.RepositoryAllowed("a/b") || o.RepositoryAllowed("full-chaos/dev-health-ops") {
				t.Fatal("an allow with neither repository_scope nor repository_allowlist granted a repository")
			}
		})
	}
	t.Run("no obligations member", func(t *testing.T) {
		raw := readFile(t, "authorization_decision", "decision-valid-allow-without-obligations.json")
		if strings.Contains(string(raw), `"obligations"`) {
			t.Fatal("fixture carries obligations")
		}
		d := decideBody(t, raw, baseRequest())
		if !d.Allowed() {
			t.Fatalf("not allowed: %v", d.Err())
		}
		if d.Obligations().RepositoryAllowed("a/b") {
			t.Fatal("an allow with no obligations granted a repository")
		}
	})
}

func TestStarIsNeverARepository(t *testing.T) {
	for name, obligations := range map[string]string{
		"organization scope": scopeOrganizationObligations,
		"star allowlist":     `"obligations": {"repository_allowlist": ["*"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := decideAllow(t, obligations)
			if !d.Allowed() {
				t.Fatalf("not allowed: %v", d.Err())
			}
			o := d.Obligations()
			if !o.RepositoryAllowed("a/b") {
				t.Fatal("an organization-wide grant did not allow a/b")
			}
			for _, key := range []string{"*", "", "A/B", "a/", "a", "a/*"} {
				if o.RepositoryAllowed(key) {
					t.Errorf("%q was accepted as a repository", key)
				}
			}
		})
	}
}

func TestBadRepositoryScopeDeniesTheWholeDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		obligations string
		want        Reason
	}{
		"both members":      {`"obligations": {"repository_scope": "organization", "repository_allowlist": ["a/b"]}`, ReasonMalformed},
		"both, empty list":  {`"obligations": {"repository_scope": "organization", "repository_allowlist": []}`, ReasonMalformed},
		"unknown value":     {`"obligations": {"repository_scope": "everything"}`, ReasonUnknownObligation},
		"wrong case":        {`"obligations": {"repository_scope": "Organization"}`, ReasonUnknownObligation},
		"empty string":      {`"obligations": {"repository_scope": ""}`, ReasonUnknownObligation},
		"number":            {`"obligations": {"repository_scope": 1}`, ReasonMalformed},
		"null":              {`"obligations": {"repository_scope": null}`, ReasonMalformed},
		"list":              {`"obligations": {"repository_scope": ["organization"]}`, ReasonMalformed},
		"true":              {`"obligations": {"repository_scope": true}`, ReasonMalformed},
		"257-key allowlist": {`"obligations": {"repository_allowlist": [` + allowlistKeys(257) + `]}`, ReasonMalformed},
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

func TestADenyWithRepositoryScopeIsMalformed(t *testing.T) {
	body := readFile(t, "authorization_decision", "decision-valid-deny.json")
	body = replaceOnce(t, body, `"allowed": false,`, `"allowed": false, "obligations": {"repository_scope": "organization"},`)
	d := decideBody(t, body, baseRequest())
	if d.Allowed() || !d.Local() || d.Reason() != ReasonMalformed {
		t.Fatalf("allowed=%v reason=%s, want local %s", d.Allowed(), d.Reason(), ReasonMalformed)
	}
}

func allowlistKeys(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf(`"owner/repo-%03d"`, i)
	}
	return strings.Join(keys, ",")
}

// The bound comes from the contract: maxItems keys decode, one more is refused.
func TestRepositoryAllowlistBoundEqualsTheContract(t *testing.T) {
	node := schemaProp(t, "authorization_decision.v1.schema.json", "properties", "obligations", "properties", "repository_allowlist")
	max, ok := node["maxItems"].(float64)
	if !ok || max < 1 {
		t.Fatalf("the decision schema repository_allowlist has no maxItems: %v", node["maxItems"])
	}
	n := int(max)
	at := decideAllow(t, `"obligations": {"repository_allowlist": [`+allowlistKeys(n)+`]}`)
	if !at.Allowed() || !at.Obligations().RepositoryAllowed(fmt.Sprintf("owner/repo-%03d", n-1)) {
		t.Fatalf("%d keys (the bound) refused: %v", n, at.Err())
	}
	over := decideAllow(t, `"obligations": {"repository_allowlist": [`+allowlistKeys(n+1)+`]}`)
	if over.Allowed() || over.Reason() != ReasonMalformed {
		t.Fatalf("%d keys (over the bound): allowed=%v reason=%s", n+1, over.Allowed(), over.Reason())
	}
}

func TestRepositoryAllowlistTooLargeIsAWireDeny(t *testing.T) {
	const tooLarge = Reason("repository_allowlist_too_large")
	found := false
	for _, r := range WireReasons() {
		found = found || r == tooLarge
	}
	if !found {
		t.Error("WireReasons lacks repository_allowlist_too_large")
	}
	body := readFile(t, "authorization_decision", "decision-valid-deny.json")
	body = replaceOnce(t, body, `"reason_code": "grant_absent"`, `"reason_code": "repository_allowlist_too_large"`)
	d := decideBody(t, body, baseRequest())
	if d.Allowed() || d.Local() || d.Err() != nil || d.Reason() != tooLarge {
		t.Fatalf("allowed=%v local=%v reason=%s, want a wire deny %s", d.Allowed(), d.Local(), d.Reason(), tooLarge)
	}
}

func TestBatchItemsKeepTheirOwnRepositoryScope(t *testing.T) {
	item := func(i int, obligations string) string {
		return fmt.Sprintf(`{"index": %d, "decision": {"schema_version": "authorization_decision.v1", "decision_id": "dec_EXAMPLE000000000000000000%d", "allowed": true, "reason_code": "allowed_by_role", "principal_id": "prn_EXAMPLE0000000000000001", "organization_id": %q, "policy_revision": 42, "expires_at": "2026-09-03T05:00:30Z", "obligations": %s}}`, i, i, testOrg, obligations)
	}
	body := `{"schema_version": "authorization_batch_response.v1", "decisions": [` +
		item(0, `{"repository_scope": "organization", "tenant_scope": "`+testOrg+`"}`) + `,` +
		item(1, `{"repository_allowlist": ["full-chaos/dev-health-ops"], "tenant_scope": "`+testOrg+`"}`) + `]}`
	a, b := baseRequest(), baseRequest()
	b.ResourceKey = "full-chaos/dev-health-web"
	pdp := newPDP(t, replyJSON([]byte(body)))
	got := pdp.client(t).DecideBatch(context.Background(), []Request{a, b})
	if len(got) != 2 || !got[0].Allowed() || !got[1].Allowed() {
		t.Fatalf("both items must allow: %+v", got)
	}
	first, second := got[0].Obligations(), got[1].Obligations()
	if !scopeOrganization(first) || first.RepositoryRestricted() || !first.RepositoryAllowed("x/y") {
		t.Fatal("item 0 lost its organization scope")
	}
	if scopeOrganization(second) || !second.RepositoryRestricted() || second.RepositoryAllowed("x/y") || !second.RepositoryAllowed("full-chaos/dev-health-ops") {
		t.Fatal("item 1 took item 0's scope")
	}
}

func TestCacheHitKeepsRepositoryScope(t *testing.T) {
	clock := newClock()
	p := newCachePDP(t, clock)
	p.set(func(wireRequest) verdict {
		return verdict{allow: true, rev: 5, obligations: `{"tenant_scope":"` + testOrg + `","repository_scope":"organization"}`}
	})
	c := p.cached(t, CacheConfig{})
	first := c.Decide(context.Background(), baseRequest())
	hit := c.Decide(context.Background(), baseRequest())
	wantCalls(t, p, 1)
	for name, d := range map[string]Decision{"miss": first, "hit": hit} {
		o := d.Obligations()
		if !d.Allowed() || !scopeOrganization(o) || !o.RepositoryAllowed("x/y") || o.RepositoryAllowed("*") {
			t.Fatalf("%s: allowed=%v reason=%s scope lost", name, d.Allowed(), d.Reason())
		}
	}
}
