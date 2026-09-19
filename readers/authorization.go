package readers

import (
	"regexp"
	"sort"
	"strings"
)

// RepositorySelectorSet is one side of a repository authorization selector.
// All grants every repository row that belongs to the organization. ExactSlugs
// and Owners are normalized repository names and owner names; they are ORed
// within this set. A zero set denies every row.
//
// The reader accepts the typed form so the SQL relation and predicate remain
// package-owned. Callers cannot provide an SQL fragment, table alias, or
// callback that changes which repository metadata the reader evaluates.
type RepositorySelectorSet struct {
	All        bool
	ExactSlugs []string
	Owners     []string
}

// RepositorySelectorScope carries the principal's repository grants and the
// request's optional repository restriction. A nil Requested value means the
// request did not add a repository selector. A non-nil zero Requested set
// denies every row, including an explicit empty request.
type RepositorySelectorScope struct {
	Granted   RepositorySelectorSet
	Requested *RepositorySelectorSet
}

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

	// RepositorySelectors is the optional slug/owner selector mode used by
	// the canonical work-item path. When nil, the ID-only predicate below is
	// used and legacy statement text stays byte-identical. When non-nil, its
	// selector predicate is evaluated in WorkItemScopeSQL against the fixed
	// work_items↔repos relation and, for an item with no repository,
	// against its project's owned repositories and its linked pull
	// requests' repositories (see WorkItemAuthorizationPaths), and is ANDed
	// with either ID predicate.
	RepositorySelectors *RepositorySelectorScope
}

// WorkItemScopeSQL is the closed SQL surface shared by a work-item census
// mask and each work-item content reader. JoinSQL is empty for the legacy
// ID-only mode and otherwise is the fixed organization-qualified LEFT JOIN
// whose aliases are owned by this package. AuthorizationExpr is a boolean
// expression over those fixed aliases. Bindings contains only typed ID,
// selector-array, and selector-flag bindings; the caller still supplies the
// common org_id and ids bindings through QueryOrgScopedNamed.
//
// Consumers should append JoinSQL after `FROM work_items AS w FINAL` and use
// AuthorizationExpr in both the S1 projection mask and the S2/S3/actual
// completion WHERE clause. Keeping both surfaces on this one value prevents
// the census and content readers from drifting apart.
type WorkItemScopeSQLResult struct {
	JoinSQL           string
	AuthorizationExpr string
	Bindings          []Binding

	// Provenance names every path that can authorize a row, in the fixed
	// order of WorkItemAuthorizationPaths, each with a boolean expression
	// over the same aliases and bindings as AuthorizationExpr. A row's
	// AuthorizationExpr is true exactly when at least one path expression
	// is true: every path expression already carries the request's own
	// restrictions, so a true path never names a row the request excludes.
	// More than one path can be true for one row; a caller discloses each.
	//
	// Populated in selector mode only. The legacy ID-only and zero-value
	// modes have no path to name and leave it nil.
	Provenance []WorkItemAuthorizationPathSQL
}

// WorkItemAuthorizationPathSQL is one authorization path of a work-item
// row: its closed-vocabulary name and its boolean SQL expression.
type WorkItemAuthorizationPathSQL struct {
	Path string
	Expr string
}

// The closed vocabulary of work-item authorization paths.
//
//   - organization_grant: the principal holds the organization-wide grant.
//   - direct_repo: the item carries a real repository whose same-org
//     metadata matches the grant. An item with a real repository is decided
//     by this path alone among the repository paths.
//   - project_ownership: the item carries no repository, and a team that
//     owns the item's project owns a repository that matches the grant
//     (projects -> team_project_ownership -> team_repo_ownership -> repos,
//     current ownership only).
//   - pr_link: the item carries no repository, and an issue-to-pull-request
//     link row names the item and a repository that matches the grant. Only
//     a link the provider recorded or the pull request text names counts;
//     a time-window guess never authorizes.
//
// A repo-less item with neither a project path nor a link path stays
// denied for every principal without the organization-wide grant.
const (
	WorkItemAuthorizationOrganizationGrant = "organization_grant"
	WorkItemAuthorizationDirectRepository  = "direct_repo"
	WorkItemAuthorizationProjectOwnership  = "project_ownership"
	WorkItemAuthorizationPullRequestLink   = "pr_link"
)

// WorkItemAuthorizationPaths is the fixed order of Provenance entries.
func WorkItemAuthorizationPaths() []string {
	return []string{
		WorkItemAuthorizationOrganizationGrant,
		WorkItemAuthorizationDirectRepository,
		WorkItemAuthorizationProjectOwnership,
		WorkItemAuthorizationPullRequestLink,
	}
}

