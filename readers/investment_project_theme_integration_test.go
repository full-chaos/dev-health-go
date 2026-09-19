//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math"
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

// The project-native investment domain: work units, work items and project
// membership are seeded into a fresh database built from the production DDL
// and read back through ReadProjectThemeMix. The expected value for every
// project comes from ptmOracle, which models the attribution rule over the
// fixture's own fields and never renders SQL.

const (
	ptmOrg      = "ptm-org"
	ptmOtherOrg = "ptm-other-org"
	ptmRepo     = "22222222-2222-4222-8222-000000000001"
	ptmRepoB    = "22222222-2222-4222-8222-000000000002"
	ptmRepoC    = "22222222-2222-4222-8222-000000000003"
)

type ptmUnit struct {
	id      string
	org     string
	effort  float64
	themes  map[string]float64
	bugfix  float64
	issues  []string
	prs     []string
	version int // computed_at ordinal; the highest version is the live row
}

type ptmItem struct {
	id      string
	org     string
	project string // "" = a project-less item
	repo    string // "" = the default repository
}

var ptmThemeKeys = []string{"feature_delivery", "operational", "maintenance", "quality", "risk"}

func ptmTheme(key string) map[string]float64 {
	out := map[string]float64{}
	for _, k := range ptmThemeKeys {
		out[k] = 0
	}
	out[key] = 1
	return out
}

var ptmProjects = []string{"proj-a", "proj-b", "proj-zero-effort", "proj-no-units", "proj-keyed", "proj-dup", "proj-amb-a", "proj-amb-b", "proj-multi-a", "proj-multi-b"}

func ptmItems() []ptmItem {
	return []ptmItem{
		{id: "linear:A-1", org: ptmOrg, project: "proj-a"},
		{id: "linear:A-2", org: ptmOrg, project: "proj-a"},
		{id: "linear:B-1", org: ptmOrg, project: "proj-b"},
		{id: "linear:Z-1", org: ptmOrg, project: "proj-zero-effort"},
		{id: "linear:NOPROJ-1", org: ptmOrg, project: ""},
		{id: "linear:O-1", org: ptmOtherOrg, project: "proj-a"},
		{id: "linear:D-1", org: ptmOrg, project: "proj-dup"},
		// One item, one repository, an inconsistent move history that leaves it
		// in two projects: unambiguous as an item, so it counts for both.
		{id: "linear:MULTI-1", org: ptmOrg, project: ""},
		// The same item id under two repositories, in two projects: ambiguous,
		// so it attributes to neither.
		{id: "linear:AMB-1", org: ptmOrg, project: "proj-amb-a", repo: ptmRepoB},
		{id: "linear:AMB-1", org: ptmOrg, project: "proj-amb-b", repo: ptmRepoC},
		{id: "linear:K-1", org: ptmOrg, project: "proj-keyed"},
		{id: "linear:K-2", org: ptmOrg, project: "KEYED"},
	}
}

