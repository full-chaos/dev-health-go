//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/clickhouse"
	"github.com/full-chaos/dev-health-go/readers"
	"github.com/full-chaos/dev-health-go/schema"
)

// The work-item authorization domain, seeded into a fresh database built
// from the production DDL (schema.DDL) and read back through the public
// readers and WorkItemScopeSQL's own provenance expressions. The expected
// result for every cell comes from authzOracle below, which models the
// rule over the fixture's own fields and never renders SQL.

const (
	authzOrg      = "authz-paths-org"
	authzOtherOrg = "authz-paths-other-org"
	authzZeroRepo = "00000000-0000-0000-0000-000000000000"
)

type authzRepo struct {
	id, slug, org string
}

var (
	authzRepoGranted    = authzRepo{"11111111-1111-4111-8111-000000000001", "acme/granted", authzOrg}
	authzRepoNonGranted = authzRepo{"11111111-1111-4111-8111-000000000002", "other/nongranted", authzOrg}
	// Same owner and a granted-looking slug, but another organization's row.
	authzRepoOtherOrg = authzRepo{"11111111-1111-4111-8111-000000000003", "acme/granted", authzOtherOrg}
	// Owner-matched only after the shared trim and case fold.
	authzRepoUpper = authzRepo{"11111111-1111-4111-8111-000000000004", " Acme/Upper ", authzOrg}
	// Owner prefix matches, but the name is not a repository slug.
	authzRepoMalformed = authzRepo{"11111111-1111-4111-8111-000000000005", "acme/not/real", authzOrg}
	// A metadata row stored under the zero UUID: never a real repository.
	authzRepoZero = authzRepo{authzZeroRepo, "acme/zero-sentinel", authzOrg}
)

// authzProject is one project and the repository its owning team owns in
// this organization (nil when no current same-org ownership reaches a real
// repository).
type authzProject struct {
	id, key  string
	provider string
	owned    *authzRepo
}

var (
	authzProjectOwned    = authzProject{id: "proj-owned", provider: "linear", owned: &authzRepoGranted}
	authzProjectNonOwned = authzProject{id: "proj-nongranted", provider: "linear", owned: &authzRepoNonGranted}
	// Answers to its unambiguous key as well as its id.
	authzProjectKeyed = authzProject{id: "proj-keyed", key: "KEYED", provider: "linear", owned: &authzRepoGranted}
	// Owned by the granted team only through an expired ownership window.
	authzProjectExpired = authzProject{id: "proj-expired", provider: "linear"}
	// A project no team owns.
	authzProjectUnowned = authzProject{id: "proj-unowned", provider: "linear"}
	// Owned by a team whose only repository row has no repository id.
	authzProjectNullRepo = authzProject{id: "proj-null-repo", provider: "linear"}
	// Owned only through another organization's ownership rows.
	authzProjectOtherOrg = authzProject{id: "proj-other-org-owner", provider: "linear"}
	// Owned by a team whose repository row exists only in another org.
	authzProjectTeamRepoOtherOrg = authzProject{id: "proj-team-repo-other-org", provider: "linear"}
	// Owned by a team whose repository id is another org's repository.
	authzProjectForeignRepo = authzProject{id: "proj-foreign-repo", provider: "linear"}
	// Owned by a team owning the zero-UUID metadata row.
	authzProjectZeroRepo = authzProject{id: "proj-zero-repo", provider: "linear"}
	// Owned by a team owning a malformed repository name.
	authzProjectMalformed = authzProject{id: "proj-malformed-repo", provider: "linear"}
	// Owned by a team owning a repository stored with case and padding.
	authzProjectUpper = authzProject{id: "proj-upper-repo", provider: "linear", owned: &authzRepoUpper}
	// A project row whose id is empty, owned via the granted repository.
	authzProjectEmptyID = authzProject{id: "", provider: "linear"}
)

type authzLink struct {
	repo       authzRepo
	provenance string
	org        string
}

type authzItem struct {
	id       string
	provider string
	repo     *authzRepo
	// projectID is the item's own project_id column; "" for a project-less
	// sub-issue or an orphan.
	projectID string
	project   *authzProject
	parentID  string
	links     []authzLink
}

func (item authzItem) repoID() string {
	if item.repo == nil {
		return authzZeroRepo
	}
	return item.repo.id
}

type authzGrant struct {
	name string
	set  readers.RepositorySelectorSet
}

