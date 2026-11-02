// Package decision holds the PEP decision value. It lives under internal/ so
// that only pep and pep/pepfake can build an allowed Decision: every other
// importer gets the zero value, which denies, or a Decision from a Client.
package decision

import "time"

// Reason is a decision reason: a wire reason_code of authorization_decision.v1
// or a closed local reason the client gives a deny it made itself.
type Reason string

const (
	AllowedByMembershipDefault Reason = "allowed_by_membership_default"
	AllowedByRepositoryGrant   Reason = "allowed_by_repository_grant"
	AllowedByRole              Reason = "allowed_by_role"

	DelegationInvalid      Reason = "delegation_invalid"
	GrantAbsent            Reason = "grant_absent"
	GrantExpired           Reason = "grant_expired"
	GrantRevoked           Reason = "grant_revoked"
	ImpersonationDenied    Reason = "impersonation_denied"
	InvalidRequest         Reason = "invalid_request"
	MembershipInactive     Reason = "membership_inactive"
	NoMembership           Reason = "no_membership"
	OrgMismatch            Reason = "org_mismatch"
	OrganizationInactive   Reason = "organization_inactive"
	PlatformRoleAbsent     Reason = "platform_role_absent"
	PrincipalInactive      Reason = "principal_inactive"
	RepositoryNotGranted   Reason = "repository_not_granted"
	ResourceMismatch       Reason = "resource_mismatch"
	RoleLacksAction        Reason = "role_lacks_action"
	RoleScopeMismatch      Reason = "role_scope_mismatch"
	RoleUnknown            Reason = "role_unknown"
	ServiceAccountInactive Reason = "service_account_inactive"
	UnknownAction          Reason = "unknown_action"
	WorkloadActionDenied   Reason = "workload_action_denied"
	WorkloadAudienceDenied Reason = "workload_audience_denied"

	Unreachable       Reason = "pdp_unreachable"
	Timeout           Reason = "pdp_timeout"
	Canceled          Reason = "pdp_canceled"
	Unavailable       Reason = "pdp_unavailable"
	RateLimited       Reason = "pdp_rate_limited"
	Refused           Reason = "pdp_refused"
	Malformed         Reason = "pdp_malformed"
	TooLarge          Reason = "pdp_too_large"
	Mismatch          Reason = "pdp_mismatch"
	UnknownObligation Reason = "pdp_unknown_obligation"
	LocalInvalid      Reason = "pdp_invalid_request"
	NotConfigured     Reason = "pdp_not_configured"
	NoDecision        Reason = "pdp_no_decision"
)

// Local reports a reason the client made itself: never a PDP verdict.
func (r Reason) Local() bool { return len(r) > 4 && r[:4] == "pdp_" }

type (
	// RedactionClass is a field class a service must remove from its response.
	RedactionClass string
	// Assurance is an authenticator assurance level.
	Assurance string
)

// Obligations are the enforceable obligations of an allow. Each member is
// optional on the wire; the zero value (what a deny carries) grants nothing.
type Obligations struct {
	granted      bool
	tenant       string
	hasTenant    bool
	allowlist    []string
	hasAllowlist bool
	redaction    []RedactionClass
	assurance    Assurance
	hasAssurance bool
	stepUp       bool
}

// ObligationSpec builds Obligations. A nil Allowlist is the absent member
// (unrestricted); a non-nil empty one means no repository at all.
type ObligationSpec struct {
	Tenant    string
	Allowlist []string
	Redaction []RedactionClass
	Assurance Assurance
	StepUp    bool
}

// NewObligations is the obligation set of an allow.
func NewObligations(s ObligationSpec) Obligations {
	o := Obligations{granted: true, tenant: s.Tenant, hasTenant: s.Tenant != "", redaction: append([]RedactionClass(nil), s.Redaction...), assurance: s.Assurance, hasAssurance: s.Assurance != "", stepUp: s.StepUp}
	if s.Allowlist != nil {
		o.hasAllowlist = true
		o.allowlist = append([]string{}, s.Allowlist...)
	}
	return o
}

func assuranceRank(a Assurance) int {
	switch a {
	case "aal1":
		return 1
	case "aal2":
		return 2
	case "aal3":
		return 3
	}
	return 0
}

// TenantScope is the organization every read and write must stay within.
func (o Obligations) TenantScope() (string, bool) { return o.tenant, o.hasTenant }

// TenantAllows reports whether org is inside the tenant scope. The zero value
// and an empty org never pass; with no tenant_scope member an allow leaves the
// organization to the request.
func (o Obligations) TenantAllows(org string) bool {
	if !o.granted || org == "" {
		return false
	}
	return !o.hasTenant || o.tenant == org
}

// RepositoryRestricted reports an allowlist member (an empty one included).
func (o Obligations) RepositoryRestricted() bool { return o.hasAllowlist }

// RepositoryAllowlist is a copy of the allowlist; empty when none.
func (o Obligations) RepositoryAllowlist() []string { return append([]string(nil), o.allowlist...) }

