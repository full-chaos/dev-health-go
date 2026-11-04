package pep

import (
	"bytes"
	"encoding/json"
	"regexp"
	"time"

	"github.com/full-chaos/dev-health-go/pep/internal/decision"
	"github.com/full-chaos/dev-health-go/repokey"
)

const (
	decisionSchema = "authorization_decision.v1"
	batchSchema    = "authorization_batch_response.v1"

	// maxRepositoryAllowlist is the contract's repository_allowlist maxItems.
	maxRepositoryAllowlist = 256
)

var (
	decisionIDPattern  = regexp.MustCompile(`^dec_[A-Za-z0-9_-]{26}$`)
	principalIDPattern = regexp.MustCompile(`^prn_[A-Za-z0-9_-]+$`)

	obligationNames  = []string{"tenant_scope", "repository_allowlist", "repository_scope", "redaction", "minimum_assurance", "step_up"}
	redactionClasses = []string{"personal_identifiers", "contact_details", "free_text"}
	assuranceLevels  = []string{"aal1", "aal2", "aal3"}
)

type object map[string]json.RawMessage

func asObject(raw json.RawMessage) (object, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var o object
	if json.Unmarshal(trimmed, &o) != nil || o == nil {
		return nil, false
	}
	return o, true
}

func asArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var a []json.RawMessage
	if json.Unmarshal(trimmed, &a) != nil || a == nil {
		return nil, false
	}
	return a, true
}

func asString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(trimmed, &s) != nil {
		return "", false
	}
	return s, true
}