var authzGrants = []authzGrant{
	{"all", readers.RepositorySelectorSet{All: true}},
	{"matching-exact", readers.RepositorySelectorSet{ExactSlugs: []string{"ACME/Granted"}}},
	{"matching-owner", readers.RepositorySelectorSet{Owners: []string{"acme/*"}}},
	{"non-matching", readers.RepositorySelectorSet{ExactSlugs: []string{"zzz/none"}}},
	{"empty", readers.RepositorySelectorSet{}},
}

func authzGrantMatches(set readers.RepositorySelectorSet, repo authzRepo) bool {
	slug := strings.ToLower(strings.TrimSpace(repo.slug))
	if repo.org != authzOrg || repo.id == authzZeroRepo || strings.Count(slug, "/") != 1 {
		return false
	}
	for _, exact := range set.ExactSlugs {
		if strings.ToLower(strings.TrimSpace(exact)) == slug {
			return true
		}
	}
	owner := strings.SplitN(slug, "/", 2)[0]
	for _, raw := range set.Owners {
		if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "/*") == owner {
			return true
		}
	}
	return false
}

// authzOracle is the rule of record over the fixture model: the
// organization grant; a real repository decides by itself; a repo-less
// item is admitted by its project's owned repository or by an authorizing
// link, each matched against the grant; the request's selector narrows
// every path.
func authzOracle(item authzItem, granted readers.RepositorySelectorSet, requested *readers.RepositorySelectorSet) (map[string]bool, map[string][]string) {
	paths := map[string]bool{
		readers.WorkItemAuthorizationOrganizationGrant: granted.All,
	}
	evidence := map[string][]string{}
	normalized := func(repo authzRepo) string { return strings.ToLower(strings.TrimSpace(repo.slug)) }
	if item.repo != nil {
		if authzGrantMatches(granted, *item.repo) {
			paths[readers.WorkItemAuthorizationDirectRepository] = true
			evidence[readers.WorkItemAuthorizationDirectRepository] = []string{normalized(*item.repo)}
		}
	} else {
		if item.project != nil && item.project.owned != nil && item.project.provider == item.provider && authzGrantMatches(granted, *item.project.owned) {
			paths[readers.WorkItemAuthorizationProjectOwnership] = true
			evidence[readers.WorkItemAuthorizationProjectOwnership] = []string{normalized(*item.project.owned)}
		}
		for _, link := range item.links {
			if link.org != authzOrg || (link.provenance != "native" && link.provenance != "explicit_text") {
				continue
			}
			if authzGrantMatches(granted, link.repo) {
				paths[readers.WorkItemAuthorizationPullRequestLink] = true
				evidence[readers.WorkItemAuthorizationPullRequestLink] = append(evidence[readers.WorkItemAuthorizationPullRequestLink], normalized(link.repo))
			}
		}
		evidence[readers.WorkItemAuthorizationPullRequestLink] = authzSortedUnique(evidence[readers.WorkItemAuthorizationPullRequestLink])
	}
	if requested != nil {
		requestedOK := item.repo != nil && (requested.All || authzGrantMatches(*requested, *item.repo))
		if !requestedOK {
			for path := range paths {
				paths[path] = false
				delete(evidence, path)
			}
		}
	}
	return paths, evidence
}

func authzSortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func authzAuthorized(paths map[string]bool) bool {
	for _, ok := range paths {
		if ok {
			return true
		}
	}
	return false
}

