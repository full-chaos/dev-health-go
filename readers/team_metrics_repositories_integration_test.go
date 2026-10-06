//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/clickhouse"
	"github.com/full-chaos/dev-health-go/readers"
	"github.com/full-chaos/dev-health-go/schema"
)

// team_metrics_daily is written one row per (org_id, team_id, repo_id, day)
// (ops migrations 080 and 096); legacy rows are the one repo_id = '' bucket.
// ReadTeamMetrics and ReadProjectMetricsBreakdown must take the newest row per
// (team, repo, day), drop the legacy bucket for a (team, day) once any real
// repository row exists, then sum the counts over repositories and recompute
// both ratios from the sums. These cells run both readers against a real
// ClickHouse table built by schema.DDL.

const tmOrg = "tm-org"

type tmRow struct {
	repo    string // "" = the legacy bucket
	day     string
	at      string // computed_at
	commits int
	ah      int
	wk      int
	name    string // "" = "Team"
	org     string // "" = tmOrg
}

type tmWant struct {
	day     string
	commits int64
	ah      int64
	wk      int64
	ahRatio float64
	wkRatio float64
	name    string // "" = not checked
}

type tmCase struct {
	name string
	rows []tmRow
	want *tmWant // nil = no row at all
}

func tmRepo(n int) string { return fmt.Sprintf("tm-repo-%d", n) }