// RepositoryAllowed reports whether the service may touch the repository key.
// An empty allowlist allows none; "*" allows every key.
func (o Obligations) RepositoryAllowed(key string) bool {
	if !o.granted || key == "" {
		return false
	}
	if !o.hasAllowlist {
		return true
	}
	for _, k := range o.allowlist {
		if k == "*" || k == key {
			return true
		}
	}
	return false
}

// Redaction is a copy of the field classes the service must remove.
func (o Obligations) Redaction() []RedactionClass {
	return append([]RedactionClass(nil), o.redaction...)
}

// Redacts reports whether the class must be removed.
func (o Obligations) Redacts(c RedactionClass) bool {
	for _, r := range o.redaction {
		if r == c {
			return true
		}
	}
	return false
}

// MinimumAssurance is the lowest assurance level the principal must hold.
func (o Obligations) MinimumAssurance() (Assurance, bool) { return o.assurance, o.hasAssurance }

// AssuranceSatisfiedBy reports whether a principal holding level meets the
// minimum. The zero value, an unknown level and an empty level never pass.
func (o Obligations) AssuranceSatisfiedBy(level Assurance) bool {
	have := assuranceRank(level)
	if !o.granted || have == 0 {
		return false
	}
	return !o.hasAssurance || have >= assuranceRank(o.assurance)
}

// StepUpRequired reports that the caller must complete step-up first.
func (o Obligations) StepUpRequired() bool { return o.stepUp }

// Decision is a PDP decision or a local deny. The zero value denies.
type Decision struct {
	allowed        bool
	reason         Reason
	err            error
	decisionID     string
	principalID    string
	organization   string
	hasOrg         bool
	policyRevision uint64
	expiresAt      time.Time
	cacheable      bool
	obligations    Obligations
}

// Wire is the content of a decision document that passed every check.
type Wire struct {
	Allowed        bool
	Reason         Reason
	DecisionID     string
	PrincipalID    string
	Organization   string
	HasOrg         bool
	PolicyRevision uint64
	ExpiresAt      time.Time
	// Cacheable is true when ExpiresAt is after the time the decision was read.
	Cacheable   bool
	Obligations Obligations
}

// FromWire builds the decision of a checked document. An allow needs an allow
// reason and the obligations of an allow; a deny drops obligations. Anything
// else is a local deny.
func FromWire(w Wire) Decision {
	if w.Allowed != allowReason(w.Reason) {
		return Local(Malformed, errInconsistent)
	}
	d := Decision{allowed: w.Allowed, reason: w.Reason, decisionID: w.DecisionID, principalID: w.PrincipalID, organization: w.Organization, hasOrg: w.HasOrg, policyRevision: w.PolicyRevision, expiresAt: w.ExpiresAt, cacheable: w.Cacheable}
	if w.Allowed {
		d.obligations = w.Obligations
	}
	return d
}

func allowReason(r Reason) bool {
	return r == AllowedByMembershipDefault || r == AllowedByRepositoryGrant || r == AllowedByRole
}

// Local is a deny the client made itself.
func Local(r Reason, err error) Decision { return Decision{reason: r, err: err} }

// Allowed reports an allow. Only a checked allow document makes one.
func (d Decision) Allowed() bool { return d.allowed }

// Reason is the PDP reason_code, or the closed local reason of a local deny.
// The zero Decision reports pdp_no_decision.
func (d Decision) Reason() Reason {
	if d.reason == "" {
		return NoDecision
	}
	return d.reason
}

// Local reports a deny the client made itself, not a PDP verdict.
func (d Decision) Local() bool { return d.Reason().Local() }

// Err is the typed cause of a local deny; nil for a PDP verdict.
func (d Decision) Err() error {
	if d.reason == "" {
		return errNoDecision
	}
	return d.err
}

// DecisionID is the PDP's decision id; empty for a local deny.
func (d Decision) DecisionID() string { return d.decisionID }

// PrincipalID is the principal the decision is about; empty for a local deny.
func (d Decision) PrincipalID() string { return d.principalID }

// OrganizationID is the decision's organization; false for a platform decision
// and for a local deny.
func (d Decision) OrganizationID() (string, bool) { return d.organization, d.hasOrg }

// PolicyRevision is the policy revision the decision was made under.
func (d Decision) PolicyRevision() uint64 { return d.policyRevision }

// Cacheable reports that the decision may be reused until ExpiresAt. A decision
// is always valid for the request it answers; expires_at bounds reuse only, so
// one whose expiry is not after the time it was read is for this call alone and
// a cache must skip it. False for a local deny.
func (d Decision) Cacheable() bool { return d.cacheable }

// ExpiresAt is when the decision stops being usable; zero for a local deny.
func (d Decision) ExpiresAt() time.Time { return d.expiresAt }

// Obligations are the obligations of an allow; the zero value otherwise.
func (d Decision) Obligations() Obligations { return d.obligations }

type noDecisionError struct{}

func (noDecisionError) Error() string { return "pep: no decision was made" }

var errNoDecision error = noDecisionError{}

type inconsistentError struct{}

func (inconsistentError) Error() string { return "pep: the verdict and the reason disagree" }

var errInconsistent error = inconsistentError{}