// authzItems enumerates repository {none, granted, non-granted} x project
// relation {owned via granted repo, owned via non-granted repo, project-less
// sub-issue, orphan} x link {none, native->granted, native->non-granted,
// heuristic->granted}, then the edge rows each guard exists for.
func authzItems() []authzItem {
	repos := []struct {
		name string
		repo *authzRepo
	}{{"norepo", nil}, {"granted", &authzRepoGranted}, {"nongranted", &authzRepoNonGranted}}
	projects := []struct {
		name    string
		project *authzProject
		parent  string
	}{
		{"projowned", &authzProjectOwned, ""},
		{"projnongranted", &authzProjectNonOwned, ""},
		{"subissue", nil, "linear:PARENT-1"},
		{"orphan", nil, ""},
	}
	links := []struct {
		name  string
		links []authzLink
	}{
		{"nolink", nil},
		{"linkgranted", []authzLink{{authzRepoGranted, "native", authzOrg}}},
		{"linknongranted", []authzLink{{authzRepoNonGranted, "native", authzOrg}}},
		{"heuristicgranted", []authzLink{{authzRepoGranted, "heuristic", authzOrg}}},
	}
	var items []authzItem
	for _, r := range repos {
		for _, p := range projects {
			for _, l := range links {
				item := authzItem{
					id:       "linear:" + r.name + "-" + p.name + "-" + l.name,
					provider: "linear",
					repo:     r.repo,
					project:  p.project,
					parentID: p.parent,
					links:    l.links,
				}
				if p.project != nil {
					item.projectID = p.project.id
				}
				items = append(items, item)
			}
		}
	}
	items = append(items,
		authzItem{id: "linear:norepo-orphan-explicittext", provider: "linear", links: []authzLink{{authzRepoGranted, "explicit_text", authzOrg}}},
		authzItem{id: "linear:norepo-orphan-link-otherorgrepo", provider: "linear", links: []authzLink{{authzRepoOtherOrg, "native", authzOrg}}},
		authzItem{id: "linear:norepo-orphan-link-row-otherorg", provider: "linear", links: []authzLink{{authzRepoGranted, "native", authzOtherOrg}}},
		authzItem{id: "linear:norepo-orphan-link-zerorepo", provider: "linear", links: []authzLink{{authzRepoZero, "native", authzOrg}}},
		authzItem{id: "linear:norepo-orphan-link-malformed", provider: "linear", links: []authzLink{{authzRepoMalformed, "native", authzOrg}}},
		authzItem{id: "linear:norepo-orphan-link-upper", provider: "linear", links: []authzLink{{authzRepoUpper, "native", authzOrg}}},
		// Several links: the evidence is every granted repository, once each,
		// sorted; a non-granted and a heuristic link add nothing.
		authzItem{id: "linear:norepo-orphan-multilink", provider: "linear", links: []authzLink{
			{authzRepoUpper, "native", authzOrg}, {authzRepoGranted, "explicit_text", authzOrg}, {authzRepoGranted, "native", authzOrg},
			{authzRepoNonGranted, "native", authzOrg}, {authzRepoMalformed, "heuristic", authzOrg},
		}},
		authzItem{id: "linear:norepo-projteamrepootherorg", provider: "linear", projectID: authzProjectTeamRepoOtherOrg.id, project: &authzProjectTeamRepoOtherOrg},
		authzItem{id: "linear:norepo-projforeignrepo", provider: "linear", projectID: authzProjectForeignRepo.id, project: &authzProjectForeignRepo},
		authzItem{id: "linear:norepo-projzerorepo", provider: "linear", projectID: authzProjectZeroRepo.id, project: &authzProjectZeroRepo},
		authzItem{id: "linear:norepo-projmalformed", provider: "linear", projectID: authzProjectMalformed.id, project: &authzProjectMalformed},
		authzItem{id: "linear:norepo-projupper", provider: "linear", projectID: authzProjectUpper.id, project: &authzProjectUpper},
		// An item with no project_id beside a project whose id is empty.
		authzItem{id: "linear:norepo-noproject-beside-emptyid-project", provider: "linear"},
		// A link row whose work_item_id is empty, beside an item whose id is.
		authzItem{id: "", provider: "linear"},
		authzItem{id: "linear:norepo-projkeyed-by-key", provider: "linear", projectID: authzProjectKeyed.key, project: &authzProjectKeyed},
		authzItem{id: "linear:norepo-projkeyed-by-id", provider: "linear", projectID: authzProjectKeyed.id, project: &authzProjectKeyed},
		authzItem{id: "linear:norepo-projexpired", provider: "linear", projectID: authzProjectExpired.id, project: &authzProjectExpired},
		authzItem{id: "linear:norepo-projunowned", provider: "linear", projectID: authzProjectUnowned.id, project: &authzProjectUnowned},
		authzItem{id: "linear:norepo-projnullrepo", provider: "linear", projectID: authzProjectNullRepo.id, project: &authzProjectNullRepo},
		authzItem{id: "linear:norepo-projotherorgowner", provider: "linear", projectID: authzProjectOtherOrg.id, project: &authzProjectOtherOrg},
		// A jira item naming a linear project's id is not that project.
		authzItem{id: "jira:norepo-projowned-wrongprovider", provider: "jira", projectID: authzProjectOwned.id, project: &authzProjectOwned},
	)
	return items
}