func tmCases() []tmCase {
	const d = "2026-03-01"
	return []tmCase{
		{name: "two-repos-with-older-rows", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 99, ah: 99, wk: 99},
			{repo: tmRepo(1), day: d, at: "2026-03-02 11:00:00", commits: 10, ah: 4, wk: 1},
			{repo: tmRepo(2), day: d, at: "2026-03-02 10:00:00", commits: 88, ah: 88, wk: 88},
			{repo: tmRepo(2), day: d, at: "2026-03-02 11:00:00", commits: 20, ah: 6, wk: 8},
		}, want: &tmWant{day: d, commits: 30, ah: 10, wk: 9, ahRatio: 10.0 / 30, wkRatio: 9.0 / 30}},
		{name: "three-repos", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 5, ah: 1, wk: 2},
			{repo: tmRepo(2), day: d, at: "2026-03-02 10:00:00", commits: 6, ah: 2, wk: 0},
			{repo: tmRepo(3), day: d, at: "2026-03-02 10:00:00", commits: 7, ah: 3, wk: 4},
			{repo: tmRepo(3), day: d, at: "2026-03-01 10:00:00", commits: 70, ah: 70, wk: 70},
		}, want: &tmWant{day: d, commits: 18, ah: 6, wk: 6, ahRatio: 6.0 / 18, wkRatio: 6.0 / 18}},
		{name: "one-repo-unchanged", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 99, ah: 99, wk: 99},
			{repo: tmRepo(1), day: d, at: "2026-03-02 11:00:00", commits: 10, ah: 4, wk: 2},
		}, want: &tmWant{day: d, commits: 10, ah: 4, wk: 2, ahRatio: 0.4, wkRatio: 0.2}},
		{name: "newest-zero-never-falls-back", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 7, ah: 3, wk: 3},
			{repo: tmRepo(1), day: d, at: "2026-03-02 11:00:00", commits: 0, ah: 0, wk: 0},
			{repo: tmRepo(2), day: d, at: "2026-03-02 11:00:00", commits: 5, ah: 1, wk: 2},
		}, want: &tmWant{day: d, commits: 5, ah: 1, wk: 2, ahRatio: 0.2, wkRatio: 0.4}},
		{name: "zero-commits-total-serves-ratio-zero", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 11:00:00", commits: 0, ah: 0, wk: 0},
			{repo: tmRepo(2), day: d, at: "2026-03-02 11:00:00", commits: 0, ah: 0, wk: 0},
		}, want: &tmWant{day: d}},
		{name: "legacy-bucket-alone", rows: []tmRow{
			{repo: "", day: d, at: "2026-03-02 10:00:00", commits: 99, ah: 99, wk: 99},
			{repo: "", day: d, at: "2026-03-02 11:00:00", commits: 8, ah: 2, wk: 4},
		}, want: &tmWant{day: d, commits: 8, ah: 2, wk: 4, ahRatio: 0.25, wkRatio: 0.5}},
		{name: "legacy-bucket-beside-real-rows-is-dropped", rows: []tmRow{
			{repo: "", day: d, at: "2026-03-03 10:00:00", commits: 100, ah: 50, wk: 50},
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 3, ah: 1, wk: 0},
			{repo: tmRepo(2), day: d, at: "2026-03-02 10:00:00", commits: 4, ah: 2, wk: 1},
		}, want: &tmWant{day: d, commits: 7, ah: 3, wk: 1, ahRatio: 3.0 / 7, wkRatio: 1.0 / 7}},
		// The legacy drop is per (team, day): a legacy-only day newer than the
		// real-row day is the latest day and is served as itself.
		{name: "legacy-only-latest-day-beside-older-real-day", rows: []tmRow{
			{repo: tmRepo(1), day: "2026-02-28", at: "2026-03-01 10:00:00", commits: 3, ah: 1, wk: 1},
			{repo: "", day: d, at: "2026-03-02 10:00:00", commits: 9, ah: 3, wk: 0},
		}, want: &tmWant{day: d, commits: 9, ah: 3, wk: 0, ahRatio: 3.0 / 9, wkRatio: 0}},
		// Day rule (unchanged): the latest day of the team over ALL repositories
		// is served; an older day of another repository is not added.
		{name: "repo-a-last-on-day-one-repo-b-on-day-two", rows: []tmRow{
			{repo: tmRepo(1), day: "2026-02-28", at: "2026-03-01 10:00:00", commits: 100, ah: 50, wk: 50},
			{repo: tmRepo(2), day: d, at: "2026-03-02 10:00:00", commits: 2, ah: 1, wk: 0},
		}, want: &tmWant{day: d, commits: 2, ah: 1, wk: 0, ahRatio: 0.5, wkRatio: 0}},
		{name: "other-org-rows-never-count", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 3, ah: 1, wk: 1},
			{repo: tmRepo(1), day: d, at: "2026-03-03 10:00:00", commits: 900, ah: 900, wk: 900, org: "tm-other-org"},
			{repo: tmRepo(2), day: d, at: "2026-03-03 10:00:00", commits: 800, ah: 800, wk: 800, org: "tm-other-org"},
			{repo: tmRepo(2), day: "2026-03-09", at: "2026-03-10 10:00:00", commits: 700, ah: 700, wk: 700, org: "tm-other-org"},
		}, want: &tmWant{day: d, commits: 3, ah: 1, wk: 1, ahRatio: 1.0 / 3, wkRatio: 1.0 / 3}},
		{name: "team-name-follows-the-newest-kept-row", rows: []tmRow{
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 1, ah: 0, wk: 0, name: "Old Name"},
			{repo: tmRepo(2), day: d, at: "2026-03-02 11:00:00", commits: 2, ah: 0, wk: 0, name: "New Name"},
		}, want: &tmWant{day: d, commits: 3, name: "New Name"}},
		// A rerun of an older day written AFTER the latest day's row must not hide
		// the latest day: the dedupe key includes the day.
		{name: "rerun-of-an-older-day-does-not-hide-the-latest-day", rows: []tmRow{
			{repo: tmRepo(1), day: "2026-02-28", at: "2026-03-09 10:00:00", commits: 100, ah: 50, wk: 50},
			{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 7, ah: 1, wk: 2},
		}, want: &tmWant{day: d, commits: 7, ah: 1, wk: 2, ahRatio: 1.0 / 7, wkRatio: 2.0 / 7}},
		// The legacy drop counts real rows of THIS org only: a real row of another
		// org with the same team id and day must not hide the legacy bucket.
		{name: "legacy-bucket-beside-a-real-row-of-another-org", rows: []tmRow{
			{repo: "", day: d, at: "2026-03-02 10:00:00", commits: 8, ah: 2, wk: 4},
			{repo: tmRepo(1), day: d, at: "2026-03-03 10:00:00", commits: 900, ah: 900, wk: 900, org: "tm-other-org"},
		}, want: &tmWant{day: d, commits: 8, ah: 2, wk: 4, ahRatio: 0.25, wkRatio: 0.5}},
		{name: "no-rows-no-zero-filled-row", rows: nil, want: nil},
	}
}

const tmTieTeams = 12

const tmNameTieTeams = 8

const tmNameTimeTeams = 8

// tmNameTimeRows: two repositories, different computed_at, different names; the
// newer row's name is served whatever the row hashes are.
func tmNameTimeRows(i int) []tmRow {
	const d = "2026-03-01"
	return []tmRow{
		{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 50 + 7*i, ah: i, wk: 1, name: fmt.Sprintf("Older %d", i)},
		{repo: tmRepo(2), day: d, at: "2026-03-02 11:00:00", commits: 2 + i, ah: 1, wk: 0, name: fmt.Sprintf("Newer %d", i)},
	}
}