func ptmUnits() []ptmUnit {
	return []ptmUnit{
		// Superseded versions must never be read, whichever order the parts
		// were written in: one unit has its live version first, one last.
		{id: "u-a-single", org: ptmOrg, effort: 100, themes: ptmTheme("feature_delivery"), issues: []string{"linear:A-1"}, version: 2},
		{id: "u-a-single", org: ptmOrg, effort: 9999, themes: ptmTheme("risk"), issues: []string{"linear:B-1"}, version: 1},
		{id: "u-b-versioned", org: ptmOrg, effort: 7777, themes: ptmTheme("risk"), issues: []string{"linear:A-2"}, version: 1},
		{id: "u-b-versioned", org: ptmOrg, effort: 3, themes: ptmTheme("quality"), issues: []string{"linear:B-1"}, version: 2},
		// The same issue named twice: one unit.
		{id: "u-dup-ref", org: ptmOrg, effort: 5, themes: ptmTheme("feature_delivery"), issues: []string{"linear:A-2", "linear:A-2"}, version: 1},
		// Negative effort is not weight: it is reported and adds nothing.
		{id: "u-negative", org: ptmOrg, effort: -30, themes: map[string]float64{"feature_delivery": 0.2, "operational": 0.2, "maintenance": 0.2, "quality": 0.2, "risk": 0.2}, bugfix: 1, issues: []string{"linear:B-1"}, version: 1},
		{id: "u-multi", org: ptmOrg, effort: 12, themes: ptmTheme("maintenance"), issues: []string{"linear:MULTI-1"}, version: 1},
		// Evidence naming an item id that sits under two repositories.
		{id: "u-ambiguous", org: ptmOrg, effort: 15, themes: ptmTheme("risk"), issues: []string{"linear:AMB-1"}, version: 1},
		// One item id shared by two providers' projects: a unit in each, spanning.
		{id: "u-dup", org: ptmOrg, effort: 25, themes: ptmTheme("operational"), issues: []string{"linear:D-1"}, version: 1},
		// One project reached under both its id and its key: one unit, one project.
		{id: "u-keyed", org: ptmOrg, effort: 40, themes: ptmTheme("maintenance"), issues: []string{"linear:K-1", "linear:K-2"}, version: 1},
		// Two issues of the SAME project: counted once for that project.
		{id: "u-a-twice", org: ptmOrg, effort: 50, themes: ptmTheme("quality"), bugfix: 0.5, issues: []string{"linear:A-1", "linear:A-2"}, version: 1},
		// Spans proj-a and proj-b: counted in full for each.
		{id: "u-span", org: ptmOrg, effort: 10, themes: ptmTheme("risk"), issues: []string{"linear:A-1", "linear:B-1"}, version: 1},
		// Mixed distribution, only proj-b.
		{id: "u-b-mixed", org: ptmOrg, effort: 20, themes: map[string]float64{"feature_delivery": 0.25, "operational": 0.25, "maintenance": 0.25, "quality": 0.25, "risk": 0}, issues: []string{"linear:B-1"}, version: 1},
		// Zero effort: reported in work_units, adds nothing to the sums.
		{id: "u-zero-effort", org: ptmOrg, effort: 0, themes: map[string]float64{"feature_delivery": 0.2, "operational": 0.2, "maintenance": 0.2, "quality": 0.2, "risk": 0.2}, issues: []string{"linear:Z-1"}, version: 1},
		// Evidence names only a project-less item: attributed to no project.
		{id: "u-no-project", org: ptmOrg, effort: 70, themes: ptmTheme("maintenance"), issues: []string{"linear:NOPROJ-1"}, version: 1},
		// Evidence is pull requests only: no issue ref, so no project.
		{id: "u-pr-only", org: ptmOrg, effort: 80, themes: ptmTheme("operational"), prs: []string{ptmRepo + "#pr7"}, version: 1},
		// Another organization's unit over another organization's item.
		{id: "u-other-org", org: ptmOtherOrg, effort: 500, themes: ptmTheme("risk"), issues: []string{"linear:O-1"}, version: 1},
		// A unit in this org whose issue ref names another org's item.
		{id: "u-cross-org-ref", org: ptmOrg, effort: 60, themes: ptmTheme("quality"), issues: []string{"linear:O-1"}, version: 1},
	}
}

type ptmWant struct {
	sums                             map[string]float64
	bugfix                           float64
	workUnits, effortUnits, spanning uint64
}

// ptmOracle models the attribution rule over the fixture only.
func ptmOracle(units []ptmUnit, items []ptmItem) map[string]ptmWant {
	itemProject := map[string][]string{}
	repos := map[string]map[string]bool{}
	for _, item := range items {
		if item.org == ptmOrg {
			if repos[item.id] == nil {
				repos[item.id] = map[string]bool{}
			}
			repos[item.id][item.repo] = true
		}
	}
	for _, item := range items {
		if len(repos[item.id]) > 1 {
			continue
		}
		if item.org != ptmOrg || (item.project == "" && item.id != "linear:MULTI-1") {
			continue
		}
		// A project answers to its id and, when it has one, its key.
		switch item.project {
		case "KEYED":
			itemProject[item.id] = []string{"linear:proj-keyed"}
		case "":
			if item.id == "linear:MULTI-1" {
				itemProject[item.id] = []string{"linear:proj-multi-a", "linear:proj-multi-b"}
			}
		case "proj-dup":
			// The same id under two providers: membership does not compare provider.
			itemProject[item.id] = []string{"jira:proj-dup", "linear:proj-dup"}
		default:
			itemProject[item.id] = []string{"linear:" + item.project}
		}
	}
	latest := map[string]ptmUnit{}
	for _, unit := range units {
		if unit.org != ptmOrg {
			continue
		}
		if cur, ok := latest[unit.id]; !ok || unit.version > cur.version {
			latest[unit.id] = unit
		}
	}
	unitProjects := map[string]map[string]bool{}
	for id, unit := range latest {
		for _, issue := range unit.issues {
			for _, project := range itemProject[issue] {
				if unitProjects[id] == nil {
					unitProjects[id] = map[string]bool{}
				}
				unitProjects[id][project] = true
			}
		}
	}
	want := map[string]ptmWant{}
	for id, projects := range unitProjects {
		unit := latest[id]
		for project := range projects {
			w, ok := want[project]
			if !ok {
				w = ptmWant{sums: map[string]float64{}}
			}
			w.workUnits++
			if unit.effort > 0 {
				w.effortUnits++
				for k, share := range unit.themes {
					w.sums[k] += share * unit.effort
				}
				w.bugfix += unit.bugfix * unit.effort
			}
			if len(projects) > 1 {
				w.spanning++
			}
			want[project] = w
		}
	}
	return want
}