func authzSeedStatements(items []authzItem) []string {
	const at = "'2026-01-01 00:00:00'"
	var stmts []string
	for _, repo := range []authzRepo{authzRepoGranted, authzRepoNonGranted, authzRepoOtherOrg, authzRepoUpper, authzRepoMalformed, authzRepoZero} {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO repos (id, repo, created_at, last_synced, org_id, provider) VALUES ('%s', '%s', %s, %s, '%s', 'github')", repo.id, repo.slug, at, at, repo.org))
	}
	for _, project := range []authzProject{authzProjectOwned, authzProjectNonOwned, authzProjectKeyed, authzProjectExpired, authzProjectUnowned, authzProjectNullRepo, authzProjectOtherOrg,
		authzProjectTeamRepoOtherOrg, authzProjectForeignRepo, authzProjectZeroRepo, authzProjectMalformed, authzProjectUpper, authzProjectEmptyID} {
		key := "NULL"
		if project.key != "" {
			key = "'" + project.key + "'"
		}
		stmts = append(stmts, fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('%s', '%s', '%s', %s, '%s', 1, 'started', '', %s)", project.id, authzOrg, project.provider, key, project.id, at))
	}
	tpo := func(org, team, project, validTo string) string {
		return fmt.Sprintf("INSERT INTO team_project_ownership (org_id, provider, team_id, project_id, project_key, source, valid_from, valid_to, updated_at) VALUES ('%s', 'linear', '%s', '%s', NULL, 'native', %s, %s, %s)", org, team, project, at, validTo, at)
	}
	tro := func(org, team, repoID, fullName, validTo string) string {
		return fmt.Sprintf("INSERT INTO team_repo_ownership (org_id, provider, team_id, repo_id, repo_full_name, match_type, source, is_primary, specificity, priority, valid_from, valid_to, updated_at) VALUES ('%s', 'github', '%s', %s, '%s', 'exact', 'inferred', 0, 0, 0, %s, %s, %s)", org, team, repoID, fullName, at, validTo, at)
	}
	stmts = append(stmts,
		tpo(authzOrg, "team-granted", authzProjectOwned.id, "NULL"),
		tpo(authzOrg, "team-nongranted", authzProjectNonOwned.id, "NULL"),
		tpo(authzOrg, "team-granted", authzProjectKeyed.id, "NULL"),
		tpo(authzOrg, "team-granted", authzProjectExpired.id, "'2026-02-01 00:00:00'"),
		tpo(authzOrg, "team-nullrepo", authzProjectNullRepo.id, "NULL"),
		tpo(authzOtherOrg, "team-granted", authzProjectOtherOrg.id, "NULL"),
		tro(authzOrg, "team-granted", "'"+authzRepoGranted.id+"'", authzRepoGranted.slug, "NULL"),
		tro(authzOrg, "team-nongranted", "'"+authzRepoNonGranted.id+"'", authzRepoNonGranted.slug, "NULL"),
		tro(authzOrg, "team-nullrepo", "NULL", "acme/granted", "NULL"),
		// An expired window for the non-granted team on the granted repo
		// must never widen that team's projects.
		tro(authzOrg, "team-nongranted", "'"+authzRepoGranted.id+"'", authzRepoGranted.slug, "'2026-02-01 00:00:00'"),
		tpo(authzOrg, "team-repo-elsewhere", authzProjectTeamRepoOtherOrg.id, "NULL"),
		tro(authzOtherOrg, "team-repo-elsewhere", "'"+authzRepoGranted.id+"'", authzRepoGranted.slug, "NULL"),
		tpo(authzOrg, "team-foreign", authzProjectForeignRepo.id, "NULL"),
		tro(authzOrg, "team-foreign", "'"+authzRepoOtherOrg.id+"'", authzRepoOtherOrg.slug, "NULL"),
		tpo(authzOrg, "team-zero", authzProjectZeroRepo.id, "NULL"),
		tro(authzOrg, "team-zero", "'"+authzZeroRepo+"'", authzRepoZero.slug, "NULL"),
		tpo(authzOrg, "team-malformed", authzProjectMalformed.id, "NULL"),
		tro(authzOrg, "team-malformed", "'"+authzRepoMalformed.id+"'", authzRepoMalformed.slug, "NULL"),
		tpo(authzOrg, "team-upper", authzProjectUpper.id, "NULL"),
		tro(authzOrg, "team-upper", "'"+authzRepoUpper.id+"'", authzRepoUpper.slug, "NULL"),
		tpo(authzOrg, "team-granted", authzProjectEmptyID.id, "NULL"),
		fmt.Sprintf("INSERT INTO work_graph_issue_pr (repo_id, work_item_id, pr_number, confidence, provenance, evidence, last_synced, org_id) VALUES ('%s', '', 9999, 1, 'native', 'fixture', %s, '%s')", authzRepoGranted.id, at, authzOrg),
	)
	pr := uint32(1)
	for _, item := range items {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO work_items (repo_id, work_item_id, provider, title, type, status, project_key, project_id, native_team_key, project_name, created_at, updated_at, completed_at, parent_id, url, last_synced, org_id) VALUES ('%s', '%s', '%s', 'title %s', 'issue', 'open', '', '%s', '', '', %s, %s, NULL, '%s', '', %s, '%s')",
			item.repoID(), item.id, item.provider, item.id, item.projectID, at, at, item.parentID, at, authzOrg))
		for _, link := range item.links {
			stmts = append(stmts, fmt.Sprintf("INSERT INTO work_graph_issue_pr (repo_id, work_item_id, pr_number, confidence, provenance, evidence, last_synced, org_id) VALUES ('%s', '%s', %d, 1, '%s', 'fixture', %s, '%s')",
				link.repo.id, item.id, pr, link.provenance, at, link.org))
			pr++
		}
	}
	return stmts
}