// tmNameTieRows is two repositories of ONE team with the SAME computed_at and
// different names: the served label is the name of the row with the larger
// cityHash64 (the newest-kept-row rule, deterministic on a tie). Odd teams
// insert the pair in the opposite order.
func tmNameTieRows(i int) []tmRow {
	const d = "2026-03-01"
	a := tmRow{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 3 + i, ah: 1, wk: 1, name: fmt.Sprintf("Alpha %d", i)}
	b := tmRow{repo: tmRepo(2), day: d, at: "2026-03-02 10:00:00", commits: 5 + 2*i, ah: 2, wk: 0, name: fmt.Sprintf("Beta %d", i)}
	if i%2 == 1 {
		return []tmRow{b, a}
	}
	return []tmRow{a, b}
}

// tmTieRows is two rows of ONE (team, repo, day) key with the SAME computed_at.
// The rule: the row with the larger cityHash64 of its value columns wins,
// whole. Odd teams insert the pair in the opposite order.
func tmTieRows(i int) []tmRow {
	const d = "2026-03-01"
	low := tmRow{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 10 + i, ah: 1 + i, wk: 2 + i}
	high := tmRow{repo: tmRepo(1), day: d, at: "2026-03-02 10:00:00", commits: 30 + i, ah: 5 + i, wk: 7 + i}
	if i%2 == 1 {
		return []tmRow{high, low}
	}
	return []tmRow{low, high}
}

// The stored ratios are deliberately wrong (0.77, 0.66): a reader that serves a
// stored ratio, or averages stored ratios, fails at a value assertion.
func tmSeedInsert(team string, r tmRow) string {
	org := r.org
	if org == "" {
		org = tmOrg
	}
	name := r.name
	if name == "" {
		name = "Team"
	}
	return fmt.Sprintf("INSERT INTO team_metrics_daily (day, team_id, team_name, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio, computed_at, org_id, repo_id) VALUES ('%s', '%s', '%s', %d, %d, %d, 0.77, 0.66, '%s', '%s', '%s')",
		r.day, team, name, r.commits, r.ah, r.wk, r.at, org, r.repo)
}

