// Package pepfake is a test double for a pep.Decider that cannot be built
// allow-by-default.
//
// A Fake allows exactly the requests its rules name and denies everything
// else. Its zero value, a nil *Fake and New() with no rules deny. There is no
// allow-all constructor, no default-allow option and no exported field to set:
// a rule must name a principal reference, an action, a resource type and a
// resource key, each exactly, and a rule missing one never matches, so an empty
// field is not a wildcard. The package's exported API is pinned by a census
// test, so adding a way to allow everything fails a test and needs review.
package pepfake

import (
	"context"

	"github.com/full-chaos/dev-health-go/pep"
	"github.com/full-chaos/dev-health-go/pep/internal/decision"
)

// Rule allows one exact request. RequestID is not part of a rule.
type Rule struct {
	PrincipalReference string
	Action             string
	ResourceType       string
	ResourceKey        string
	// OrganizationID is the request's organization; empty means a request that
	// names none, not any organization.
	OrganizationID string

	// The obligations the allow carries. A nil RepositoryAllowlist is the
	// absent member; a non-nil empty one allows no repository.
	TenantScope         string
	RepositoryAllowlist []string
	Redaction           []pep.RedactionClass
	MinimumAssurance    pep.Assurance
	StepUp              bool
}

// Fake decides from its rules. Build one with New.
type Fake struct {
	rules []Rule
}

var _ pep.Decider = (*Fake)(nil)

// New copies the rules. With none it denies every request.
func New(rules ...Rule) *Fake {
	return &Fake{rules: append([]Rule(nil), rules...)}
}

func (r Rule) complete() bool {
	return r.PrincipalReference != "" && r.Action != "" && r.ResourceType != "" && r.ResourceKey != ""
}

func (r Rule) matches(q pep.Request) bool {
	return r.complete() && r.PrincipalReference == q.PrincipalReference && r.Action == q.Action && r.ResourceType == q.ResourceType && r.ResourceKey == q.ResourceKey && r.OrganizationID == q.OrganizationID
}

func (r Rule) allow(q pep.Request) pep.Decision {
	if r.MinimumAssurance != "" && !validAssurance(r.MinimumAssurance) {
		return decision.Local(decision.Malformed, errBadRule)
	}
	return decision.FromWire(decision.Wire{
		Allowed:      true,
		Reason:       decision.AllowedByRole,
		DecisionID:   "dec_FAKE00000000000000000000000",
		PrincipalID:  "prn_FAKE",
		Organization: q.OrganizationID,
		HasOrg:       q.OrganizationID != "",
		Obligations: decision.NewObligations(decision.ObligationSpec{
			Tenant:    r.TenantScope,
			Allowlist: r.RepositoryAllowlist,
			Redaction: r.Redaction,
			Assurance: r.MinimumAssurance,
			StepUp:    r.StepUp,
		}),
	})
}

func validAssurance(a pep.Assurance) bool {
	return a == pep.AssuranceAAL1 || a == pep.AssuranceAAL2 || a == pep.AssuranceAAL3
}

type badRule struct{}

func (badRule) Error() string { return "pepfake: the rule names an unknown assurance level" }

var errBadRule error = badRule{}

// Decide allows a request a rule names and denies every other.
func (f *Fake) Decide(_ context.Context, req pep.Request) pep.Decision {
	if f == nil {
		return pep.Decision{}
	}
	for _, r := range f.rules {
		if r.matches(req) {
			return r.allow(req)
		}
	}
	return pep.Decision{}
}

// DecideBatch decides each request apart.
func (f *Fake) DecideBatch(ctx context.Context, reqs []pep.Request) []pep.Decision {
	out := make([]pep.Decision, len(reqs))
	for i, r := range reqs {
		out[i] = f.Decide(ctx, r)
	}
	return out
}