// authzDatabase creates a fresh database from the production DDL, seeds
// the domain, and returns a reader client bound to it.
func authzDatabase(t *testing.T, items []authzItem) *clickhouse.Client {
	t.Helper()
	required := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1"
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" || os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required when reader integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required for the real authorization-path test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN: %v", err)
	}
	database := fmt.Sprintf("authz_paths_%d", time.Now().UnixNano())
	options, err := clickhousedriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN options: %v", err)
	}
	admin, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open integration admin connection: %v", err)
	}
	ctx := context.Background()
	if err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatalf("create %s: %v", database, err)
	}
	t.Cleanup(func() {
		_ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database)
		_ = admin.Close()
	})
	options.Auth.Database = database
	seed, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open seed connection: %v", err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	stmts := schema.DDL("projects", "repos", "team_project_ownership", "team_repo_ownership", "work_graph_issue_pr", "work_items")
	stmts = append(stmts, authzSeedStatements(items)...)
	for _, stmt := range stmts {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed statement %q: %v", stmt, err)
		}
	}

	parsed.Path = "/" + database
	client, err := clickhouse.NewClickHouseQueryClientWithOptions(clickhouse.Options{DSN: parsed.String(), QueryTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func authzIDs(items []authzItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.repoID()+":"+item.id)
	}
	return ids
}

type authzReadRow struct {
	authorized bool
	paths      map[string]bool
	evidence   map[string][]string
}

// authzReadProvenance runs AuthorizationExpr and every Provenance
// expression in one statement over the seeded rows, through the org-scoped
// bound-parameter funnel.
func authzReadProvenance(t *testing.T, client *clickhouse.Client, scope readers.AuthorizationScope, ids []string) map[string]authzReadRow {
	t.Helper()
	rendered := readers.WorkItemScopeSQL(scope)
	if len(rendered.Provenance) != len(readers.WorkItemAuthorizationPaths()) {
		t.Fatalf("Provenance has %d paths, want %d", len(rendered.Provenance), len(readers.WorkItemAuthorizationPaths()))
	}
	columns := []string{"w.work_item_id", "toUInt8(" + rendered.AuthorizationExpr + ")"}
	for i, path := range rendered.Provenance {
		if path.Path != readers.WorkItemAuthorizationPaths()[i] {
			t.Fatalf("Provenance[%d].Path = %q, want %q", i, path.Path, readers.WorkItemAuthorizationPaths()[i])
		}
		columns = append(columns, "toUInt8("+path.Expr+")", path.RepositoriesExpr)
	}
	statement := "SELECT " + strings.Join(columns, ", ") + "\nFROM work_items AS w FINAL " + rendered.JoinSQL +
		"\nWHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}"
	got := map[string]authzReadRow{}
	err := readers.QueryOrgScopedNamed(context.Background(), client, "authzProvenance", statement, authzOrg, ids, func(row readers.RowScanner) error {
		var id string
		var authorized uint8
		flags := make([]uint8, len(rendered.Provenance))
		repositories := make([][]string, len(rendered.Provenance))
		dest := []any{&id, &authorized}
		for i := range flags {
			dest = append(dest, &flags[i], &repositories[i])
		}
		if err := row.Scan(dest...); err != nil {
			return err
		}
		read := authzReadRow{authorized: authorized == 1, paths: map[string]bool{}, evidence: map[string][]string{}}
		for i, path := range rendered.Provenance {
			read.paths[path.Path] = flags[i] == 1
			if len(repositories[i]) > 0 {
				read.evidence[path.Path] = repositories[i]
			}
		}
		got[id] = read
		return nil
	}, rendered.Bindings...)
	if err != nil {
		t.Fatalf("provenance statement: %v", err)
	}
	return got
}