func tmClient(t *testing.T) (*clickhouse.Client, clickhousedriver.Conn) {
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
	database := fmt.Sprintf("tm_repo_%d", rand.Uint64())
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
	for _, stmt := range schema.DDL("team_metrics_daily", "projects", "team_project_ownership", "teams") {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("ddl %q: %v", stmt, err)
		}
	}
	// The production table is a ReplacingMergeTree: a background merge could
	// collapse the deliberate same-key rows of these cells mid-test.
	if err := seed.Exec(ctx, "SYSTEM STOP MERGES team_metrics_daily"); err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	client, err := clickhouse.NewClickHouseQueryClientWithOptions(clickhouse.Options{DSN: parsed.String(), QueryTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, seed
}

// tmSeedProject makes project "linear:proj-<team>" owned by team in tmOrg.
func tmSeedProject(t *testing.T, seed clickhousedriver.Conn, team string) string {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('proj-%s', '%s', 'linear', NULL, 'p', 1, 'started', '', '2026-01-01 00:00:00')", team, tmOrg),
		fmt.Sprintf("INSERT INTO team_project_ownership (org_id, provider, team_id, project_id, project_key, source, valid_from, valid_to, updated_at) VALUES ('%s', 'linear', '%s', 'proj-%s', NULL, 'native', '2026-01-01 00:00:00', NULL, '2026-01-01 00:00:00')", tmOrg, team, team),
		fmt.Sprintf("INSERT INTO teams (id, org_id, name, updated_at) VALUES ('%s', '%s', 'Team %s', '2026-01-01 00:00:00')", team, tmOrg, team),
	} {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return "linear:proj-" + team
}

func tmCheck(t *testing.T, label string, got, want *tmWant) {
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
	if got.day != want.day || got.commits != want.commits || got.ah != want.ah || got.wk != want.wk {
		t.Errorf("%s: got %+v, want %+v", label, *got, *want)
	}
	if !(math.Abs(got.ahRatio-want.ahRatio) <= 1e-9) || !(math.Abs(got.wkRatio-want.wkRatio) <= 1e-9) {
		t.Errorf("%s: ratios = %v/%v, want %v/%v (recomputed from the summed counts)", label, got.ahRatio, got.wkRatio, want.ahRatio, want.wkRatio)
	}
	if want.name != "" && got.name != want.name {
		t.Errorf("%s: team_name = %q, want %q", label, got.name, want.name)
	}
}

func tmFromTeamRow(r readers.TeamMetricsRow) tmWant {
	return tmWant{day: r.Day, commits: r.CommitsCount, ah: r.AfterHoursCommitsCount, wk: r.WeekendCommitsCount, ahRatio: r.AfterHoursCommitRatio, wkRatio: r.WeekendCommitRatio}
}

func TestIntegrationTeamMetricsReadersSumRepositoriesAfterFullKeyDedupe(t *testing.T) {
	client, seed := tmClient(t)
	ctx := context.Background()
	cases := tmCases()
	for i := 0; i < tmTieTeams; i++ {
		cases = append(cases, tmCase{name: fmt.Sprintf("same-computed-at-%02d", i), rows: tmTieRows(i)})
	}
	for i := 0; i < tmNameTimeTeams; i++ {
		cases = append(cases, tmCase{name: fmt.Sprintf("name-time-%02d", i), rows: tmNameTimeRows(i), want: &tmWant{day: "2026-03-01", commits: int64(52 + 8*i), ah: int64(i + 1), wk: 1, ahRatio: float64(i+1) / float64(52+8*i), wkRatio: 1 / float64(52+8*i), name: fmt.Sprintf("Newer %d", i)}})
	}
	for i := 0; i < tmNameTieTeams; i++ {
		cases = append(cases, tmCase{name: fmt.Sprintf("name-tie-%02d", i), rows: tmNameTieRows(i)})
	}
	projectKey := map[string]string{}
	for _, c := range cases {
		team := "t-" + c.name
		projectKey[c.name] = tmSeedProject(t, seed, team)
		for _, r := range c.rows {
			if err := seed.Exec(ctx, tmSeedInsert(team, r)); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	// A team outside the requested scope, in this org and owning a project, and
	// a team of another org with the SAME team id as a requested team, add
	// nothing to any served row.
	const outTeam = "t-out-of-scope"
	outProject := tmSeedProject(t, seed, outTeam)
	for _, r := range []tmRow{
		{repo: tmRepo(1), day: "2026-03-01", at: "2026-03-02 10:00:00", commits: 1000, ah: 1000, wk: 1000},
		{repo: tmRepo(2), day: "2026-03-05", at: "2026-03-06 10:00:00", commits: 2000, ah: 2000, wk: 2000},
	} {
		if err := seed.Exec(ctx, tmSeedInsert(outTeam, r)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ids := make([]string, 0, len(cases))
	projects := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, "t-"+c.name)
		projects = append(projects, projectKey[c.name])
	}
	_ = outProject

	teamRows, err := readers.ReadTeamMetrics(ctx, client, tmOrg, ids, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadTeamMetrics: %v", err)
	}
	projectRows, err := readers.ReadProjectMetricsBreakdown(ctx, client, tmOrg, projects, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadProjectMetricsBreakdown: %v", err)
	}
	teamGot := map[string][]tmWant{}
	for _, r := range teamRows {
		if r.TeamID == outTeam {
			t.Errorf("ReadTeamMetrics served a team outside the scope: %+v", r)
		}
		teamGot[r.TeamID] = append(teamGot[r.TeamID], tmFromTeamRow(r))
	}
	projectGot := map[string][]tmWant{}
	for _, r := range projectRows {
		if r.TeamID == outTeam {
			t.Errorf("ReadProjectMetricsBreakdown served a team outside the scope: %+v", r)
		}
		w := tmFromTeamRow(r.TeamMetricsRow)
		w.name = r.TeamName
		projectGot[r.TeamID] = append(projectGot[r.TeamID], w)
	}
	one := func(label string, got []tmWant) *tmWant {
		t.Helper()
		if len(got) > 1 {
			t.Errorf("%s: %d rows, want at most one per team", label, len(got))
		}
		if len(got) == 0 {
			return nil
		}
		return &got[0]
	}
	for _, c := range cases {
		team := "t-" + c.name
		if c.want == nil && len(c.rows) > 0 {
			continue
		}
		if strings.HasPrefix(c.name, "name-tie-") || strings.HasPrefix(c.name, "same-computed-at-") {
			continue // checked below against the larger-hash row
		}
		tw := one("team "+c.name, teamGot[team])
		pw := one("project "+c.name, projectGot[team])
		if c.want != nil && c.want.name != "" {
			// team_name is served by the project reader only.
			cw := *c.want
			cw.name = ""
			tmCheck(t, "ReadTeamMetrics "+c.name, tw, &cw)
		} else {
			tmCheck(t, "ReadTeamMetrics "+c.name, tw, c.want)
		}
		tmCheck(t, "ReadProjectMetricsBreakdown "+c.name, pw, c.want)
	}
	// Same computed_at: the row with the larger cityHash64 of its value columns
	// wins, whole (counts of the winner, ratios recomputed from them).
	for i := 0; i < tmTieTeams; i++ {
		team := fmt.Sprintf("t-same-computed-at-%02d", i)
		var commits, ah, wk uint32
		row := seed.QueryRow(ctx, fmt.Sprintf("SELECT commits_count, after_hours_commits_count, weekend_commits_count FROM team_metrics_daily WHERE team_id = '%s' ORDER BY cityHash64(tuple(team_name, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio)) DESC LIMIT 1", team))
		if err := row.Scan(&commits, &ah, &wk); err != nil {
			t.Fatal(err)
		}
		want := &tmWant{day: "2026-03-01", commits: int64(commits), ah: int64(ah), wk: int64(wk), ahRatio: float64(ah) / float64(commits), wkRatio: float64(wk) / float64(commits)}
		tmCheck(t, "ReadTeamMetrics "+team, one(team, teamGot[team]), want)
		tmCheck(t, "ReadProjectMetricsBreakdown "+team, one(team, projectGot[team]), want)
	}
	// Same computed_at across two repositories with different names: the name of
	// the larger-hash row is served (project reader), counts are summed.
	for i := 0; i < tmNameTieTeams; i++ {
		team := fmt.Sprintf("t-name-tie-%02d", i)
		var winner string
		row := seed.QueryRow(ctx, fmt.Sprintf("SELECT team_name FROM team_metrics_daily WHERE team_id = '%s' ORDER BY computed_at DESC, cityHash64(tuple(team_name, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio)) DESC LIMIT 1", team))
		if err := row.Scan(&winner); err != nil {
			t.Fatal(err)
		}
		commits := int64(3+i) + int64(5+2*i)
		want := &tmWant{day: "2026-03-01", commits: commits, ah: 3, wk: 1, ahRatio: 3 / float64(commits), wkRatio: 1 / float64(commits), name: winner}
		cw := *want
		cw.name = ""
		tmCheck(t, "ReadTeamMetrics "+team, one(team, teamGot[team]), &cw)
		tmCheck(t, "ReadProjectMetricsBreakdown "+team, one(team, projectGot[team]), want)
	}
}

// A time bound still applies: the latest day AT OR BEFORE the bound's end is the
// one served, summed over that day's repositories.
func TestIntegrationTeamMetricsReadersTimeBoundPicksLatestDayInWindow(t *testing.T) {
	client, seed := tmClient(t)
	ctx := context.Background()
	const team = "t-bound"
	project := tmSeedProject(t, seed, team)
	for _, r := range []tmRow{
		{repo: tmRepo(1), day: "2026-02-27", at: "2026-02-28 10:00:00", commits: 4, ah: 1, wk: 0},
		{repo: tmRepo(2), day: "2026-02-27", at: "2026-02-28 10:00:00", commits: 6, ah: 1, wk: 2},
		{repo: tmRepo(1), day: "2026-03-05", at: "2026-03-06 10:00:00", commits: 100, ah: 100, wk: 100},
	} {
		if err := seed.Exec(ctx, tmSeedInsert(team, r)); err != nil {
			t.Fatal(err)
		}
	}
	bound := readers.TimeBound{Active: true, End: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	want := &tmWant{day: "2026-02-27", commits: 10, ah: 2, wk: 2, ahRatio: 0.2, wkRatio: 0.2}
	teamRows, err := readers.ReadTeamMetrics(ctx, client, tmOrg, []string{team}, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(teamRows) != 1 {
		t.Fatalf("ReadTeamMetrics bounded = %+v, want one row", teamRows)
	}
	g := tmFromTeamRow(teamRows[0])
	tmCheck(t, "ReadTeamMetrics bounded", &g, want)
	projectRows, err := readers.ReadProjectMetricsBreakdown(ctx, client, tmOrg, []string{project}, bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectRows) != 1 {
		t.Fatalf("ReadProjectMetricsBreakdown bounded = %+v, want one row", projectRows)
	}
	g = tmFromTeamRow(projectRows[0].TeamMetricsRow)
	tmCheck(t, "ReadProjectMetricsBreakdown bounded", &g, want)
}