func ptmItemRepo(item ptmItem) string {
	if item.repo != "" {
		return item.repo
	}
	return ptmRepo
}

func ptmSeed(items []ptmItem, units []ptmUnit) []string {
	const at = "'2026-03-01 00:00:00'"
	var stmts []string
	stmts = append(stmts, fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('proj-dup', '%s', 'jira', NULL, 'dup', 1, 'started', '', %s)", ptmOrg, at))
	for _, id := range ptmProjects {
		key := "NULL"
		if id == "proj-keyed" {
			key = "'KEYED'"
		}
		stmts = append(stmts, fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('%s', '%s', 'linear', %s, '%s', 1, 'started', '', %s)", id, ptmOrg, key, id, at))
	}
	stmts = append(stmts, fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('proj-a', '%s', 'linear', NULL, 'other', 1, 'started', '', %s)", ptmOtherOrg, at))
	transition := func(event, from, to, occurred string) string {
		return fmt.Sprintf("INSERT INTO project_membership_transitions (org_id, source_id, repo_id, subject_kind, subject_id, provider, from_project_id, to_project_id, from_project_key, to_project_key, actor, occurred_at, last_synced, event_id) VALUES ('%s', NULL, '%s', 'work_item', 'linear:MULTI-1', 'linear', '%s', '%s', '', '', 'test', '%s', %s, '%s')",
			ptmOrg, ptmRepo, from, to, occurred, at, event)
	}
	stmts = append(stmts,
		transition("e1", "proj-multi-a", "proj-multi-b", "2026-02-01 00:00:00"),
		transition("e2", "proj-multi-c", "proj-multi-a", "2026-02-02 00:00:00"),
	)
	for _, item := range items {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO work_items (repo_id, work_item_id, provider, title, type, status, project_key, project_id, native_team_key, project_name, created_at, updated_at, completed_at, parent_id, url, last_synced, org_id) VALUES ('%s', '%s', 'linear', 'title', 'issue', 'open', '', '%s', '', '', %s, %s, NULL, '', '', %s, '%s')",
			ptmItemRepo(item), item.id, item.project, at, at, at, item.org))
	}
	for _, unit := range units {
		refs := func(values []string) string {
			quoted := make([]string, len(values))
			for i, v := range values {
				quoted[i] = `"` + v + `"`
			}
			return "[" + strings.Join(quoted, ",") + "]"
		}
		evidence := fmt.Sprintf(`{"issues": %s, "prs": %s, "commits": [], "edges": []}`, refs(unit.issues), refs(unit.prs))
		keys := make([]string, 0, len(unit.themes))
		for k := range unit.themes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, fmt.Sprintf("'%s', %g", k, unit.themes[k]))
		}
		stmts = append(stmts, fmt.Sprintf("INSERT INTO work_unit_investments (work_unit_id, from_ts, to_ts, effort_value, theme_distribution_json, subcategory_distribution_json, structural_evidence_json, computed_at, org_id) VALUES ('%s', '2026-02-01 00:00:00', '2026-04-01 00:00:00', %g, map(%s), map('quality.bugfix', %g), '%s', '2026-03-0%d 00:00:00', '%s')",
			unit.id, unit.effort, strings.Join(pairs, ", "), unit.bugfix, strings.ReplaceAll(evidence, "'", "''"), unit.version, unit.org))
	}
	return stmts
}

