//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math/rand/v2"
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

// Two versions of one work unit at ONE computed_at millisecond: the readers
// must serve the later insert, the row a ReplacingMergeTree merge keeps, in
// every column, before and after the merge. The want is the same reader on a
// database that holds only the later rows, so it does not depend on the key.

const (
	vtTieAt   = "2026-03-01 00:00:00.123"
	vtNewerAt = "2026-03-01 00:00:01.000"
	vtOlderAt = "2026-03-01 00:00:00.500"
)

type vtRow struct {
	unit, from, to, theme, item, computedAt string
	bugfix                                  float64
}

type vtPair struct {
	first, second vtRow // insert order; the winner is second unless computedAt says otherwise
	winner        vtRow
}

func vtWindowed(kind, side string) (string, string) {
	if side == "x" {
		return "2026-02-01 00:00:00", "2026-04-01 00:00:00"
	}
	switch kind {
	case "after":
		return "2026-05-01 00:00:00", "2026-06-01 00:00:00"
	case "before":
		return "2025-01-01 00:00:00", "2025-02-01 00:00:00"
	}
	return "2026-02-10 00:00:00", "2026-03-20 00:00:00"
}

// vtPairs returns tie units of three kinds (the second row in the window,
// after it, before it), each in both insert orders, plus units whose newer
// computed_at row is the FIRST insert. The two rows of a unit differ in every
// column the readers pick, and each row's evidence item belongs to a team and a
// project of its own, so a wrong pick at any one site moves that unit's own
// output row (one unit's error can never cancel another's in a sum).
func vtPairs() []vtPair {
	row := func(unit, kind, side, at string) vtRow {
		from, to := vtWindowed(kind, side)
		r := vtRow{unit: unit, from: from, to: to, computedAt: at, item: "linear:VTX-" + unit, theme: "feature_delivery", bugfix: 0.25}
		if side == "y" {
			r.item, r.theme, r.bugfix = "linear:VTY-"+unit, "risk", 0.75
		}
		return r
	}
	var out []vtPair
	for _, kind := range []string{"in", "after", "before"} {
		for _, order := range []string{"xy", "yx"} {
			for rep := 0; rep < 2; rep++ {
				unit := fmt.Sprintf("u-tie-%s-%s-%d", kind, order, rep)
				x, y := row(unit, kind, "x", vtTieAt), row(unit, kind, "y", vtTieAt)
				if order == "xy" {
					out = append(out, vtPair{first: x, second: y, winner: y})
				} else {
					out = append(out, vtPair{first: y, second: x, winner: x})
				}
			}
		}
	}
	for rep := 0; rep < 2; rep++ {
		unit := fmt.Sprintf("u-ver-%d", rep)
		newer, older := row(unit, "in", "x", vtNewerAt), row(unit, "in", "y", vtOlderAt)
		out = append(out, vtPair{first: newer, second: older, winner: newer})
	}
	return out
}

// vtFillers sort before every u- unit in ORDER BY (org_id, work_unit_id) and
// resolve to no team and no project. In the first part they push each tied
// row to a higher offset than its twin in the later part, so a key without the
// block number picks the earlier insert.
func vtFillers() []vtRow {
	var out []vtRow
	for i := 0; i < 16; i++ {
		out = append(out, vtRow{unit: fmt.Sprintf("a-fill-%02d", i), from: "2026-02-01 00:00:00", to: "2026-04-01 00:00:00", theme: "operational", item: "linear:NOPE-1", computedAt: vtTieAt})
	}
	return out
}

func vtValues(rows []vtRow) string {
	values := make([]string, len(rows))
	for i, r := range rows {
		values[i] = fmt.Sprintf(`('%s', '%s', '%s', 1, map('%s', 1), map('quality.bugfix', %g), '{"issues":["%s"],"prs":[]}', '%s', '%s')`,
			r.unit, r.from, r.to, r.theme, r.bugfix, r.item, r.computedAt, ptmOrg)
	}
	return strings.Join(values, ", ")
}

func vtInsert(settings string, rows []vtRow) string {
	return "INSERT INTO work_unit_investments (work_unit_id, from_ts, to_ts, effort_value, theme_distribution_json, subcategory_distribution_json, structural_evidence_json, computed_at, org_id) SETTINGS " + settings + " VALUES " + vtValues(rows)
}