const (
	workItemRepositoryJoin = "LEFT JOIN repos AS r FINAL ON r.id = w.repo_id AND r.org_id = w.org_id"
	// ClickHouse trimBoth removes ordinary spaces only. The producer uses
	// strings.TrimSpace, so the regex also removes the Unicode White_Space
	// characters and ASCII controls that the Go normalizer accepts.
	workItemRepositoryWhitespacePattern = `'^[\\p{Zs}\\p{Zl}\\p{Zp}\\t\\n\\r\\v\\f\\x{85}]+|[\\p{Zs}\\p{Zl}\\p{Zp}\\t\\n\\r\\v\\f\\x{85}]+$'`
	// ClickHouse lowerUTF8 leaves U+212A (Kelvin sign) and U+0130 (dotted
	// capital I) unchanged, while the producer's strings.ToLower maps them
	// to ASCII k and i. Keep those two producer-accepted preimages aligned
	// before the ASCII repository grammar is applied.
	workItemRepoSlug = "replaceAll(replaceAll(lowerUTF8(replaceRegexpAll(trimBoth(ifNull(r.repo, '')), " + workItemRepositoryWhitespacePattern + ", '')), 'K', 'k'), 'İ', 'i')"
	// This is the same repository-part grammar as acr/internal/auth. The
	// match guard is needed before owner extraction: a malformed value such
	// as owner/not-a-slug/extra must never match owner/* by its prefix.
	workItemRepoSlugGrammar = ", '^[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?/[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?$')"
	workItemRepoSlugValid   = "match(" + workItemRepoSlug + workItemRepoSlugGrammar
	workItemRepoNamed       = workItemRepoSlug + " != ''"

	zeroUUID = "00000000-0000-0000-0000-000000000000"
)

// workItemRepoPresent is the one spelling of "this work item carries a real
// repository", shared by the direct path and the two repo-less paths so
// the paths cannot overlap or leave a gap between them.
var workItemRepoPresent = keyPresentSQL("w.repo_id", uuidKey)

// keyKind names the storage shape of a key column for keyPresentSQL.
type keyKind uint8

const (
	// uuidKey is a UUID column; the empty string and the zero UUID are
	// absent. On a Nullable(UUID) column a NULL fails both comparisons, so
	// NULL is absent too.
	uuidKey keyKind = iota + 1
	// stringKey is a non-Nullable String column; the empty string is
	// absent.
	stringKey
)

// keyPresentSQL is the shared predicate for "this key column carries a
// value" across the key shapes this package joins on. Every presence test
// in the work-item authorization relation goes through it, so NULL, the
// empty string and the zero UUID mean "absent" in one place rather than
// per clause. column is an internal Go string literal at every call site,
// never caller-supplied.
func keyPresentSQL(column string, kind keyKind) string {
	switch kind {
	case uuidKey:
		return "toString(" + column + ") != '' AND toString(" + column + ") != '" + zeroUUID + "'"
	case stringKey:
		return column + " != ''"
	}
	panic("readers: unknown key kind")
}

// repositorySlugSQL is workItemRepoSlug over another repository-name
// column: the same trim, case fold and producer-preimage mapping, so a
// repository reached through ownership or a link row is compared under the
// exact normalization the direct path uses.
func repositorySlugSQL(column string) string {
	return strings.Replace(workItemRepoSlug, "ifNull(r.repo, '')", "ifNull("+column+", '')", 1)
}

// grantedRepositoryMatchSQL is the principal-grant test for one repository
// name column: grammar first, then exact slug or owner membership.
func grantedRepositoryMatchSQL(prefix, column string) string {
	slug := repositorySlugSQL(column)
	return "match(" + slug + workItemRepoSlugGrammar + " = 1 AND " + repositorySelectorMatchOver(prefix, slug)
}

// workItemAuthorizingLinkProvenances are the link provenances that
// authorize. work_graph_issue_pr also carries 'heuristic' rows (a pull
// request near an issue in time), which are a guess rather than a link and
// never authorize.
const workItemAuthorizingLinkProvenances = "'native', 'explicit_text'"