func asBool(raw json.RawMessage) (value, ok bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// keysWithin reports that every required member is present and no member is
// outside required and optional. Member names compare exactly: "Allowed" is
// not "allowed".
func keysWithin(o object, required []string, optional ...string) bool {
	for _, k := range required {
		if _, ok := o[k]; !ok {
			return false
		}
	}
	for k := range o {
		if !contains(required, k) && !contains(optional, k) {
			return false
		}
	}
	return true
}

func parseDecision(body []byte, req Request, now time.Time) Decision {
	if rejectDuplicates(body) != nil {
		return deny(fail(ReasonMalformed))
	}
	o, ok := asObject(body)
	if !ok {
		return deny(fail(ReasonMalformed))
	}
	return decodeDecision(o, req, now)
}

func decodeDecision(o object, req Request, now time.Time) Decision {
	required := []string{"schema_version", "decision_id", "allowed", "reason_code", "principal_id", "organization_id", "policy_revision", "expires_at"}
	if !keysWithin(o, required, "obligations") {
		return deny(fail(ReasonMalformed))
	}
	if v, ok := asString(o["schema_version"]); !ok || v != decisionSchema {
		return deny(fail(ReasonMalformed))
	}
	var w decision.Wire
	var ok bool
	if w.DecisionID, ok = asString(o["decision_id"]); !ok || !decisionIDPattern.MatchString(w.DecisionID) {
		return deny(fail(ReasonMalformed))
	}
	if w.Allowed, ok = asBool(o["allowed"]); !ok {
		return deny(fail(ReasonMalformed))
	}
	code, ok := asString(o["reason_code"])
	if !ok {
		return deny(fail(ReasonMalformed))
	}
	if w.Reason, ok = wireReason(code); !ok || w.Allowed != allowReasons[w.Reason] {
		return deny(fail(ReasonMalformed))
	}
	if w.PrincipalID, ok = asString(o["principal_id"]); !ok || !principalIDPattern.MatchString(w.PrincipalID) {
		return deny(fail(ReasonMalformed))
	}
	if string(bytes.TrimSpace(o["organization_id"])) != "null" {
		if w.Organization, ok = asString(o["organization_id"]); !ok || w.Organization == "" {
			return deny(fail(ReasonMalformed))
		}
		w.HasOrg = true
	}
	revision, err := parseRevision(bytes.TrimSpace(o["policy_revision"]))
	if err != nil {
		return deny(fail(ReasonMalformed))
	}
	w.PolicyRevision = revision
	expires, ok := asString(o["expires_at"])
	if !ok {
		return deny(fail(ReasonMalformed))
	}
	if w.ExpiresAt, err = parseExpiry(expires); err != nil {
		return deny(fail(ReasonMalformed))
	}
	if raw, present := o["obligations"]; present {
		if !w.Allowed {
			return deny(fail(ReasonMalformed))
		}
		obligations, e := decodeObligations(raw)
		if e != nil {
			return deny(e)
		}
		w.Obligations = obligations
	} else if w.Allowed {
		w.Obligations = decision.NewObligations(decision.ObligationSpec{})
	}
	if w.HasOrg != (req.OrganizationID != "") || (w.HasOrg && w.Organization != req.OrganizationID) {
		return deny(fail(ReasonMismatch))
	}
	w.Cacheable = w.ExpiresAt.After(now)
	return decision.FromWire(w)
}

func decodeObligations(raw json.RawMessage) (decision.Obligations, *Error) {
	var zero decision.Obligations
	o, ok := asObject(raw)
	if !ok {
		return zero, fail(ReasonMalformed)
	}
	for k := range o {
		if !contains(obligationNames, k) {
			return zero, fail(ReasonUnknownObligation)
		}
	}
	var spec decision.ObligationSpec
	if v, present := o["tenant_scope"]; present {
		s, ok := asString(v)
		if !ok || s == "" {
			return zero, fail(ReasonMalformed)
		}
		spec.Tenant = s
	}
	_, hasScope := o["repository_scope"]
	if _, hasList := o["repository_allowlist"]; hasScope && hasList {
		return zero, fail(ReasonMalformed)
	}
	if hasScope {
		s, ok := asString(o["repository_scope"])
		if !ok {
			return zero, fail(ReasonMalformed)
		}
		if decision.RepositoryScope(s) != decision.ScopeOrganization {
			return zero, fail(ReasonUnknownObligation)
		}
		spec.Scope = decision.ScopeOrganization
	}
	if v, present := o["repository_allowlist"]; present {
		items, ok := asArray(v)
		if !ok || len(items) > maxRepositoryAllowlist {
			return zero, fail(ReasonMalformed)
		}
		spec.Allowlist = []string{}
		for _, item := range items {
			s, ok := asString(item)
			if !ok || !repokey.Valid(s) || contains(spec.Allowlist, s) {
				return zero, fail(ReasonMalformed)
			}
			spec.Allowlist = append(spec.Allowlist, s)
		}
	}
	if v, present := o["redaction"]; present {
		items, ok := asArray(v)
		if !ok {
			return zero, fail(ReasonMalformed)
		}
		var seen []string
		for _, item := range items {
			s, ok := asString(item)
			if !ok || contains(seen, s) {
				return zero, fail(ReasonMalformed)
			}
			if !contains(redactionClasses, s) {
				return zero, fail(ReasonUnknownObligation)
			}
			seen = append(seen, s)
			spec.Redaction = append(spec.Redaction, RedactionClass(s))
		}
	}
	if v, present := o["minimum_assurance"]; present {
		s, ok := asString(v)
		if !ok {
			return zero, fail(ReasonMalformed)
		}
		if !contains(assuranceLevels, s) {
			return zero, fail(ReasonUnknownObligation)
		}
		spec.Assurance = Assurance(s)
	}
	if v, present := o["step_up"]; present {
		b, ok := asBool(v)
		if !ok {
			return zero, fail(ReasonMalformed)
		}
		spec.StepUp = b
	}
	return decision.NewObligations(spec), nil
}

func denyAll(e *Error, n int) []Decision {
	out := make([]Decision, n)
	for i := range out {
		out[i] = deny(e)
	}
	return out
}

// parseBatch reads a batch response against the requests it answers. The
// binding is count, then position and explicit index: a response with fewer or
// more decisions, or an index that is swapped, repeated, skipped or in decimal
// form, is refused entirely and every item is denied. A client must never use
// the items that happen to line up: that is one item's allow read against
// another item's resource. One bad item denies the whole batch for the same
// reason.
func parseBatch(body []byte, reqs []Request, now time.Time) []Decision {
	bad := func(r Reason) []Decision { return denyAll(fail(r), len(reqs)) }
	if rejectDuplicates(body) != nil {
		return bad(ReasonMalformed)
	}
	o, ok := asObject(body)
	if !ok || !keysWithin(o, []string{"schema_version", "decisions"}) {
		return bad(ReasonMalformed)
	}
	if v, ok := asString(o["schema_version"]); !ok || v != batchSchema {
		return bad(ReasonMalformed)
	}
	items, ok := asArray(o["decisions"])
	if !ok || len(items) == 0 || len(items) > MaxBatch {
		return bad(ReasonMalformed)
	}
	entries := make([]object, len(items))
	indexes := make([]uint64, len(items))
	for i, item := range items {
		entry, ok := asObject(item)
		if !ok || !keysWithin(entry, []string{"index", "decision"}) {
			return bad(ReasonMalformed)
		}
		index, err := parseRevision(bytes.TrimSpace(entry["index"]))
		if err != nil || index >= MaxBatch {
			return bad(ReasonMalformed)
		}
		inner, ok := asObject(entry["decision"])
		if !ok {
			return bad(ReasonMalformed)
		}
		entries[i], indexes[i] = inner, index
	}
	if len(items) != len(reqs) {
		return bad(ReasonMismatch)
	}
	for i, index := range indexes {
		if int(index) != i {
			return bad(ReasonMismatch)
		}
	}
	out := make([]Decision, len(items))
	for i, inner := range entries {
		out[i] = decodeDecision(inner, reqs[i], now)
		if out[i].Local() {
			e, _ := out[i].Err().(*Error)
			if e == nil {
				e = fail(ReasonMalformed)
			}
			return denyAll(e, len(reqs))
		}
	}
	return out
}
