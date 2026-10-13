package readers

import "strings"

// WorkItemCandidates narrows WorkItemScopeSQLAmong's two
// organization-wide authorization aggregates (project ownership and
// pull-request links) to the work items a statement can hold, so their
// cost follows the statement rather than the organization.
//
// It narrows only; it never widens. Each aggregate keeps or drops whole
// groups (one per work item id, one per provider and project identity), so a
// kept group is the same group the unnarrowed relation computes. A work
// item whose group was dropped joins no aggregate row and is denied by both
// repo-less paths, exactly as an item with no owning project or no link is
// today. The candidate set must therefore hold every work_item_id the
// statement's work_items rows (alias w) can carry; an extra id costs a
// little work and changes nothing, a missing id denies that item.
//
// The zero value narrows nothing. Every value is a bound parameter: the
// relation text is built here from a package-owned binding or from the
// caller's own fixed SQL, never from a value.
type WorkItemCandidates struct {
	relation string
	bindings []Binding
}

const workItemCandidateIDsBinding = "wia_candidate_work_item_ids"

// WorkItemCandidateIDs is the candidate set of the given work_item_id
// values, bound as one Array(String) parameter. A nil or empty list is an
// empty candidate set: every repo-less path is denied.
func WorkItemCandidateIDs(ids []string) WorkItemCandidates {
	if ids == nil {
		ids = []string{}
	}
	return WorkItemCandidates{
		relation: "{" + workItemCandidateIDsBinding + ":Array(String)}",
		bindings: []Binding{{Name: workItemCandidateIDsBinding, Value: ids}},
	}
}

// WorkItemCandidateSubquery is the candidate set a caller's subquery
// selects: one String column of work_item_id values. selectSQL is fixed
// statement text of the caller's own (it may read {org_id:String}); every
// value it compares against is a parameter named in bindings. A binding
// may not reuse a name this package binds (org_id, ids, or the authorized_,
// requested_ and wia_ prefixes); doing so panics, as a programming error.
func WorkItemCandidateSubquery(selectSQL string, bindings ...Binding) WorkItemCandidates {
	if strings.TrimSpace(selectSQL) == "" {
		panic("readers: empty work item candidate subquery")
	}
	for _, binding := range bindings {
		if reservedWorkItemBindingName(binding.Name) {
			panic("readers: work item candidate binding " + binding.Name + " reuses a reserved name")
		}
	}
	return WorkItemCandidates{
		relation: "(" + selectSQL + ")",
		bindings: append([]Binding(nil), bindings...),
	}
}

func reservedWorkItemBindingName(name string) bool {
	if name == "org_id" || name == "ids" {
		return true
	}
	for _, prefix := range []string{"authorized_", "requested_", "wia_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