// vtSubjects returns the team ids and project subject keys of every row of
// the pairs: one team and one project per row.
func vtSubjects(pairs []vtPair) (teams, projects []string) {
	for _, p := range pairs {
		for _, side := range []string{"x", "y"} {
			teams = append(teams, "t-"+side+"-"+p.first.unit)
			projects = append(projects, "linear:p-"+side+"-"+p.first.unit)
		}
	}
	return teams, projects
}

func vtContext(pairs []vtPair) []string {
	var items []ptmItem
	var stmts []string
	for _, p := range pairs {
		for _, side := range []string{"x", "y"} {
			item := "linear:VT" + strings.ToUpper(side) + "-" + p.first.unit
			project := "p-" + side + "-" + p.first.unit
			items = append(items, ptmItem{id: item, org: ptmOrg, project: project})
			stmts = append(stmts,
				fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('%s', '%s', 'linear', NULL, '%s', 1, 'started', '', '2026-03-01 00:00:00')", project, ptmOrg, project),
				ttmAttribution(ptmOrg, ptmRepo, item, "t-"+side+"-"+p.first.unit))
		}
	}
	return append(ptmSeed(items, nil), stmts...)
}

func vtDatabase(t *testing.T, prefix string) (clickhousedriver.Conn, *clickhouse.Client) {
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
	stmts := schema.DDL("projects", "work_items", "work_unit_investments", "project_membership_transitions", "repos", "work_item_team_attributions")
	stmts = append(stmts, schema.ProjectMembershipPresenceViewDDL, "SYSTEM STOP MERGES work_unit_investments")
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
	return seed, client
}

