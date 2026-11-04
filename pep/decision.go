package pep

import (
	"context"

	"github.com/full-chaos/dev-health-go/pep/internal/decision"
)

// Decision is the PDP's answer, or a deny the client made itself. Its zero
// value denies, it has no exported field, and the only way to get an allowed
// one is a Client that parsed a valid allow document. Callers must enforce
// Obligations on an allow; there is no allow without them.
type Decision = decision.Decision

// Obligations are the enforceable obligations of an allow. The zero value
// grants nothing.
type Obligations = decision.Obligations

// Reason is a decision reason: a wire reason_code (WireReasons) or a closed
// local reason (LocalReasons) the client gives a deny it made itself. A local
// reason starts with "pdp_" and is never a PDP verdict.
type Reason = decision.Reason

// RedactionClass is a field class a service must remove from its response.
type RedactionClass = decision.RedactionClass

// Assurance is a NIST SP 800-63B authenticator assurance level.
type Assurance = decision.Assurance

// Decider is what a policy enforcement point calls. *Client and pepfake.Fake
// implement it. A failure is a deny; there is no error return to forget.
type Decider interface {
	Decide(ctx context.Context, req Request) Decision
	DecideBatch(ctx context.Context, reqs []Request) []Decision
}

const (
	RedactPersonalIdentifiers RedactionClass = "personal_identifiers"
	RedactContactDetails      RedactionClass = "contact_details"
	RedactFreeText            RedactionClass = "free_text"

	AssuranceAAL1 Assurance = "aal1"
	AssuranceAAL2 Assurance = "aal2"
	AssuranceAAL3 Assurance = "aal3"
)

// Wire reasons: the closed authorization_decision.v1 reason_code set.
const (
	ReasonAllowedByMembershipDefault  = decision.AllowedByMembershipDefault
	ReasonAllowedByRepositoryGrant    = decision.AllowedByRepositoryGrant
	ReasonAllowedByRole               = decision.AllowedByRole
	ReasonDelegationInvalid           = decision.DelegationInvalid
	ReasonGrantAbsent                 = decision.GrantAbsent
	ReasonGrantExpired                = decision.GrantExpired
	ReasonGrantRevoked                = decision.GrantRevoked
	ReasonImpersonationDenied         = decision.ImpersonationDenied
	ReasonWireInvalidRequest          = decision.InvalidRequest
	ReasonMembershipInactive          = decision.MembershipInactive
	ReasonNoMembership                = decision.NoMembership
	ReasonOrgMismatch                 = decision.OrgMismatch
	ReasonOrganizationInactive        = decision.OrganizationInactive
	ReasonPlatformRoleAbsent          = decision.PlatformRoleAbsent
	ReasonPrincipalInactive           = decision.PrincipalInactive
	ReasonRepositoryAllowlistTooLarge = decision.RepositoryAllowlistTooLarge
	ReasonRepositoryNotGranted        = decision.RepositoryNotGranted
	ReasonResourceMismatch            = decision.ResourceMismatch
	ReasonRoleLacksAction             = decision.RoleLacksAction
	ReasonRoleScopeMismatch           = decision.RoleScopeMismatch
	ReasonRoleUnknown                 = decision.RoleUnknown
	ReasonServiceAccountInactive      = decision.ServiceAccountInactive
	ReasonUnknownAction               = decision.UnknownAction
	ReasonWorkloadActionDenied        = decision.WorkloadActionDenied
	ReasonWorkloadAudienceDenied      = decision.WorkloadAudienceDenied
)

// Local reasons: why the client denied without a PDP verdict.
const (
	ReasonUnreachable       = decision.Unreachable
	ReasonTimeout           = decision.Timeout
	ReasonCanceled          = decision.Canceled
	ReasonUnavailable       = decision.Unavailable
	ReasonRateLimited       = decision.RateLimited
	ReasonRefused           = decision.Refused
	ReasonMalformed         = decision.Malformed
	ReasonTooLarge          = decision.TooLarge
	ReasonMismatch          = decision.Mismatch
	ReasonUnknownObligation = decision.UnknownObligation
	ReasonInvalidRequest    = decision.LocalInvalid
	ReasonNotConfigured     = decision.NotConfigured
	ReasonNoDecision        = decision.NoDecision
)

var wireReasons = []Reason{
	ReasonAllowedByMembershipDefault, ReasonAllowedByRepositoryGrant, ReasonAllowedByRole,
	ReasonDelegationInvalid, ReasonGrantAbsent, ReasonGrantExpired, ReasonGrantRevoked, ReasonImpersonationDenied,
	ReasonWireInvalidRequest, ReasonMembershipInactive, ReasonNoMembership, ReasonOrgMismatch, ReasonOrganizationInactive,
	ReasonPlatformRoleAbsent, ReasonPrincipalInactive, ReasonRepositoryAllowlistTooLarge, ReasonRepositoryNotGranted, ReasonResourceMismatch,
	ReasonRoleLacksAction, ReasonRoleScopeMismatch, ReasonRoleUnknown, ReasonServiceAccountInactive,
	ReasonUnknownAction, ReasonWorkloadActionDenied, ReasonWorkloadAudienceDenied,
}

var localReasons = []Reason{
	ReasonUnreachable, ReasonTimeout, ReasonCanceled, ReasonUnavailable, ReasonRateLimited, ReasonRefused,
	ReasonMalformed, ReasonTooLarge, ReasonMismatch, ReasonUnknownObligation,
	ReasonInvalidRequest, ReasonNotConfigured, ReasonNoDecision,
}

var allowReasons = map[Reason]bool{
	ReasonAllowedByMembershipDefault: true,
	ReasonAllowedByRepositoryGrant:   true,
	ReasonAllowedByRole:              true,
}

// WireReasons is a copy of the closed set of PDP reason codes. A reason
// outside it makes the whole decision a local deny.
func WireReasons() []Reason { return append([]Reason(nil), wireReasons...) }

// LocalReasons is a copy of the closed set of local deny reasons.
func LocalReasons() []Reason { return append([]Reason(nil), localReasons...) }

func wireReason(s string) (Reason, bool) {
	r := Reason(s)
	for _, w := range wireReasons {
		if w == r {
			return r, true
		}
	}
	return "", false
}
