//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math"
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

// investment_metrics_daily is append-only and a team that owns several
// repositories writes one row per repository per run. A reader must take the
// newest row per (day, repo) and then add the repositories up. These cells run
// ReadTeamInvestment and ReadProjectInvestment against a real ClickHouse
// table built by schema.DDL (the ops DDL chain: 007 columns incl. repo_id,
// org_id from 024, sorting key from 027).

const fkOrg = "fk-org"

const fkNilRepo = "00000000-0000-0000-0000-000000000000"

type fkRow struct {
	repo     string // "" = repo_id NULL
	day      string
	at       string // computed_at
	units    int
	items    int
	cycle    float64
	areaName string // "" = "product"
	org      string // "" = fkOrg
	stream   string // "" = "growth"
}

type fkCase struct {
	name string
	rows []fkRow
	// want is nil when the team must produce no row at all.
	want *fkWant
}

type fkWant struct {
	day   string
	units int64
	items int64
	churn uint64
	prs   int64
	// cycle is the served CycleP50Hours; cycleKnown is CycleP50Known (0 is not a value when false).
	cycle      float64
	cycleKnown bool
}

// prs is checked as 2 * items: fkSeedInsert writes prs_merged = 2 * items.

func fkRepo(n int) string { return fmt.Sprintf("44444444-4444-4444-8444-%012d", n) }

func fkCases() []fkCase {
	const d = "2026-03-01"
	return []fkCase{
		{name: "two-repos-with-older-rows", rows: []fkRow{
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 99, items: 99, cycle: 1},
			{repo: fkRepo(1), day: d, at: "2026-03-02 11:00:00", units: 10, items: 10, cycle: 10},
			{repo: fkRepo(2), day: d, at: "2026-03-02 10:00:00", units: 88, items: 88, cycle: 1},
			{repo: fkRepo(2), day: d, at: "2026-03-02 11:00:00", units: 20, items: 20, cycle: 20},
		}, want: &fkWant{day: d, units: 30, items: 30, churn: 3000, cycleKnown: false}},
		{name: "three-repos", rows: []fkRow{
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 5, items: 5, cycle: 5},
			{repo: fkRepo(2), day: d, at: "2026-03-02 10:00:00", units: 6, items: 6, cycle: 6},
			{repo: fkRepo(3), day: d, at: "2026-03-02 10:00:00", units: 7, items: 7, cycle: 7},
			{repo: fkRepo(3), day: d, at: "2026-03-01 10:00:00", units: 70, items: 70, cycle: 70},
		}, want: &fkWant{day: d, units: 18, items: 18, churn: 1800}},
		{name: "one-repo-unchanged", rows: []fkRow{
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 99, items: 99, cycle: 1},
			{repo: fkRepo(1), day: d, at: "2026-03-02 11:00:00", units: 10, items: 10, cycle: 12.5},
		}, want: &fkWant{day: d, units: 10, items: 10, churn: 1000, cycle: 12.5, cycleKnown: true}},
		{name: "newest-zero-never-falls-back", rows: []fkRow{
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 7, items: 7, cycle: 7},
			{repo: fkRepo(1), day: d, at: "2026-03-02 11:00:00", units: 0, items: 0, cycle: 0},
			{repo: fkRepo(2), day: d, at: "2026-03-02 11:00:00", units: 5, items: 5, cycle: 8},
		}, want: &fkWant{day: d, units: 5, items: 5, churn: 500, cycleKnown: false}},
		{name: "nil-uuid-repository-counts-with-a-real-one", rows: []fkRow{
			{repo: fkNilRepo, day: d, at: "2026-03-02 10:00:00", units: 3, items: 3, cycle: 3},
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 4, items: 4, cycle: 4},
		}, want: &fkWant{day: d, units: 7, items: 7, churn: 700}},
		// A NULL repo_id and the nil UUID both mean "no repository": one key.
		{name: "null-repo-and-nil-uuid-are-one-key", rows: []fkRow{
			{repo: "", day: d, at: "2026-03-02 10:00:00", units: 50, items: 50, cycle: 5},
			{repo: fkNilRepo, day: d, at: "2026-03-02 11:00:00", units: 6, items: 6, cycle: 6},
		}, want: &fkWant{day: d, units: 6, items: 6, churn: 600, cycle: 6, cycleKnown: true}},
		{name: "latest-day-only-per-team-area-stream", rows: []fkRow{
			{repo: fkRepo(1), day: "2026-02-28", at: "2026-03-01 10:00:00", units: 100, items: 100, cycle: 1},
			{repo: fkRepo(2), day: d, at: "2026-03-02 10:00:00", units: 2, items: 2, cycle: 2},
		}, want: &fkWant{day: d, units: 2, items: 2, churn: 200, cycle: 2, cycleKnown: true}},
		{name: "other-org-rows-never-count", rows: []fkRow{
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 3, items: 3, cycle: 3},
			{repo: fkRepo(1), day: d, at: "2026-03-03 10:00:00", units: 900, items: 900, cycle: 900, org: "fk-other-org"},
			{repo: fkRepo(2), day: d, at: "2026-03-03 10:00:00", units: 800, items: 800, cycle: 800, org: "fk-other-org"},
		}, want: &fkWant{day: d, units: 3, items: 3, churn: 300, cycleKnown: true, cycle: 3}},
		{name: "rerun-of-an-older-day-does-not-hide-the-latest-day", rows: []fkRow{
			{repo: fkRepo(1), day: "2026-02-28", at: "2026-03-09 10:00:00", units: 100, items: 100, cycle: 1},
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 7, items: 7, cycle: 7},
		}, want: &fkWant{day: d, units: 7, items: 7, churn: 700, cycle: 7, cycleKnown: true}},
		{name: "no-completed-items-cycle-is-the-plain-mean", rows: []fkRow{
			{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 2, items: 0, cycle: 4},
			{repo: fkRepo(2), day: d, at: "2026-03-02 10:00:00", units: 3, items: 0, cycle: 8},
		}, want: &fkWant{day: d, units: 5, items: 0, churn: 500, cycleKnown: false}},
		{name: "no-rows-no-zero-filled-row", rows: nil, want: nil},
	}
}

