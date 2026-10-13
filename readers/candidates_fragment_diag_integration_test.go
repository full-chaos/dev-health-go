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

func TestIntegrationCandidatesFragmentDiag(t *testing.T) {
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" {
		t.Skip("no dsn")
	}
	options, err := clickhousedriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("authz_diag_%d", rand.Uint64())
	if err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database); _ = admin.Close() })
	options.Auth.Database = database
	conn, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var version string
	if err := conn.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("DIAG version %s", version)

	sizes := map[string]int{"org-30k": 30000, "org-300k": 300000}
	stmts := schema.DDL("projects", "repos", "team_project_ownership", "team_repo_ownership", "work_graph_issue_pr", "work_items", "work_item_team_attributions")
	for org, n := range sizes {
		stmts = append(stmts, memorySeed(org, n, 1000)...)
	}
	stmts = append(stmts, "OPTIMIZE TABLE work_items FINAL", "OPTIMIZE TABLE work_graph_issue_pr FINAL")
	for _, s := range stmts {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	scope := readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: readers.RepositorySelectorSet{Owners: []string{"acme/*"}}}}
	team := []string{"team-7"}
	rendered := readers.WorkItemScopeSQLAmong(scope, readers.WorkItemCandidateSubquery(memoryTeamCandidates, readers.Binding{Name: "team_ids", Value: team}))
	join := rendered.JoinSQL
	pIdx := strings.Index(join, "LEFT JOIN (SELECT wia_scope")
	lIdx := strings.Index(join, "LEFT JOIN (SELECT wia_link.work_item_id")
	if pIdx < 0 || lIdx < pIdx {
		t.Fatalf("cannot split join: %d %d", pIdx, lIdx)
	}
	j0, jp, jl := join[:pIdx], join[pIdx:lIdx], join[lIdx:]

	current := readers.OwnershipValidityPredicate(readers.TimeBound{})
	candidateScopes := "(SELECT provider, project_id FROM work_items WHERE org_id = {org_id:String} AND work_item_id IN (" + memoryTeamCandidates + "))"
	statements := map[string]string{
		"00-candidate-ids":      "SELECT count() FROM (" + memoryTeamCandidates + ")",
		"01-candidate-scopes":   "SELECT count() FROM " + candidateScopes,
		"02-scopes-narrowed":    "SELECT count() FROM (SELECT provider, id, scope FROM " + readers.ProjectIdentityCatalogSQL() + " WHERE (provider, scope) IN " + candidateScopes + ")",
		"03-scopes-whole":       "SELECT count() FROM (SELECT provider, id, scope FROM " + readers.ProjectIdentityCatalogSQL() + ")",
		"04-ownedprojects-narr": "SELECT count() FROM (SELECT provider, id, team_id FROM " + readers.ProjectOwnershipCatalogJoinSQL(current) + " WHERE (provider, id) IN (SELECT provider, id FROM " + readers.ProjectIdentityCatalogSQL() + " WHERE (provider, scope) IN " + candidateScopes + "))",
		"05-ownedprojects-all":  "SELECT count() FROM (SELECT provider, id, team_id FROM " + readers.ProjectOwnershipCatalogJoinSQL(current) + ")",
		"06-teamrepo":           "SELECT count() FROM " + readers.TeamRepositoryOwnershipSQL(current) + " AS t",
		"10-stmt-repojoin-only": memoryStmtWith(j0, "1"),
		"11-stmt-plus-project":  memoryStmtWith(j0+" "+jp, "1"),
		"12-stmt-plus-link":     memoryStmtWith(j0+" "+jl, "1"),
		"13-stmt-all":           memoryStmtWith(join, "1"),
	}
	modCandidates := memoryTeamCandidates + " AND cityHash64(work_item_id) % {wia_mod:UInt64} = 0"
	modRendered := readers.WorkItemScopeSQLAmong(scope, readers.WorkItemCandidateSubquery(modCandidates, readers.Binding{Name: "team_ids", Value: team}, readers.Binding{Name: "mod_unused", Value: 1}))
	modStmt := func(joinSQL string) string {
		return strings.ReplaceAll(memoryStmtWith(joinSQL, "1"), memoryTeamCandidates, modCandidates)
	}
	mj := modRendered.JoinSQL
	mp := strings.Index(mj, "LEFT JOIN (SELECT wia_scope")
	ml := strings.Index(mj, "LEFT JOIN (SELECT wia_link.work_item_id")
	statements["20-eqcand-repojoin"] = modStmt(mj[:mp])
	statements["21-eqcand-project"] = modStmt(mj[:ml])
	statements["22-eqcand-link"] = modStmt(mj[:mp] + " " + mj[ml:])
	statements["23-eqcand-all"] = modStmt(mj)
	keys := make([]string, 0, len(statements))
	for k := range statements {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type result struct {
		key, org string
		peak     int64
	}
	var ids []string
	byID := map[string]string{}
	for _, org := range []string{"org-30k", "org-300k"} {
		for _, key := range keys {
			for rep := 0; rep < 3; rep++ {
				params := map[string]string{"org_id": org, "team_ids": memoryParameter(team), "wia_mod": "1"}
				if org == "org-300k" {
					params["wia_mod"] = "10"
				}
				for _, b := range rendered.Bindings {
					params[b.Name] = memoryParameter(b.Value)
				}
				id := fmt.Sprintf("%s-%s-%s-%d", database, org, key, rep)
				qctx := clickhousedriver.Context(ctx, clickhousedriver.WithQueryID(id), clickhousedriver.WithParameters(params))
				rows, err := conn.Query(qctx, statements[key])
				if err != nil {
					t.Fatalf("%s %s: %v", org, key, err)
				}
				for rows.Next() {
				}
				rows.Close()
				ids = append(ids, id)
				byID[id] = org + " " + key
			}
		}
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "SELECT query_id, memory_usage FROM system.query_log WHERE type = 'QueryFinish' AND query_id IN ?", ids)
	if err != nil {
		t.Fatal(err)
	}
	minPeak := map[string]int64{}
	for rows.Next() {
		var id string
		var peak uint64
		if err := rows.Scan(&id, &peak); err != nil {
			t.Fatal(err)
		}
		k := byID[id]
		if cur, ok := minPeak[k]; !ok || int64(peak) < cur {
			minPeak[k] = int64(peak)
		}
	}
	for _, key := range keys {
		small, large := minPeak["org-30k "+key], minPeak["org-300k "+key]
		t.Logf("DIAG %-24s 30k %7.2f MiB  300k %7.2f MiB  delta %+7.2f MiB", key, float64(small)/mib, float64(large)/mib, float64(large-small)/mib)
	}
}

func memoryStmtWith(joinSQL, expr string) string {
	return memoryStatement(readers.WorkItemScopeSQLResult{AuthorizationExpr: expr, JoinSQL: joinSQL})
}
