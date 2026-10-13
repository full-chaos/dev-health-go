//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/readers"
	"github.com/full-chaos/dev-health-go/schema"
)

const mib = 1 << 20

// memorySeed seeds one organization of n work items: half repo-less, the
// given number of projects owned by 100 teams over 300 repositories, 1.5
// links per item across every provenance, and one team attribution per item.
func memorySeed(org string, n, projects int) []string {
	repoID := func(expr string) string {
		return "toUUID(UUIDNumToString(MD5(concat('" + org + "repo', toString(" + expr + ")))))"
	}
	itemID := func(expr string) string {
		return "concat('linear:', lower(hex(MD5(concat('" + org + "', toString(" + expr + "))))))"
	}
	repoOrNone := "if(number % 2 = 0, toUUID('00000000-0000-0000-0000-000000000000'), " + repoID("number % 300") + ")"
	return []string{
		"INSERT INTO repos (id, repo, created_at, last_synced, org_id, provider) SELECT " + repoID("number") + ", if(number % 3 = 0, concat('other/repo-', toString(number)), concat('acme/repo-', toString(number))), now(), now(), '" + org + "', 'github' FROM numbers(300)",
		"INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) SELECT concat('proj-', toString(number)), '" + org + "', 'linear', if(number % 10 = 0, concat('KEY', toString(number)), NULL), 'p', 1, 'started', '', now() FROM numbers(" + fmt.Sprint(projects) + ")",
		"INSERT INTO team_project_ownership (org_id, provider, team_id, project_id, project_key, source, valid_from, valid_to, updated_at) SELECT '" + org + "', 'linear', concat('team-', toString(number % 100)), concat('proj-', toString(number)), NULL, 'native', '2026-01-01 00:00:00', NULL, now() FROM numbers(" + fmt.Sprint(projects) + ")",
		"INSERT INTO team_repo_ownership (org_id, provider, team_id, repo_id, repo_full_name, match_type, source, is_primary, specificity, priority, valid_from, valid_to, updated_at) SELECT '" + org + "', 'github', concat('team-', toString(intDiv(number, 3))), " + repoID("number") + ", '', 'exact', 'inferred', 0, 0, 0, '2026-01-01 00:00:00', NULL, now() FROM numbers(300)",
		fmt.Sprintf("INSERT INTO work_items (repo_id, work_item_id, provider, title, type, status, project_key, project_id, native_team_key, project_name, created_at, updated_at, completed_at, parent_id, url, last_synced, org_id) SELECT %s, %s, 'linear', 't', 'issue', 'open', '', if(number %% 10 = 0, concat('KEY', toString(number %% %[5]d - number %% 10)), concat('proj-', toString(number %% %[5]d))), '', '', now(), now(), NULL, '', '', now(), '%[3]s' FROM numbers(%[4]d)", repoOrNone, itemID("number"), org, n, projects),
		fmt.Sprintf("INSERT INTO work_graph_issue_pr (repo_id, work_item_id, pr_number, confidence, provenance, evidence, last_synced, org_id) SELECT %s, %s, toUInt32(number), 1, multiIf(number %% 20 < 12, 'native', number %% 20 < 17, 'explicit_text', 'heuristic'), 'e', now(), '%s' FROM numbers(%d)", repoID("number % 297"), itemID(fmt.Sprintf("number %% %d", n)), org, n*3/2),
		fmt.Sprintf("INSERT INTO work_item_team_attributions (org_id, repo_id, work_item_id, team_id, team_name, source, is_primary, confidence, computed_at) SELECT '%s', %s, %s, concat('team-', toString(number %% 100)), 'n', 'project_ownership', 1, 'high', now() FROM numbers(%d)", org, repoOrNone, itemID("number"), n),
	}
}

const memoryTeamCandidates = "SELECT work_item_id FROM work_item_team_attributions WHERE org_id = {org_id:String} AND team_id IN {team_ids:Array(String)}"

// memoryStatement is a team-scoped work-item selection: the statement's own
// rows are one team's, so whatever grows with the organization is the
// authorization relation.
func memoryStatement(rendered readers.WorkItemScopeSQLResult) string {
	return "SELECT count(), sum(ok) FROM (SELECT w.work_item_id, any(toUInt8(" + rendered.AuthorizationExpr + ")) AS ok " +
		"FROM work_item_team_attributions AS a FINAL " +
		"INNER JOIN (SELECT work_item_id, repo_id, org_id, provider, project_id FROM work_items FINAL WHERE org_id = {org_id:String} AND work_item_id IN (" + memoryTeamCandidates + ")) AS w " +
		"ON w.work_item_id = a.work_item_id AND w.repo_id = a.repo_id AND w.org_id = a.org_id " + rendered.JoinSQL +
		" WHERE a.org_id = {org_id:String} AND a.team_id IN {team_ids:Array(String)} GROUP BY w.work_item_id) SETTINGS max_threads = 2"
}

func memoryParameter(value any) string {
	switch v := value.(type) {
	case []string:
		quoted := make([]string, len(v))
		for i, s := range v {
			quoted[i] = "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
		}
		return "[" + strings.Join(quoted, ",") + "]"
	default:
		return fmt.Sprint(v)
	}
}