const fkTieTeams = 12

// fkTieRows is two rows of ONE (day, repo) key with the SAME computed_at. The
// reader's documented rule: the row with the larger cityHash64 of its value
// columns wins. Odd teams insert the pair in the opposite order, so a winner
// that follows insertion order instead of the hash is caught.
func fkTieRows(i int) []fkRow {
	const d = "2026-03-01"
	low := fkRow{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 10 + i, items: 10 + i, cycle: float64(10 + i)}
	high := fkRow{repo: fkRepo(1), day: d, at: "2026-03-02 10:00:00", units: 30 + i, items: 30 + i, cycle: float64(30 + i)}
	if i%2 == 1 {
		return []fkRow{high, low}
	}
	return []fkRow{low, high}
}

func fkSeedInsert(team string, r fkRow) string {
	repo := "NULL"
	if r.repo != "" {
		repo = "'" + r.repo + "'"
	}
	org := r.org
	if org == "" {
		org = fkOrg
	}
	stream := r.stream
	if stream == "" {
		stream = "growth"
	}
	area := r.areaName
	if area == "" {
		area = "product"
	}
	return fmt.Sprintf("INSERT INTO investment_metrics_daily (repo_id, day, team_id, investment_area, project_stream, delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours, computed_at, org_id) VALUES (%s, '%s', '%s', '%s', '%s', %d, %d, %d, %d, %v, '%s', '%s')",
		repo, r.day, team, area, stream, r.units, r.items, r.items*2, r.units*100, r.cycle, r.at, org)
}

