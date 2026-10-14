//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
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

const memoryTeamCandidates = "SELECT work_item_id FROM work_item_team_attributions WHERE org_id = {org_id:String} AND team_id IN {team_ids:Array(String)} AND cityHash64(work_item_id) % {cand_mod:UInt64} = 0"

// biggestTeamCandidates is the candidate count of the largest team the
// relation must serve. The seed spreads work items over 100 teams, so a
// 300k-item organization gives each team 3000 candidates.
const biggestTeamCandidates = 20000

const (
	memoryWarmRuns          = 3
	memorySlopeRatio        = 1.15
	memoryBytesPerCandidate = 4096
	memoryRelationCap       = 32 * mib
)

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
// team-scoped statement with no authorization relation, with the unnarrowed
// one, and with candidates. It separates two inputs: organization size at a
// fixed candidate count must not grow the relation (min of three warm runs,
// slope rule), and the candidate count may, within the cap, because a bigger
// team is a real input. Unnarrowed, the relation grows with organization
// size, which shows the measurement sees it at all.
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
	sizes := map[string]orgSize{"org-30k": {30000, 1000}, "org-300k": {300000, 1000}, "org-300k-5kprojects": {300000, 5000}, "org-300k-20kprojects": {300000, 20000}}
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
	teamsFor := func(n int) []string {
		teams := make([]string, 0, n)
		for i := 0; i < n; i++ {
			teams = append(teams, fmt.Sprintf("team-%d", 7+i))
		}
		return teams
	}
	// A scenario fixes the organization, the teams and the share of their
	// work items kept as candidates, so the organization size and the
	// candidate count move independently.
	type scenario struct {
		org   string
		teams []string
		mod   int
	}
	scenarios := map[string]scenario{
		"org-30k":              {"org-30k", teamsFor(1), 1},
		"org-300k":             {"org-300k", teamsFor(1), 1},
		"org-300k-equal":       {"org-300k", teamsFor(1), 10},
		"org-300k-5kprojects":  {"org-300k-5kprojects", teamsFor(1), 1},
		"org-300k-20kprojects": {"org-300k-20kprojects", teamsFor(1), 1},
		"org-300k-biggest":     {"org-300k", teamsFor((biggestTeamCandidates + 2999) / 3000), 1},
	}
	scenarioNames := make([]string, 0, len(scenarios))
	for name := range scenarios {
		scenarioNames = append(scenarioNames, name)
	}
	sort.Strings(scenarioNames)

	candidateCount := map[string]int64{}
	for name, sc := range scenarios {
		params := map[string]string{"org_id": sc.org, "team_ids": memoryParameter(sc.teams), "cand_mod": fmt.Sprint(sc.mod)}
		qctx := clickhousedriver.Context(ctx, clickhousedriver.WithParameters(params))
		var n uint64
		if err := conn.QueryRow(qctx, "SELECT count() FROM ("+memoryTeamCandidates+")").Scan(&n); err != nil {
			t.Fatalf("count candidates %s: %v", name, err)
		}
		candidateCount[name] = int64(n)
		t.Logf("scenario %s: %d candidates", name, n)
	}

	run := func(name, mode string, rep int, rendered readers.WorkItemScopeSQLResult) (uint64, uint64, string) {
		t.Helper()
		sc := scenarios[name]
		params := map[string]string{"org_id": sc.org, "team_ids": memoryParameter(sc.teams), "cand_mod": fmt.Sprint(sc.mod)}
		for _, b := range rendered.Bindings {
			params[b.Name] = memoryParameter(b.Value)
		}
		queryID := fmt.Sprintf("%s-%s-%s-%d", database, name, mode, rep)
		qctx := clickhousedriver.Context(ctx, clickhousedriver.WithQueryID(queryID), clickhousedriver.WithParameters(params))
		var count, admitted uint64
		if err := conn.QueryRow(qctx, memoryStatement(rendered)).Scan(&count, &admitted); err != nil {
			t.Fatalf("%s %s: %v", name, mode, err)
		}
		return count, admitted, queryID
	}

	// Each statement runs memoryWarmRuns times and the smallest peak counts:
	// one peak of a short multi-threaded query carries several MiB of
	// scheduling noise on either ClickHouse version.
	runPeaks := map[string]int64{}
	var queryIDs []string
	for _, name := range scenarioNames {
		sc := scenarios[name]
		for rep := 0; rep < memoryWarmRuns; rep++ {
			baseCount, _, baseID := run(name, "baseline", rep, readers.WorkItemScopeSQLResult{AuthorizationExpr: "1"})
			oldCount, oldAdmitted, oldID := run(name, "unnarrowed", rep, readers.WorkItemScopeSQL(scope))
			newCount, newAdmitted, newID := run(name, "candidates", rep, readers.WorkItemScopeSQLAmong(scope, readers.WorkItemCandidateSubquery(memoryTeamCandidates,
				readers.Binding{Name: "team_ids", Value: sc.teams}, readers.Binding{Name: "cand_mod", Value: sc.mod})))
			if oldCount == 0 || oldAdmitted == 0 || oldAdmitted == oldCount || baseCount != oldCount {
				t.Fatalf("%s: %d rows (baseline %d), %d admitted: the seed must hold admitted and denied rows", name, oldCount, baseCount, oldAdmitted)
			}
			if newCount != oldCount || newAdmitted != oldAdmitted {
				t.Errorf("%s: candidates read %d rows, %d admitted; unnarrowed read %d, %d", name, newCount, newAdmitted, oldCount, oldAdmitted)
			}
			queryIDs = append(queryIDs, baseID, oldID, newID)
		}
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	rows, err := conn.Query(ctx, "SELECT query_id, memory_usage FROM system.query_log WHERE type = 'QueryFinish' AND query_id IN ?", queryIDs)
	if err != nil {
		t.Fatalf("read query_log: %v", err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var peak uint64
		if err := rows.Scan(&id, &peak); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		seen[id] = true
		t.Logf("run %s peak %.2f MiB", strings.TrimPrefix(id, database+"-"), float64(peak)/mib)
		runPeaks[strings.TrimPrefix(id, database+"-")] = int64(peak)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query_log rows: %v", err)
	}
	for _, id := range queryIDs {
		if !seen[id] {
			t.Fatalf("no query_log row for %s: the peak was never measured", id)
		}
	}
	// The relation's share of a run is that run's peak less the peak of the
	// no-relation statement run right beside it; the share of a scenario is
	// the smallest over its warm runs, so a noisy baseline on one run cannot
	// stand in for the whole scenario.
	share := func(name, mode string) int64 {
		best := int64(-1)
		for rep := 0; rep < memoryWarmRuns; rep++ {
			delta := runPeaks[fmt.Sprintf("%s-%s-%d", name, mode, rep)] - runPeaks[fmt.Sprintf("%s-baseline-%d", name, rep)]
			if best < 0 || delta < best {
				best = delta
			}
		}
		return best
	}
	if grew := share("org-300k", "unnarrowed") - share("org-30k", "unnarrowed"); grew < 32*mib {
		t.Fatalf("unnarrowed relation grew %.2f MiB from 30k to 300k work items, want at least 32: the measurement does not see organization size", float64(grew)/mib)
	}

	// Organization size alone: the same number of candidates in a 30k and a
	// 300k organization.
	if a, b := candidateCount["org-30k"], candidateCount["org-300k-equal"]; a*10 < b*9 || b*10 < a*9 {
		t.Fatalf("equal-candidate scenarios hold %d and %d candidates, want within 10%%", a, b)
	}
	// The slope is read on the candidates statement itself: the smallest of
	// the warm reads per size, and the floor is that statement's own spread
	// (largest less smallest warm read, both sizes, rounded up to a whole
	// MiB), so the row fails only on growth the repeat noise cannot explain.
	warmReads := func(name, mode string) (smallest, spread int64) {
		var lo, hi int64 = -1, 0
		for rep := 0; rep < memoryWarmRuns; rep++ {
			peak := runPeaks[fmt.Sprintf("%s-%s-%d", name, mode, rep)]
			if lo < 0 || peak < lo {
				lo = peak
			}
			if peak > hi {
				hi = peak
			}
		}
		return lo, hi - lo
	}
	small, smallSpread := warmReads("org-30k", "candidates")
	large, largeSpread := warmReads("org-300k-equal", "candidates")
	floor := smallSpread
	if largeSpread > floor {
		floor = largeSpread
	}
	floor = (floor + mib - 1) / mib * mib
	t.Logf("organization-size slope at %d candidates: %.2f MiB at 30k (spread %.2f), %.2f MiB at 300k (spread %.2f), floor %d MiB",
		candidateCount["org-30k"], float64(small)/mib, float64(smallSpread)/mib, float64(large)/mib, float64(largeSpread)/mib, floor/mib)
	if float64(large) > memorySlopeRatio*float64(small) && large > small+floor {
		t.Errorf("candidates statement grew with organization size at %d candidates: %.2f MiB at 30k, %.2f MiB at 300k work items, want at most x%.2f or +%d MiB (its own spread)",
			candidateCount["org-30k"], float64(small)/mib, float64(large)/mib, memorySlopeRatio, floor/mib)
	}

	// Candidate count: a bigger team is a real input, so the relation may
	// grow with it, but the largest team still fits under the cap.
	if got := share("org-300k", "candidates"); got >= memoryRelationCap {
		t.Errorf("candidates relation at 300k = %.2f MiB, want < 32", float64(got)/mib)
	}
	biggest := share("org-300k-biggest", "candidates")
	if biggest >= memoryRelationCap {
		t.Errorf("candidates relation for %d candidates = %.2f MiB, want < 32", candidateCount["org-300k-biggest"], float64(biggest)/mib)
	}
	if extra := candidateCount["org-300k-biggest"] - candidateCount["org-300k"]; extra > 0 {
		perCandidate := float64(biggest-share("org-300k", "candidates")) / float64(extra)
		if perCandidate > memoryBytesPerCandidate {
			t.Errorf("candidates relation grows %.0f bytes per extra candidate (%d to %d candidates), want at most %d",
				perCandidate, candidateCount["org-300k"], candidateCount["org-300k-biggest"], memoryBytesPerCandidate)
		}
	}
	// The project side reads the whole project catalog (its key-ambiguity
	// count is organization-wide), so it grows with the catalog, not with
	// work items. Five times the projects stays under the same bound.
	if got := share("org-300k-5kprojects", "candidates"); got >= memoryRelationCap {
		t.Errorf("candidates relation at 300k work items and 5k projects = %.2f MiB, want < 32", float64(got)/mib)
	}
	t.Logf("candidates relation: %.2f MiB at %d candidates (30k), %.2f MiB at %d (300k, equal), %.2f MiB at %d (300k), %.2f MiB at %d (300k, biggest team)",
		float64(small)/mib, candidateCount["org-30k"], float64(large)/mib, candidateCount["org-300k-equal"],
		float64(share("org-300k", "candidates"))/mib, candidateCount["org-300k"], float64(biggest)/mib, candidateCount["org-300k-biggest"])
	t.Logf("candidates relation at 300k work items: %.2f MiB with 1k projects, %.2f MiB with 5k, %.2f MiB with 20k (logged, not bounded)",
		float64(share("org-300k", "candidates"))/mib, float64(share("org-300k-5kprojects", "candidates"))/mib, float64(share("org-300k-20kprojects", "candidates"))/mib)
}