// TestIntegrationWorkItemCandidatesMemory measures the peak memory of a
// team-scoped statement over a 30k and a 300k work-item organization: with
// no authorization relation, with the unnarrowed one, and with candidates.
// With candidates the relation's share stays under 32 MiB and does not grow
// with the organization; unnarrowed it does, which shows the measurement
// sees organization size at all.
func TestIntegrationWorkItemCandidatesMemory(t *testing.T) {
	required := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1"
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" || os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required when reader integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required for the memory test")
	}
	options, err := clickhousedriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN: %v", err)
	}
	ctx := context.Background()
	admin, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	database := fmt.Sprintf("authz_memory_%d", rand.Uint64())
	if err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatalf("create %s: %v", database, err)
	}
	t.Cleanup(func() {
		_ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database)
		_ = admin.Close()
	})
	options.Auth.Database = database
	conn, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	type orgSize struct{ items, projects int }
	sizes := map[string]orgSize{"org-30k": {30000, 1000}, "org-300k": {300000, 1000}, "org-300k-5kprojects": {300000, 5000}}
	stmts := schema.DDL("projects", "repos", "team_project_ownership", "team_repo_ownership", "work_graph_issue_pr", "work_items", "work_item_team_attributions")
	for org, size := range sizes {
		stmts = append(stmts, memorySeed(org, size.items, size.projects)...)
	}
	stmts = append(stmts, "OPTIMIZE TABLE work_items FINAL", "OPTIMIZE TABLE work_graph_issue_pr FINAL")
	for _, stmt := range stmts {
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed statement %q: %v", stmt, err)
		}
	}

	scope := readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: readers.RepositorySelectorSet{Owners: []string{"acme/*"}}}}
	team := []string{"team-7"}
	run := func(org, mode string, rendered readers.WorkItemScopeSQLResult) (uint64, uint64, string) {
		t.Helper()
		params := map[string]string{"org_id": org, "team_ids": memoryParameter(team)}
		for _, b := range rendered.Bindings {
			params[b.Name] = memoryParameter(b.Value)
		}
		queryID := fmt.Sprintf("%s-%s-%s", database, org, mode)
		qctx := clickhousedriver.Context(ctx, clickhousedriver.WithQueryID(queryID), clickhousedriver.WithParameters(params))
		var count, admitted uint64
		if err := conn.QueryRow(qctx, memoryStatement(rendered)).Scan(&count, &admitted); err != nil {
			t.Fatalf("%s %s: %v", org, mode, err)
		}
		return count, admitted, queryID
	}

	peaks := map[string]int64{}
	var queryIDs []string
	for org := range sizes {
		baseCount, _, baseID := run(org, "baseline", readers.WorkItemScopeSQLResult{AuthorizationExpr: "1"})
		oldCount, oldAdmitted, oldID := run(org, "unnarrowed", readers.WorkItemScopeSQL(scope))
		newCount, newAdmitted, newID := run(org, "candidates", readers.WorkItemScopeSQLAmong(scope, readers.WorkItemCandidateSubquery(memoryTeamCandidates, readers.Binding{Name: "team_ids", Value: team})))
		if oldCount == 0 || oldAdmitted == 0 || oldAdmitted == oldCount || baseCount != oldCount {
			t.Fatalf("%s: %d rows (baseline %d), %d admitted: the seed must hold admitted and denied rows", org, oldCount, baseCount, oldAdmitted)
		}
		if newCount != oldCount || newAdmitted != oldAdmitted {
			t.Errorf("%s: candidates read %d rows, %d admitted; unnarrowed read %d, %d", org, newCount, newAdmitted, oldCount, oldAdmitted)
		}
		queryIDs = append(queryIDs, baseID, oldID, newID)
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	rows, err := conn.Query(ctx, "SELECT query_id, memory_usage FROM system.query_log WHERE type = 'QueryFinish' AND query_id IN ?", queryIDs)
	if err != nil {
		t.Fatalf("read query_log: %v", err)
	}
	for rows.Next() {
		var id string
		var peak uint64
		if err := rows.Scan(&id, &peak); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		peaks[strings.TrimPrefix(id, database+"-")] = int64(peak)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query_log rows: %v", err)
	}
	for _, id := range queryIDs {
		if _, ok := peaks[strings.TrimPrefix(id, database+"-")]; !ok {
			t.Fatalf("no query_log row for %s: the peak was never measured", id)
		}
	}
	for key, peak := range peaks {
		t.Logf("%s peak %.2f MiB", key, float64(peak)/mib)
	}

	// The authorization relation's share of a peak is the statement's peak
	// less the same statement's peak with no authorization relation.
	share := func(org, mode string) int64 { return peaks[org+"-"+mode] - peaks[org+"-baseline"] }
	if grew := share("org-300k", "unnarrowed") - share("org-30k", "unnarrowed"); grew < 32*mib {
		t.Fatalf("unnarrowed relation grew %.2f MiB from 30k to 300k work items, want at least 32: the measurement does not see organization size", float64(grew)/mib)
	}
	if got := share("org-300k", "candidates"); got >= 32*mib {
		t.Errorf("candidates relation at 300k = %.2f MiB, want < 32", float64(got)/mib)
	}
	if grew := share("org-300k", "candidates") - share("org-30k", "candidates"); grew > 8*mib {
		t.Errorf("candidates relation grew %.2f MiB from 30k to 300k work items, want at most 8", float64(grew)/mib)
	}
	// The project side reads the whole project catalog (its key-ambiguity
	// count is organization-wide), so it grows with the catalog, not with
	// work items. Five times the projects stays under the same bound.
	if got := share("org-300k-5kprojects", "candidates"); got >= 32*mib {
		t.Errorf("candidates relation at 300k work items and 5k projects = %.2f MiB, want < 32", float64(got)/mib)
	}
	t.Logf("candidates relation: %.2f MiB with 1k projects, %.2f MiB with 5k projects", float64(share("org-300k", "candidates"))/mib, float64(share("org-300k-5kprojects", "candidates"))/mib)
}