func fkClient(t *testing.T) (*clickhouse.Client, clickhousedriver.Conn) {
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
	database := fmt.Sprintf("inv_fk_%d", rand.Uint64())
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
	stmts := schema.DDL("investment_metrics_daily", "projects", "team_project_ownership", "teams")
	for _, stmt := range stmts {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("ddl %q: %v", stmt, err)
		}
	}
	parsed.Path = "/" + database
	client, err := clickhouse.NewClickHouseQueryClientWithOptions(clickhouse.Options{DSN: parsed.String(), QueryTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, seed
}

// fkSeedProject makes project "linear:proj-<team>" owned by team.
func fkSeedProject(t *testing.T, seed clickhousedriver.Conn, team string) string {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('proj-%s', '%s', 'linear', NULL, 'p', 1, 'started', '', '2026-01-01 00:00:00')", team, fkOrg),
		fmt.Sprintf("INSERT INTO team_project_ownership (org_id, provider, team_id, project_id, project_key, source, valid_from, valid_to, updated_at) VALUES ('%s', 'linear', '%s', 'proj-%s', NULL, 'native', '2026-01-01 00:00:00', NULL, '2026-01-01 00:00:00')", fkOrg, team, team),
		fmt.Sprintf("INSERT INTO teams (id, org_id, name, updated_at) VALUES ('%s', '%s', 'Team %s', '2026-01-01 00:00:00')", team, fkOrg, team),
	} {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return "linear:proj-" + team
}

func fkCheck(t *testing.T, label string, got *fkWant, want *fkWant) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s: got a row %+v, want no row (no zero-filled row)", label, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s: no row, want %+v", label, *want)
		return
	}
	if got.day != want.day || got.units != want.units || got.items != want.items || got.churn != want.churn || got.prs != want.items*2 {
		t.Errorf("%s: got %+v, want %+v", label, *got, *want)
	}
	if got.cycleKnown != want.cycleKnown || math.Abs(got.cycle-want.cycle) > 1e-9 {
		t.Errorf("%s: cycle = %v known=%v, want %v known=%v", label, got.cycle, got.cycleKnown, want.cycle, want.cycleKnown)
	}
}

func TestIntegrationInvestmentReadersDedupeByFullKeyThenSumRepositories(t *testing.T) {
	client, seed := fkClient(t)
	ctx := context.Background()
	cases := fkCases()
	for i := 0; i < fkTieTeams; i++ {
		cases = append(cases, fkCase{name: fmt.Sprintf("same-computed-at-%02d", i), rows: fkTieRows(i)})
	}
	projectKey := map[string]string{}
	for _, c := range cases {
		team := "t-" + c.name
		projectKey[c.name] = fkSeedProject(t, seed, team)
		for _, r := range c.rows {
			if err := seed.Exec(ctx, fkSeedInsert(team, r)); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	ids := make([]string, 0, len(cases))
	projects := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, "t-"+c.name)
		projects = append(projects, projectKey[c.name])
	}

	teamRows, err := readers.ReadTeamInvestment(ctx, client, fkOrg, ids, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadTeamInvestment: %v", err)
	}
	projectRows, err := readers.ReadProjectInvestment(ctx, client, fkOrg, projects, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadProjectInvestment: %v", err)
	}
	teamGot := map[string][]fkWant{}
	for _, r := range teamRows {
		teamGot[r.TeamID] = append(teamGot[r.TeamID], fkWant{day: r.Day, units: r.DeliveryUnits, items: r.WorkItemsCompleted, prs: r.PRsMerged, churn: r.ChurnLOC, cycle: r.CycleP50Hours, cycleKnown: r.CycleP50Known})
	}
	projectGot := map[string][]fkWant{}
	for _, r := range projectRows {
		projectGot[r.TeamID] = append(projectGot[r.TeamID], fkWant{day: r.Day, units: r.DeliveryUnits, items: r.WorkItemsCompleted, prs: r.PRsMerged, churn: r.ChurnLOC, cycle: r.CycleP50Hours, cycleKnown: r.CycleP50Known})
	}
	one := func(label string, got []fkWant) *fkWant {
		t.Helper()
		if len(got) > 1 {
			t.Errorf("%s: %d rows, want at most one per (team, area, stream)", label, len(got))
		}
		if len(got) == 0 {
			return nil
		}
		return &got[0]
	}
	for _, c := range cases {
		team := "t-" + c.name
		if c.want != nil {
			fkCheck(t, "ReadTeamInvestment "+c.name, one("team "+c.name, teamGot[team]), c.want)
			fkCheck(t, "ReadProjectInvestment "+c.name, one("project "+c.name, projectGot[team]), c.want)
			continue
		}
		if len(c.rows) == 0 {
			fkCheck(t, "ReadTeamInvestment "+c.name, one("team "+c.name, teamGot[team]), nil)
			fkCheck(t, "ReadProjectInvestment "+c.name, one("project "+c.name, projectGot[team]), nil)
		}
	}
	// Same computed_at: the row with the larger cityHash64 of its value
	// columns wins, whole, whatever the insertion order.
	for i := 0; i < fkTieTeams; i++ {
		team := fmt.Sprintf("t-same-computed-at-%02d", i)
		var winner uint32
		row := seed.QueryRow(ctx, fmt.Sprintf("SELECT delivery_units FROM investment_metrics_daily WHERE team_id = '%s' ORDER BY cityHash64(tuple(delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours)) DESC LIMIT 1", team))
		if err := row.Scan(&winner); err != nil {
			t.Fatal(err)
		}
		for label, got := range map[string][]fkWant{"ReadTeamInvestment": teamGot[team], "ReadProjectInvestment": projectGot[team]} {
			if len(got) != 1 || got[0].units != int64(winner) || got[0].items != int64(winner) {
				t.Errorf("same computed_at %s %s: got %+v, want the larger-hash row (units %d)", label, team, got, winner)
			}
		}
	}
}

