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
//
// ttmClient builds a fresh database with the team theme mix tables, runs the
// seed statements and returns a query client on it.
func ttmClient(t *testing.T, prefix string, seedStatements []string) *clickhouse.Client {
	t.Helper()
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
	database := fmt.Sprintf("%s_%d", prefix, rand.Uint64())
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
	for _, stmt := range append(schema.DDL("work_unit_investments", "repos", "work_item_team_attributions"), seedStatements...) {
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
	return client
}

func ttmAttribution(org, repo, item, team string) string {
	return fmt.Sprintf("INSERT INTO work_item_team_attributions (org_id, repo_id, work_item_id, team_id, team_name, source, is_primary, confidence, computed_at) VALUES ('%s', '%s', '%s', '%s', '%s', 'linked_issue', 1, 'high', '2026-03-01 00:00:00')", org, repo, item, team, team)
}

func ttmUnit(org, id, item string, effort float64, theme string) string {
	return fmt.Sprintf(`INSERT INTO work_unit_investments (work_unit_id, from_ts, to_ts, effort_value, theme_distribution_json, subcategory_distribution_json, structural_evidence_json, computed_at, org_id) VALUES ('%s', '2026-02-01 00:00:00', '2026-04-01 00:00:00', %g, map('%s', 1), map(), '{"issues":["%s"],"prs":[]}', '2026-03-01 00:00:00', '%s')`, id, effort, theme, item, org)
}

func TestIntegrationReadTeamThemeMixItemIDUnderTwoRepositories(t *testing.T) {
	const org = "ttm-dup-org"
	const repoA, repoB = "33333333-3333-4333-8333-000000000001", "33333333-3333-4333-8333-000000000002"
	client := ttmClient(t, "ttm_dup", []string{
		// Same team under both repositories.
		ttmAttribution(org, repoA, "linear:SAME-1", "team-same"), ttmAttribution(org, repoB, "linear:SAME-1", "team-same"),
		ttmUnit(org, "u-same", "linear:SAME-1", 10, "risk"),
		// Two different teams under the two repositories.
		ttmAttribution(org, repoA, "linear:SPLIT-1", "team-a"), ttmAttribution(org, repoB, "linear:SPLIT-1", "team-b"),
		ttmUnit(org, "u-split", "linear:SPLIT-1", 10, "risk"),
	})
	rows, err := readers.ReadTeamThemeMix(context.Background(), client, org, []string{"team-same", "team-a", "team-b"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadTeamThemeMix() error = %v", err)
	}
	risk := map[string]float64{}
	for _, row := range rows {
		if row.Kind == "theme" && row.Key == "risk" {
			risk[row.TeamID] += row.WorkUnits
		}
	}
	if risk["team-same"] != 1 {
		t.Errorf("same team under two repositories = %v, want the unit counted once (1)", risk["team-same"])
	}
	if risk["team-b"] != 1 || risk["team-a"] != 0 {
		t.Errorf("two teams = a:%v b:%v, want the whole unit on the larger team id (b:1, a:0)", risk["team-a"], risk["team-b"])
	}
}

// The team mix is a work-unit count: the stored effort_value (lines changed
// by default) is never read. Two teams hold one feature delivery unit and one
// risk unit each; one team's risk unit carries a stored effort of a billion.
// Both teams must read the same counts, so the planted unit cannot move a
// share, and a unit with zero stored effort still counts once.
func TestIntegrationReadTeamThemeMixIgnoresStoredEffortValue(t *testing.T) {
	const org = "ttm-effort-org"
	const repo = "33333333-3333-4333-8333-000000000003"
	client := ttmClient(t, "ttm_effort", []string{
		ttmAttribution(org, repo, "linear:S-1", "team-small"), ttmAttribution(org, repo, "linear:S-2", "team-small"), ttmAttribution(org, repo, "linear:S-3", "team-small"),
		ttmAttribution(org, repo, "linear:B-1", "team-big"), ttmAttribution(org, repo, "linear:B-2", "team-big"), ttmAttribution(org, repo, "linear:B-3", "team-big"),
		ttmUnit(org, "u-s-feature", "linear:S-1", 1, "feature_delivery"),
		ttmUnit(org, "u-s-risk", "linear:S-2", 1, "risk"),
		ttmUnit(org, "u-s-unsized", "linear:S-3", 0, "operational"),
		ttmUnit(org, "u-b-feature", "linear:B-1", 1, "feature_delivery"),
		// The planted large-LOC unit: still one work unit.
		ttmUnit(org, "u-b-risk", "linear:B-2", 1e9, "risk"),
		ttmUnit(org, "u-b-unsized", "linear:B-3", -5, "operational"),
	})
	rows, err := readers.ReadTeamThemeMix(context.Background(), client, org, []string{"team-small", "team-big"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadTeamThemeMix() error = %v", err)
	}
	got := map[string]map[string]float64{"team-small": {}, "team-big": {}}
	for _, row := range rows {
		if row.Kind == "theme" {
			got[row.TeamID][row.Key] += row.WorkUnits
		}
	}
	for team, themes := range got {
		for theme, want := range map[string]float64{"feature_delivery": 1, "risk": 1, "operational": 1} {
			if themes[theme] != want {
				t.Errorf("%s %s = %v, want %v whatever each unit's stored effort", team, theme, themes[theme], want)
			}
		}
	}
}