func ptmDatabase(t *testing.T, items []ptmItem, units []ptmUnit) *clickhouse.Client {
	t.Helper()
	required := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1"
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" || os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required when reader integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required for the real project theme mix test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN: %v", err)
	}
	database := fmt.Sprintf("ptm_mix_%d", rand.Uint64())
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
	stmts := schema.DDL("projects", "work_items", "work_unit_investments", "project_membership_transitions")
	stmts = append(stmts, schema.ProjectMembershipPresenceViewDDL)
	ddlCount := len(stmts)
	stmts = append(stmts, ptmSeed(items, units)...)
	// Keep superseded work unit versions unmerged so a read that skips the
	// latest-version rule sees them. Best effort: a role without the
	// privilege still runs every assertion, only with the versions merged.
	for i, stmt := range stmts {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed statement %q: %v", stmt, err)
		}
		if i == ddlCount-1 {
			_ = seed.Exec(ctx, "SYSTEM STOP MERGES work_unit_investments")
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

func ptmClose(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestIntegrationReadProjectThemeMix(t *testing.T) {
	items, units := ptmItems(), ptmUnits()
	client := ptmDatabase(t, items, units)
	ids := make([]string, 0, len(ptmProjects))
	for _, p := range ptmProjects {
		ids = append(ids, "linear:"+p)
	}
	ids = append(ids, "jira:proj-dup")
	rows, err := readers.ReadProjectThemeMix(context.Background(), client, ptmOrg, ids, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadProjectThemeMix() error = %v", err)
	}
	want := ptmOracle(units, items)
	got := map[string]readers.ProjectThemeMixRow{}
	for _, row := range rows {
		got[row.ProjectSubjectKey] = row
	}
	if len(got) != len(want) {
		t.Fatalf("projects with a row = %v, want %d oracle projects %v", got, len(want), want)
	}
	for project, w := range want {
		row, ok := got[project]
		if !ok {
			t.Fatalf("project %s has no row; want %#v", project, w)
		}
		sums := map[string]float64{
			"feature_delivery": row.FeatureDelivery, "operational": row.Operational, "maintenance": row.Maintenance,
			"quality": row.Quality, "risk": row.Risk,
		}
		for _, k := range ptmThemeKeys {
			if !ptmClose(sums[k], w.sums[k]) {
				t.Errorf("%s %s = %v, want %v", project, k, sums[k], w.sums[k])
			}
		}
		if !ptmClose(row.BugfixWeighted, w.bugfix) {
			t.Errorf("%s bugfix = %v, want %v", project, row.BugfixWeighted, w.bugfix)
		}
		if row.WorkUnits != w.workUnits || row.EffortUnits != w.effortUnits || row.SpanningUnits != w.spanning {
			t.Errorf("%s population = (%d,%d,%d), want (%d,%d,%d)", project, row.WorkUnits, row.EffortUnits, row.SpanningUnits, w.workUnits, w.effortUnits, w.spanning)
		}
	}
	// Named absences: a project with no units, a project whose only unit
	// lives in another organization, and the no-project / PR-only /
	// cross-org-ref units all leave no row and no contribution.
	for _, absent := range []string{"linear:proj-no-units"} {
		if _, ok := got[absent]; ok {
			t.Errorf("project %s has a row; it has no attributable unit in this organization", absent)
		}
	}
	// A project whose only unit carries zero effort reports the unit and no weight.
	zero := got["linear:proj-zero-effort"]
	if zero.WorkUnits != 1 || zero.EffortUnits != 0 || zero.FeatureDelivery != 0 {
		t.Errorf("zero-effort project row = %#v", zero)
	}
	// Spanning is disclosed for both projects it touches.
	if got["linear:proj-a"].SpanningUnits != 1 || got["linear:proj-b"].SpanningUnits != 1 {
		t.Errorf("spanning = (%d,%d), want (1,1)", got["linear:proj-a"].SpanningUnits, got["linear:proj-b"].SpanningUnits)
	}
	// The shared id resolves under both providers, each with its own row.
	if got["linear:proj-dup"].WorkUnits != 1 || got["jira:proj-dup"].WorkUnits != 1 || got["jira:proj-dup"].SpanningUnits != 1 {
		t.Errorf("dup rows = %#v / %#v", got["linear:proj-dup"], got["jira:proj-dup"])
	}
	// Spanning counts every project in the organization, not only the requested ones.
	subset, err := readers.ReadProjectThemeMix(context.Background(), client, ptmOrg, []string{"linear:proj-a"}, readers.TimeBound{})
	if err != nil || len(subset) != 1 || subset[0].SpanningUnits != 1 {
		t.Errorf("subset rows = %#v, err = %v, want proj-a alone with spanning 1", subset, err)
	}
	// The superseded version's weight never appears.
	if got["linear:proj-a"].Risk > 10.0+1e-9 {
		t.Errorf("proj-a risk = %v: a superseded work unit version was read", got["linear:proj-a"].Risk)
	}
}

func TestIntegrationReadProjectThemeMixOrgScope(t *testing.T) {
	items, units := ptmItems(), ptmUnits()
	client := ptmDatabase(t, items, units)
	rows, err := readers.ReadProjectThemeMix(context.Background(), client, ptmOtherOrg, []string{"linear:proj-a"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadProjectThemeMix() error = %v", err)
	}
	if len(rows) != 1 || rows[0].ProjectSubjectKey != "linear:proj-a" || rows[0].WorkUnits != 1 || !ptmClose(rows[0].Risk, 500) {
		t.Fatalf("rows = %#v, want the other organization's own single unit only", rows)
	}
	// The other organization's item shares this organization's project id and
	// must add nothing to it.
	own, err := readers.ReadProjectThemeMix(context.Background(), client, ptmOrg, []string{"linear:proj-a"}, readers.TimeBound{})
	if err != nil || len(own) != 1 || own[0].Risk > 10.0+1e-9 {
		t.Fatalf("rows = %#v, err = %v, want this organization's proj-a without the other organization's weight", own, err)
	}
}