func vtExec(t *testing.T, seed clickhousedriver.Conn, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if err := seed.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// vtRequireStored fails unless every u- unit holds rows rows in parts parts,
// so a fixture a merge or an insert dedup already collapsed never passes.
func vtRequireStored(t *testing.T, seed clickhousedriver.Conn, units, rows, parts int) {
	t.Helper()
	got, err := seed.Query(context.Background(), "SELECT work_unit_id, count(), uniqExact(_part) FROM work_unit_investments WHERE startsWith(work_unit_id, 'u-') GROUP BY work_unit_id ORDER BY work_unit_id")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	seen := 0
	for got.Next() {
		var unit string
		var count, distinctParts uint64
		if err := got.Scan(&unit, &count, &distinctParts); err != nil {
			t.Fatal(err)
		}
		seen++
		if int(count) != rows || int(distinctParts) != parts {
			t.Fatalf("fixture: unit %s holds %d rows in %d parts, want %d rows in %d parts", unit, count, distinctParts, rows, parts)
		}
	}
	if err := got.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != units {
		t.Fatalf("fixture: %d units stored, want %d", seen, units)
	}
}

var vtWindow = readers.TimeBound{Active: true, HasStart: true, Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}

func vtRead(t *testing.T, client *clickhouse.Client, teamIDs, projectIDs []string) (team, project []string) {
	t.Helper()
	ctx := context.Background()
	teamRows, err := readers.ReadTeamThemeMix(ctx, client, ptmOrg, teamIDs, vtWindow)
	if err != nil {
		t.Fatalf("ReadTeamThemeMix: %v", err)
	}
	for _, r := range teamRows {
		team = append(team, fmt.Sprintf("%s|%s|%s|%s|%.6f", r.TeamID, r.TeamName, r.Kind, r.Key, r.WorkUnits))
	}
	projectRows, err := readers.ReadProjectThemeMix(ctx, client, ptmOrg, projectIDs, vtWindow)
	if err != nil {
		t.Fatalf("ReadProjectThemeMix: %v", err)
	}
	for _, r := range projectRows {
		project = append(project, fmt.Sprintf("%s|fd=%.6f|op=%.6f|mt=%.6f|q=%.6f|r=%.6f|bf=%.6f|wu=%d|sp=%d|mp=%d",
			r.ProjectSubjectKey, r.FeatureDelivery, r.Operational, r.Maintenance, r.Quality, r.Risk, r.BugfixWeighted, r.WorkUnits, r.SpanningUnits, r.MultiPlacedUnits))
	}
	sort.Strings(team)
	sort.Strings(project)
	return team, project
}

func vtCompare(t *testing.T, arm string, gotTeam, gotProject, wantTeam, wantProject []string) {
	t.Helper()
	t.Run(arm+"/team", func(t *testing.T) {
		if strings.Join(gotTeam, "\n") != strings.Join(wantTeam, "\n") {
			t.Errorf("team mix:\n got  %v\n want %v", gotTeam, wantTeam)
		}
	})
	t.Run(arm+"/project", func(t *testing.T) {
		if strings.Join(gotProject, "\n") != strings.Join(wantProject, "\n") {
			t.Errorf("project mix:\n got  %v\n want %v", gotProject, wantProject)
		}
	})
}

func vtMerge(t *testing.T, seed clickhousedriver.Conn, units int) {
	t.Helper()
	vtExec(t, seed, "SYSTEM START MERGES work_unit_investments", "OPTIMIZE TABLE work_unit_investments FINAL")
	vtRequireStored(t, seed, units, 1, 1)
}

func TestIntegrationWorkUnitVersionSameMillisecondTiePicksTheLaterInsert(t *testing.T) {
	pairs := vtPairs()
	fillers := vtFillers()
	var first, second, winners, onePart []vtRow
	for _, p := range pairs {
		first = append(first, p.first)
		second = append(second, p.second)
		winners = append(winners, p.winner)
		onePart = append(onePart, p.first, p.second)
	}
	seedContext := vtContext(pairs)
	teamIDs, projectIDs := vtSubjects(pairs)
	const sync = "async_insert=0"

	oracleSeed, oracle := vtDatabase(t, "vt_oracle")
	vtExec(t, oracleSeed, seedContext...)
	vtExec(t, oracleSeed, vtInsert(sync, append(append([]vtRow{}, fillers...), winners...)))
	wantTeam, wantProject := vtRead(t, oracle, teamIDs, projectIDs)
	// In the window: the 4 "in" units, the 4 "after"/"before" units whose
	// in-window row is the later insert, and the 2 newer-first units.
	if len(wantProject) != 10 || len(wantTeam) != 20 {
		t.Fatalf("oracle answer: %d project rows and %d team rows, want 10 and 20: team %v project %v", len(wantProject), len(wantTeam), wantTeam, wantProject)
	}

	// Two parts: the first insert also carries the fillers, so only the block
	// number orders a tie (the earlier row has the higher offset).
	twoSeed, two := vtDatabase(t, "vt_two_parts")
	vtExec(t, twoSeed, seedContext...)
	vtExec(t, twoSeed, vtInsert(sync, append(append([]vtRow{}, fillers...), first...)), vtInsert(sync, second))
	vtRequireStored(t, twoSeed, len(pairs), 2, 2)
	gotTeam, gotProject := vtRead(t, two, teamIDs, projectIDs)
	vtCompare(t, "two_parts", gotTeam, gotProject, wantTeam, wantProject)
	vtMerge(t, twoSeed, len(pairs))
	gotTeam, gotProject = vtRead(t, two, teamIDs, projectIDs)
	vtCompare(t, "two_parts_after_merge", gotTeam, gotProject, wantTeam, wantProject)

	// One part: both rows of a unit in one insert with no insert-time dedup, so
	// only the row offset orders a tie.
	oneSeed, one := vtDatabase(t, "vt_one_part")
	vtExec(t, oneSeed, seedContext...)
	vtExec(t, oneSeed, vtInsert(sync+", optimize_on_insert=0", append(append([]vtRow{}, fillers...), onePart...)))
	vtRequireStored(t, oneSeed, len(pairs), 2, 1)
	gotTeam, gotProject = vtRead(t, one, teamIDs, projectIDs)
	vtCompare(t, "one_part", gotTeam, gotProject, wantTeam, wantProject)
	vtMerge(t, oneSeed, len(pairs))
	gotTeam, gotProject = vtRead(t, one, teamIDs, projectIDs)
	vtCompare(t, "one_part_after_merge", gotTeam, gotProject, wantTeam, wantProject)
}
