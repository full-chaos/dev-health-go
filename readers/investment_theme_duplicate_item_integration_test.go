//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/clickhouse"
	"github.com/full-chaos/dev-health-go/readers"
	"github.com/full-chaos/dev-health-go/schema"
)

// ReadTeamThemeMix resolves an evidence ref to a team through
// work_item_team_attributions keyed on work_item_id alone. These cells pin
// what it does when one item id is attributed under two repositories, so the
// behaviour is a named, executed limit rather than an assumption:
//
//   - the same team under both repositories: the work unit is counted once;
//   - two different teams: the work unit goes to the team with the larger
//     team_id (the reader's documented tie-break), never to both and never
//     to neither. Unlike ReadProjectThemeMix this reader does not fail closed
//     on the collision, because its result is the ops Investment view's
//     majority vote and must not diverge from it.
func TestIntegrationReadTeamThemeMixItemIDUnderTwoRepositories(t *testing.T) {
	required := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1"
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" || os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required when reader integration is mandatory")
		}
		t.Skip("integration DSN not configured")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("ttm_dup_%d", rand.Uint64())
	options, err := clickhousedriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database)
		_ = admin.Close()
	})
	options.Auth.Database = database
	seed, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	const org = "ttm-dup-org"
	const at = "'2026-03-01 00:00:00'"
	stmts := schema.DDL("work_unit_investments", "repos", "work_item_team_attributions")
	attribution := func(repo, item, team string) string {
		return fmt.Sprintf("INSERT INTO work_item_team_attributions (org_id, repo_id, work_item_id, team_id, team_name, source, is_primary, confidence, computed_at) VALUES ('%s', '%s', '%s', '%s', '%s', 'linked_issue', 1, 'high', %s)", org, repo, item, team, team, at)
	}
	const repoA, repoB = "33333333-3333-4333-8333-000000000001", "33333333-3333-4333-8333-000000000002"
	unit := func(id, item string) string {
		return fmt.Sprintf(`INSERT INTO work_unit_investments (work_unit_id, from_ts, to_ts, effort_value, theme_distribution_json, subcategory_distribution_json, structural_evidence_json, computed_at, org_id) VALUES ('%s', '2026-02-01 00:00:00', '2026-04-01 00:00:00', 10, map('risk', 1), map(), '{"issues":["%s"],"prs":[]}', %s, '%s')`, id, item, at, org)
	}
	stmts = append(stmts,
		// Same team under both repositories.
		attribution(repoA, "linear:SAME-1", "team-same"), attribution(repoB, "linear:SAME-1", "team-same"),
		unit("u-same", "linear:SAME-1"),
		// Two different teams under the two repositories.
		attribution(repoA, "linear:SPLIT-1", "team-a"), attribution(repoB, "linear:SPLIT-1", "team-b"),
		unit("u-split", "linear:SPLIT-1"),
	)
	for _, stmt := range stmts {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	parsed.Path = "/" + database
	client, err := clickhouse.NewClickHouseQueryClientWithOptions(clickhouse.Options{DSN: parsed.String(), QueryTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	rows, err := readers.ReadTeamThemeMix(ctx, client, org, []string{"team-same", "team-a", "team-b"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadTeamThemeMix() error = %v", err)
	}
	risk := map[string]float64{}
	for _, row := range rows {
		if row.Kind == "theme" && row.Key == "risk" {
			risk[row.TeamID] += row.WeightedEffort
		}
	}
	if risk["team-same"] != 10 {
		t.Errorf("same team under two repositories = %v, want the unit counted once (10)", risk["team-same"])
	}
	if risk["team-b"] != 10 || risk["team-a"] != 0 {
		t.Errorf("two teams = a:%v b:%v, want the whole unit on the larger team id (b:10, a:0)", risk["team-a"], risk["team-b"])
	}
}