// workItemProjectOwnershipSQL is true when the item's project (matched on
// provider and any identity value the project answers to) is owned by a
// team that currently owns a repository the grant matches. Every relation
// is read organization-scoped through the caller's {org_id} binding; the
// grant arrays are the same bindings the direct path reads.
func workItemProjectOwnershipSQL(prefix string) string {
	current := OwnershipValidityPredicate(TimeBound{})
	return "(w.provider, w.project_id) IN (" +
		"SELECT wia_scope.provider, wia_scope.scope FROM (SELECT provider, id, scope FROM " + ProjectIdentityCatalogSQL() + ") AS wia_scope " +
		"WHERE (wia_scope.provider, wia_scope.id) IN (" +
		"SELECT wia_owned.provider, wia_owned.id FROM (SELECT provider, id, team_id FROM " + ProjectOwnershipCatalogJoinSQL(current) + ") AS wia_owned " +
		"INNER JOIN (SELECT team_id, repo_id FROM team_repo_ownership FINAL WHERE org_id = {org_id:String} AND " + keyPresentSQL("repo_id", uuidKey) + current + " GROUP BY team_id, repo_id) AS wia_team_repo ON wia_team_repo.team_id = wia_owned.team_id " +
		"INNER JOIN (SELECT id, repo FROM repos FINAL WHERE org_id = {org_id:String}) AS wia_owned_repo ON wia_owned_repo.id = wia_team_repo.repo_id " +
		"WHERE " + grantedRepositoryMatchSQL(prefix, "wia_owned_repo.repo") + "))"
}

// workItemPullRequestLinkSQL is true when an authorizing issue-to-pull-
// request link row names the item and a same-org repository the grant
// matches.
func workItemPullRequestLinkSQL(prefix string) string {
	return "w.work_item_id IN (" +
		"SELECT wia_link.work_item_id FROM work_graph_issue_pr AS wia_link FINAL " +
		"INNER JOIN (SELECT id, repo FROM repos FINAL WHERE org_id = {org_id:String}) AS wia_link_repo ON wia_link_repo.id = wia_link.repo_id " +
		"WHERE wia_link.org_id = {org_id:String} AND wia_link.provenance IN (" + workItemAuthorizingLinkProvenances + ") AND " +
		keyPresentSQL("wia_link.work_item_id", stringKey) + " AND " + keyPresentSQL("wia_link.repo_id", uuidKey) + " AND " +
		grantedRepositoryMatchSQL(prefix, "wia_link_repo.repo") + ")"
}

var repositoryPartPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?$`)

// WorkItemScopeSQL renders the fixed work-item authorization relation and
// expression. It is intentionally a function over AuthorizationScope, with
// no caller-provided SQL or alias parameters, so S1 and every content reader
// cannot accidentally use different repository semantics.
//
// In selector mode the expression also reads projects,
// team_project_ownership, team_repo_ownership, work_graph_issue_pr and
// repos through uncorrelated subqueries, each filtered by the caller's
// {org_id} binding. A caller that caps rows read with Settings must budget
// for those relations as well as work_items.
func WorkItemScopeSQL(scope AuthorizationScope) WorkItemScopeSQLResult {
	result := WorkItemScopeSQLResult{}
	var expressions []string
	if scope.GrantedRepositoryIDs != nil {
		expressions = append(expressions, "toString(w.repo_id) IN {authorized_repo_ids:Array(String)}")
		result.Bindings = append(result.Bindings, Binding{Name: "authorized_repo_ids", Value: scope.GrantedRepositoryIDs})
	}
	if scope.RequestedRepositoryIDs != nil {
		expressions = append(expressions, "toString(w.repo_id) IN {requested_repo_ids:Array(String)}")
		result.Bindings = append(result.Bindings, Binding{Name: "requested_repo_ids", Value: scope.RequestedRepositoryIDs})
	}

	if scope.RepositorySelectors != nil {
		result.JoinSQL = workItemRepositoryJoin
		// Every path expression carries the ID dimensions and the requested
		// selector, so a true path always implies an authorized row.
		restrictions := append([]string(nil), expressions...)
		grantedExpr, grantedBindings, paths := renderRepositorySelectorSet(scope.RepositorySelectors.Granted, "authorized")
		expressions = append(expressions, grantedExpr)
		result.Bindings = append(result.Bindings, grantedBindings...)
		if scope.RepositorySelectors.Requested != nil {
			requestedExpr, requestedBindings := renderRequestedRepositorySelectorSet(*scope.RepositorySelectors.Requested)
			expressions = append(expressions, requestedExpr)
			restrictions = append(restrictions, requestedExpr)
			result.Bindings = append(result.Bindings, requestedBindings...)
		}
		for _, path := range paths {
			result.Provenance = append(result.Provenance, WorkItemAuthorizationPathSQL{
				Path: path.Path,
				Expr: strings.Join(append([]string{path.Expr}, restrictions...), " AND "),
			})
		}
	}

	if len(expressions) == 0 {
		// The expression is useful to S1 consumers even when the zero-value
		// scope has no predicate. Existing readers special-case this mode to
		// preserve their exact pre-selector SQL.
		result.AuthorizationExpr = "1"
		return result
	}
	result.AuthorizationExpr = strings.Join(expressions, " AND ")
	return result
}

func renderRepositorySelectorSet(set RepositorySelectorSet, prefix string) (string, []Binding, []WorkItemAuthorizationPathSQL) {
	exactSlugs := normalizeRepositorySlugs(set.ExactSlugs)
	owners := normalizeRepositoryOwners(set.Owners)
	bindings := []Binding{
		{Name: prefix + "_repo_all", Value: uint8(boolToFlag(set.All))},
		{Name: prefix + "_repo_slugs", Value: exactSlugs},
		{Name: prefix + "_repo_owners", Value: owners},
	}

	// The global grant wildcard intentionally short-circuits metadata
	// matching. This is what permits repo-less and orphan work items for an
	// organization-wide principal when there is no requested selector.
	//
	// A repo-less item has two further paths, both through the grant's own
	// repositories: its project's owning teams, and its linked pull
	// requests. A repo-less item with neither stays denied. An item with a
	// real repository never takes either further path: its own repository
	// decides.
	organization := "{" + prefix + "_repo_all:UInt8} = 1"
	direct := "(" + workItemRepoPresent + " AND " + workItemRepoSlugValid + " = 1 AND " + repositorySelectorMatch(prefix) + ")"
	repoLess := "NOT (" + workItemRepoPresent + ")"
	project := "(" + repoLess + " AND " + keyPresentSQL("w.project_id", stringKey) + " AND " + workItemProjectOwnershipSQL(prefix) + ")"
	link := "(" + repoLess + " AND " + workItemPullRequestLinkSQL(prefix) + ")"
	paths := []WorkItemAuthorizationPathSQL{
		{Path: WorkItemAuthorizationOrganizationGrant, Expr: "(" + organization + ")"},
		{Path: WorkItemAuthorizationDirectRepository, Expr: direct},
		{Path: WorkItemAuthorizationProjectOwnership, Expr: project},
		{Path: WorkItemAuthorizationPullRequestLink, Expr: link},
	}
	match := "(" + organization + " OR " + direct + " OR " + project + " OR " + link + ")"
	return match, bindings, paths
}

func renderRequestedRepositorySelectorSet(set RepositorySelectorSet) (string, []Binding) {
	exactSlugs := normalizeRepositorySlugs(set.ExactSlugs)
	owners := normalizeRepositoryOwners(set.Owners)
	bindings := []Binding{
		{Name: "requested_repo_all", Value: uint8(boolToFlag(set.All))},
		{Name: "requested_repo_slugs", Value: exactSlugs},
		{Name: "requested_repo_owners", Value: owners},
	}
	// Unlike a principal's organization-wide grant, an explicit requested
	// wildcard still denotes a repository scope. It therefore requires a
	// real nonzero repository and a nonempty same-org metadata row, even when
	// All is true. Global '*' keeps its production short-circuit semantics and
	// therefore does not add a new slug-validity policy. Exact and owner
	// selectors still require the normalized repository-name grammar before
	// comparing either the full slug or its owner.
	validMatch := "(" + workItemRepoSlugValid + " = 1 AND " + repositorySelectorMatch("requested") + ")"
	match := "(" + workItemRepoPresent + " AND " + workItemRepoNamed + " AND ({requested_repo_all:UInt8} = 1 OR " + validMatch + "))"
	return match, bindings
}

func repositorySelectorMatch(prefix string) string {
	return repositorySelectorMatchOver(prefix, workItemRepoSlug)
}

func repositorySelectorMatchOver(prefix, slug string) string {
	return "(has({" + prefix + "_repo_slugs:Array(String)}, " + slug + ") OR has({" + prefix + "_repo_owners:Array(String)}, arrayElement(splitByChar('/', " + slug + "), 1)))"
}

func boolToFlag(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}

func normalizeRepositorySlugs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		normalized := strings.ToLower(strings.TrimSpace(raw))
		parts := strings.Split(normalized, "/")
		if len(parts) != 2 || !repositoryPartPattern.MatchString(parts[0]) || !repositoryPartPattern.MatchString(parts[1]) {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result
}

func normalizeRepositoryOwners(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		normalized := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasSuffix(normalized, "/*") {
			normalized = strings.TrimSuffix(normalized, "/*")
		}
		if normalized == "" || strings.Contains(normalized, "/") || !repositoryPartPattern.MatchString(normalized) {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result
}

// predicate returns the legacy ID-only SQL fragment (each clause starting
// with " AND ...") and the bindings it references, narrowing repoIDColumn --
// the toString-able repository id expression a statement's FROM already
// selects (e.g. "w.repo_id") -- to s's scope. Selector mode is rendered by
// WorkItemScopeSQL so its package-owned repos join is present in the same
// statement.
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