// A time bound still applies: the latest day AT OR BEFORE the bound's end is
// the one served, summed across that day's repositories.
func TestIntegrationInvestmentReadersTimeBoundPicksLatestDayInWindow(t *testing.T) {
	client, seed := fkClient(t)
	ctx := context.Background()
	const team = "t-bound"
	project := fkSeedProject(t, seed, team)
	for _, r := range []fkRow{
		{repo: fkRepo(1), day: "2026-02-27", at: "2026-02-28 10:00:00", units: 4, items: 4, cycle: 4},
		{repo: fkRepo(2), day: "2026-02-27", at: "2026-02-28 10:00:00", units: 5, items: 5, cycle: 5},
		{repo: fkRepo(1), day: "2026-03-05", at: "2026-03-06 10:00:00", units: 100, items: 100, cycle: 1},
	} {
		if err := seed.Exec(ctx, fkSeedInsert(team, r)); err != nil {
			t.Fatal(err)
		}
	}
	bound := readers.TimeBound{Active: true, End: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	teamRows, err := readers.ReadTeamInvestment(ctx, client, fkOrg, []string{team}, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(teamRows) != 1 || teamRows[0].Day != "2026-02-27" || teamRows[0].DeliveryUnits != 9 {
		t.Errorf("ReadTeamInvestment bounded = %+v, want one row, day 2026-02-27, units 9", teamRows)
	}
	projectRows, err := readers.ReadProjectInvestment(ctx, client, fkOrg, []string{project}, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectRows) != 1 || projectRows[0].Day != "2026-02-27" || projectRows[0].DeliveryUnits != 9 {
		t.Errorf("ReadProjectInvestment bounded = %+v, want one row, day 2026-02-27, units 9", projectRows)
	}
}

// Each (area, stream) pair of a team is its own series with its own latest day.
func TestIntegrationInvestmentReadersLatestDayIsPerAreaAndStream(t *testing.T) {
	client, seed := fkClient(t)
	ctx := context.Background()
	const team = "t-series"
	project := fkSeedProject(t, seed, team)
	for _, r := range []fkRow{
		{repo: fkRepo(1), day: "2026-03-01", at: "2026-03-02 10:00:00", units: 1, items: 1},
		{repo: fkRepo(2), day: "2026-03-01", at: "2026-03-02 10:00:00", units: 2, items: 2},
		{repo: fkRepo(1), day: "2026-02-20", at: "2026-02-21 10:00:00", units: 4, items: 4, areaName: "ops"},
		{repo: fkRepo(1), day: "2026-02-10", at: "2026-02-11 10:00:00", units: 8, items: 8, stream: "platform"},
		{repo: fkRepo(2), day: "2026-02-10", at: "2026-02-11 10:00:00", units: 16, items: 16, stream: "platform"},
	} {
		if err := seed.Exec(ctx, fkSeedInsert(team, r)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]struct {
		day   string
		units int64
	}{"product/growth": {"2026-03-01", 3}, "ops/growth": {"2026-02-20", 4}, "product/platform": {"2026-02-10", 24}}
	teamRows, err := readers.ReadTeamInvestment(ctx, client, fkOrg, []string{team}, readers.TimeBound{})
	if err != nil {
		t.Fatal(err)
	}
	projectRows, err := readers.ReadProjectInvestment(ctx, client, fkOrg, []string{project}, readers.TimeBound{})
	if err != nil {
		t.Fatal(err)
	}
	got, gotProject := map[string][2]any{}, map[string][2]any{}
	for _, r := range teamRows {
		got[r.InvestmentArea+"/"+r.ProjectStream] = [2]any{r.Day, r.DeliveryUnits}
	}
	for _, r := range projectRows {
		gotProject[r.InvestmentArea+"/"+r.ProjectStream] = [2]any{r.Day, r.DeliveryUnits}
	}
	if len(teamRows) != len(want) || len(projectRows) != len(want) {
		t.Errorf("rows: team %d, project %d, want %d each", len(teamRows), len(projectRows), len(want))
	}
	for key, w := range want {
		for label, m := range map[string]map[string][2]any{"ReadTeamInvestment": got, "ReadProjectInvestment": gotProject} {
			if m[key] != [2]any{w.day, w.units} {
				t.Errorf("%s %s = %v, want day %s units %d", label, key, m[key], w.day, w.units)
			}
		}
	}
}

// One repository writing several (area, stream) pairs on one day keeps each
// pair: area and stream are part of the dedupe key.
func TestIntegrationInvestmentReadersSameDayRepositoryKeepsEveryAreaAndStream(t *testing.T) {
	client, seed := fkClient(t)
	ctx := context.Background()
	const team, day, at = "t-pairs", "2026-03-01", "2026-03-02 10:00:00"
	project := fkSeedProject(t, seed, team)
	for _, r := range []fkRow{
		{repo: fkRepo(1), day: day, at: at, units: 1, items: 1},
		{repo: fkRepo(1), day: day, at: at, units: 2, items: 2, areaName: "ops"},
		{repo: fkRepo(1), day: day, at: at, units: 4, items: 4, stream: "platform"},
	} {
		if err := seed.Exec(ctx, fkSeedInsert(team, r)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int64{"product/growth": 1, "ops/growth": 2, "product/platform": 4}
	teamRows, err := readers.ReadTeamInvestment(ctx, client, fkOrg, []string{team}, readers.TimeBound{})
	if err != nil {
		t.Fatal(err)
	}
	projectRows, err := readers.ReadProjectInvestment(ctx, client, fkOrg, []string{project}, readers.TimeBound{})
	if err != nil {
		t.Fatal(err)
	}
	got, gotProject := map[string]int64{}, map[string]int64{}
	for _, r := range teamRows {
		got[r.InvestmentArea+"/"+r.ProjectStream] = r.DeliveryUnits
	}
	for _, r := range projectRows {
		gotProject[r.InvestmentArea+"/"+r.ProjectStream] = r.DeliveryUnits
	}
	for key, w := range want {
		if got[key] != w || gotProject[key] != w {
			t.Errorf("%s: team %d, project %d, want %d", key, got[key], gotProject[key], w)
		}
	}
	if len(teamRows) != len(want) || len(projectRows) != len(want) {
		t.Errorf("rows: team %d, project %d, want %d each", len(teamRows), len(projectRows), len(want))
	}
}
