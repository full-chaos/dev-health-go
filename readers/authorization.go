package readers

// AuthorizationScope narrows a work-item content read to the repositories a
// caller may see. GrantedRepositoryIDs is the requesting principal's own
// repository grants; RequestedRepositoryIDs is the request's own requested
// repository scope. Both are repository ids in the same string form
// ReadRepositoryIdentity/ReadRepositoryIDs already use (toString(r.id)).
//
// A nil list is allow-all for that dimension; a non-nil, empty list is
// deny-all (grants/scope explicitly narrowed to nothing). When both are
// non-nil, a row must satisfy BOTH -- the intersection of what the
// principal may see and what this request actually asked for, never
// either alone.
//
// The zero value, AuthorizationScope{}, is allow-all on both dimensions:
// it renders no predicate at all, so a caller that never sets a scope gets
// the exact statement it would have gotten before this existed.
type AuthorizationScope struct {
	GrantedRepositoryIDs   []string
	RequestedRepositoryIDs []string
}

// predicate returns the SQL fragment (each clause starting with " AND ...")
// and the bindings it references, narrowing repoIDColumn -- the
// toString-able repository id expression a statement's FROM already
// selects (e.g. "w.repo_id") -- to s's scope.
//
// This reuses the SAME work_items<->repos relation ReadWorkItemRepository
// already expresses (its join condition, "r.id = w.repo_id": a work item's
// repo_id column IS the foreign key to repos.id). Filtering directly on
// that column is that relation, not a new one, and does it without an
// INNER JOIN to repos: repos carries no fact this predicate needs, and
// joining it would additionally require a matching repos row to exist,
// silently dropping a repo-less or orphaned work item's row even when its
// repository is inside an allow-all or otherwise-granted scope. Binding
// the ids as an Array(String) parameter (never string-concatenated) is
// clickhouse.Client's only supported safe path for a caller-controlled
// list -- see clickHouseStringArray.
func (s AuthorizationScope) predicate(repoIDColumn string) (string, []Binding) {
	var clause string
	var bindings []Binding
	if s.GrantedRepositoryIDs != nil {
		clause += " AND toString(" + repoIDColumn + ") IN {authorized_repo_ids:Array(String)}"
		bindings = append(bindings, Binding{Name: "authorized_repo_ids", Value: s.GrantedRepositoryIDs})
	}
	if s.RequestedRepositoryIDs != nil {
		clause += " AND toString(" + repoIDColumn + ") IN {requested_repo_ids:Array(String)}"
		bindings = append(bindings, Binding{Name: "requested_repo_ids", Value: s.RequestedRepositoryIDs})
	}
	return clause, bindings
}
