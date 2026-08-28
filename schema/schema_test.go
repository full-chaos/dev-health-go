package schema

import (
	"regexp"
	"strings"
	"testing"
)

// teamCognitiveLoadDailyDDL is the ops migration this table is declared
// from, copied verbatim (CHAOS-4365 item 2 / 4347-C,
// src/dev_health_ops/migrations/clickhouse/081_team_cognitive_load_daily.sql
// in full-chaos/dev-health-ops). This package has no access to that repo at
// build/test time, so the DDL is pinned here as the source of truth this
// test checks ProductionColumns/EngineFull against -- a future edit to
// either the ops migration or this map, without updating the other, is
// caught by re-deriving the expected column list from this string by hand
// and comparing.
const teamCognitiveLoadDailyDDL = `
CREATE TABLE IF NOT EXISTS team_cognitive_load_daily
(
    org_id                    String,
    team_id                   String,
    day                       Date,

    pr_interruption_load      Float64,
    context_spread_count      Float64,
    review_request_load       Float64,

    after_hours_commit_ratio  Nullable(Float64),
    weekend_commit_ratio      Nullable(Float64),

    contributing_repo_count   UInt32,
    sample_author_count       UInt32,

    computed_at               DateTime64(6, 'UTC')
) ENGINE = MergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (org_id, team_id, day);
`

// columnListRE captures the column-definition block: everything between the
// table's opening "(" and the ")" that precedes "ENGINE" -- tolerant of the
// blank separator lines and alignment padding in the pinned DDL.
var columnListRE = regexp.MustCompile(`(?s)\(\s*\n(.*?)\n\)\s*ENGINE\s*=\s*(\w+)`)

// columnLineRE splits one column-definition line into its name and type.
// SplitN on the first run of whitespace would work for every type here
// except DateTime64(6, 'UTC'), whose argument list itself contains a space
// -- so this captures everything after the name as the type instead.
var columnLineRE = regexp.MustCompile(`^(\S+)\s+(.+?),?$`)

var partitionByRE = regexp.MustCompile(`PARTITION BY\s+(.+)`)
var orderByRE = regexp.MustCompile(`ORDER BY\s+(\([^)]*\))`)

// parseColumnsFromDDL derives the expected ProductionColumns entry directly
// from teamCognitiveLoadDailyDDL, so a future edit to either the ops
// migration string or the map, without updating the other, is caught by
// actual parsing -- not by a second hand-transcribed list that could drift
// from the DDL the same way the map itself could (codex R1: a prior version
// of this test only checked the DDL constant was non-empty and compared
// ProductionColumns against a separately hand-maintained slice, so editing
// the DDL string alone would still pass).
func parseColumnsFromDDL(t *testing.T, ddl string) ([]Column, string) {
	t.Helper()

	m := columnListRE.FindStringSubmatch(ddl)
	if m == nil {
		t.Fatalf("could not locate column-list block in teamCognitiveLoadDailyDDL: %q", ddl)
	}
	block, engineName := m[1], m[2]

	var columns []Column
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lm := columnLineRE.FindStringSubmatch(line)
		if lm == nil {
			t.Fatalf("could not parse column line %q in teamCognitiveLoadDailyDDL", line)
		}
		columns = append(columns, Column{Name: lm[1], Type: lm[2]})
	}
	if len(columns) == 0 {
		t.Fatal("parsed zero columns from teamCognitiveLoadDailyDDL -- regexp likely out of sync with the DDL shape")
	}

	pm := partitionByRE.FindStringSubmatch(ddl)
	if pm == nil {
		t.Fatal("could not locate PARTITION BY clause in teamCognitiveLoadDailyDDL")
	}
	om := orderByRE.FindStringSubmatch(ddl)
	if om == nil {
		t.Fatal("could not locate ORDER BY clause in teamCognitiveLoadDailyDDL")
	}
	// SETTINGS index_granularity = 8192 is ClickHouse's implicit default --
	// it never appears in migration DDL (CREATE TABLE omits it and the
	// server fills it in), but every other EngineFull entry in this package
	// is captured verbatim from a live `SHOW CREATE TABLE`, which DOES spell
	// it out. This table isn't live yet (see the package-doc exception at
	// its ProductionColumns entry), so the suffix can't be parsed from
	// anywhere -- it is appended here to match that convention, not derived.
	engine := engineName + " PARTITION BY " + strings.TrimSpace(pm[1]) +
		" ORDER BY " + om[1] + " SETTINGS index_granularity = 8192"

	return columns, engine
}

func TestTeamCognitiveLoadDailyMatchesOpsMigrationDDL(t *testing.T) {
	if teamCognitiveLoadDailyDDL == "" {
		t.Fatal("DDL constant must not be empty -- guards against an accidental blank pin")
	}

	wantColumns, wantEngine := parseColumnsFromDDL(t, teamCognitiveLoadDailyDDL)

	got, ok := ProductionColumns["team_cognitive_load_daily"]
	if !ok {
		t.Fatal(`ProductionColumns["team_cognitive_load_daily"] is not declared`)
	}
	if len(got) != len(wantColumns) {
		t.Fatalf(
			"column count mismatch: ProductionColumns has %d, ops migration DDL has %d\n"+
				"got:      %+v\nexpected: %+v",
			len(got), len(wantColumns),
			got, wantColumns,
		)
	}
	for i, want := range wantColumns {
		if got[i] != want {
			t.Errorf("column %d: got %+v, want %+v (parsed from ops migration DDL)", i, got[i], want)
		}
	}

	gotEngine, ok := EngineFull["team_cognitive_load_daily"]
	if !ok {
		t.Fatal(`EngineFull["team_cognitive_load_daily"] is not declared`)
	}
	if gotEngine != wantEngine {
		t.Errorf(
			"engine mismatch: got %q, want %q (parsed from the ops migration DDL's "+
				"ENGINE/PARTITION BY/ORDER BY clauses, plus the implicit SETTINGS suffix)",
			gotEngine, wantEngine,
		)
	}
}

// TestTeamCognitiveLoadDailyOrderByColumnsExist catches the class of drift
// a straight string comparison can miss silently if someone "fixes" the
// engine string without updating the columns (or vice versa): every column
// named in the ORDER BY clause must actually be declared.
func TestTeamCognitiveLoadDailyOrderByColumnsExist(t *testing.T) {
	declared := map[string]bool{}
	for _, c := range ProductionColumns["team_cognitive_load_daily"] {
		declared[c.Name] = true
	}
	for _, name := range []string{"org_id", "team_id", "day"} {
		if !declared[name] {
			t.Errorf("ORDER BY column %q is not in ProductionColumns[\"team_cognitive_load_daily\"]", name)
		}
	}
}
