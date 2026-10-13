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

func TestIntegrationCandidatesReadOnceDiag(t *testing.T) {
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

	stmts := schema.DDL("projects", "repos", "team_project_ownership", "team_repo_ownership", "work_graph_issue_pr", "work_items", "work_item_team_attributions")
	stmts = append(stmts, memorySeed("org-300k", 300000, 1000)...)
	stmts = append(stmts, "OPTIMIZE TABLE work_items FINAL", "OPTIMIZE TABLE work_graph_issue_pr FINAL")
	for _, s := range stmts {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	scope := readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: readers.RepositorySelectorSet{Owners: []string{"acme/*"}}}}
	teams := []string{"team-7", "team-8", "team-9", "team-10", "team-11", "team-12", "team-13"}
	scalar := "SELECT arrayJoin((SELECT groupArray(work_item_id) FROM (" + memoryTeamCandidates + ")))"
	type variant struct {
		name   string
		render func() readers.WorkItemScopeSQLResult
	}
	bind := func(sql string) readers.WorkItemScopeSQLResult {
		return readers.WorkItemScopeSQLAmong(scope, readers.WorkItemCandidateSubquery(sql,
			readers.Binding{Name: "team_ids", Value: teams}, readers.Binding{Name: "cand_mod", Value: 1}))
	}
	variants := []variant{
		{"base", func() readers.WorkItemScopeSQLResult { return readers.WorkItemScopeSQLResult{AuthorizationExpr: "1"} }},
		{"current", func() readers.WorkItemScopeSQLResult { return bind(memoryTeamCandidates) }},
		{"scalar", func() readers.WorkItemScopeSQLResult { return bind(scalar) }},
	}
	var ids []string
	byID := map[string]string{}
	for _, v := range variants {
		rendered := v.render()
		for rep := 0; rep < 5; rep++ {
			params := map[string]string{"org_id": "org-300k", "team_ids": memoryParameter(teams), "cand_mod": "1"}
			for _, b := range rendered.Bindings {
				params[b.Name] = memoryParameter(b.Value)
			}
			id := fmt.Sprintf("%s-%s-%d", database, v.name, rep)
			qctx := clickhousedriver.Context(ctx, clickhousedriver.WithQueryID(id), clickhousedriver.WithParameters(params))
			var count, admitted uint64
			if err := conn.QueryRow(qctx, memoryStatement(rendered)).Scan(&count, &admitted); err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			ids = append(ids, id)
			byID[id] = v.name
		}
	}
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "SELECT query_id, memory_usage, read_rows, ProfileEvents['ScalarSubqueriesCacheMiss'], ProfileEvents['ScalarSubqueriesGlobalCacheHit'], ProfileEvents['ScalarSubqueriesLocalCacheHit'] FROM system.query_log WHERE type = 'QueryFinish' AND query_id IN ?", ids)
	if err != nil {
		t.Fatal(err)
	}
	type agg struct{ minPeak, readRows, miss, ghit, lhit uint64 }
	res := map[string]*agg{}
	for rows.Next() {
		var id string
		var peak, rr, miss, gh, lh uint64
		if err := rows.Scan(&id, &peak, &rr, &miss, &gh, &lh); err != nil {
			t.Fatal(err)
		}
		a := res[byID[id]]
		if a == nil {
			a = &agg{minPeak: peak}
			res[byID[id]] = a
		}
		if peak < a.minPeak {
			a.minPeak = peak
		}
		a.readRows, a.miss, a.ghit, a.lhit = rr, miss, gh, lh
	}
	names := []string{}
	for k := range res {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		a := res[k]
		t.Logf("DIAG %-8s peak %7.2f MiB read_rows %d scalarMiss %d globalHit %d localHit %d", k, float64(a.minPeak)/mib, a.readRows, a.miss, a.ghit, a.lhit)
	}
	_ = strings.TrimSpace
}