func TestIntegrationWorkItemAuthorizationPaths(t *testing.T) {
	items := authzItems()
	client := authzDatabase(t, items)
	ids := authzIDs(items)
	ctx := context.Background()

	requestedGranted := readers.RepositorySelectorSet{ExactSlugs: []string{"acme/granted"}}
	type scopeCase struct {
		name      string
		granted   readers.RepositorySelectorSet
		requested *readers.RepositorySelectorSet
	}
	var cases []scopeCase
	for _, grant := range authzGrants {
		cases = append(cases, scopeCase{name: grant.name, granted: grant.set})
	}
	cases = append(cases,
		scopeCase{name: "matching-exact+requested-granted", granted: authzGrants[1].set, requested: &requestedGranted},
		scopeCase{name: "all+requested-granted", granted: authzGrants[0].set, requested: &requestedGranted},
	)

	executed := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: tc.granted, Requested: tc.requested}}
			want := map[string]map[string]bool{}
			wantEvidence := map[string]map[string][]string{}
			var wantAuthorized []string
			for _, item := range items {
				want[item.id], wantEvidence[item.id] = authzOracle(item, tc.granted, tc.requested)
				if authzAuthorized(want[item.id]) {
					wantAuthorized = append(wantAuthorized, item.id)
				}
			}
			sort.Strings(wantAuthorized)

			provenance := authzReadProvenance(t, client, scope, ids)
			if len(provenance) != len(items) {
				t.Fatalf("provenance statement returned %d rows, want every seeded item (%d)", len(provenance), len(items))
			}
			for _, item := range items {
				read := provenance[item.id]
				for _, path := range readers.WorkItemAuthorizationPaths() {
					if read.paths[path] != want[item.id][path] {
						t.Errorf("%s path %s = %v, want %v", item.id, path, read.paths[path], want[item.id][path])
					}
					if strings.Join(read.evidence[path], ",") != strings.Join(wantEvidence[item.id][path], ",") {
						t.Errorf("%s path %s repositories = %v, want %v", item.id, path, read.evidence[path], wantEvidence[item.id][path])
					}
				}
				if read.authorized != authzAuthorized(want[item.id]) {
					t.Errorf("%s AuthorizationExpr = %v, want %v", item.id, read.authorized, authzAuthorized(want[item.id]))
				}
				executed++
			}

			status, err := readers.ReadWorkItemStatusWithScope(ctx, client, authzOrg, ids, scope, readers.Settings{})
			if err != nil {
				t.Fatalf("ReadWorkItemStatusWithScope() error = %v", err)
			}
			title, err := readers.ReadWorkItemTitleWithScope(ctx, client, authzOrg, ids, scope, readers.Settings{})
			if err != nil {
				t.Fatalf("ReadWorkItemTitleWithScope() error = %v", err)
			}
			completion, err := readers.ReadWorkItemCompletionWithScope(ctx, client, authzOrg, ids, readers.TimeBound{}, scope, readers.Settings{})
			if err != nil {
				t.Fatalf("ReadWorkItemCompletionWithScope() error = %v", err)
			}
			for name, got := range map[string][]string{
				"status":     statusIDs(status),
				"title":      titleIDs(title),
				"completion": completionIDs(completion),
			} {
				if strings.Join(got, ",") != strings.Join(wantAuthorized, ",") {
					t.Errorf("%s reader returned %v, want %v", name, got, wantAuthorized)
				}
			}
		})
	}
	if want := len(cases) * len(items); executed != want {
		t.Fatalf("executed %d cells, want %d", executed, want)
	}
}

func statusIDs(rows []readers.WorkItemStatusRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	sort.Strings(ids)
	return ids
}

func titleIDs(rows []readers.WorkItemTitleRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	sort.Strings(ids)
	return ids
}

func completionIDs(rows []readers.WorkItemCompletionRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	sort.Strings(ids)
	return ids
}
