package schema

import (
	"strings"
	"testing"
)

// teamComplexityDailyDDL is the ops migration this table is declared from,
// copied verbatim (CHAOS-4365 item 3 / 4347-C,
// src/dev_health_ops/migrations/clickhouse/082_team_complexity_daily.sql in
// full-chaos/dev-health-ops). Same pin-and-parse contract as
// teamCognitiveLoadDailyDDL in schema_test.go: this package has no access to
// that repo at build/test time, so an edit to either the ops migration or
// this map, without updating the other, is caught by re-deriving the
// expected column list from this string and comparing -- reuses
// parseColumnsFromDDL and the column/engine regexps schema_test.go already
// declares at package scope (not redefined here to avoid a duplicate
// declaration in the same package).
const teamComplexityDailyDDL = `
CREATE TABLE IF NOT EXISTS team_complexity_daily
(
    org_id                          String,
    team_id                         String,
    day                             Date,

    loc_total                       UInt64,
    cyclomatic_total                UInt64,
    cyclomatic_per_kloc             Float64,
    high_complexity_functions       UInt64,
    very_high_complexity_functions  UInt64,

    contributing_repo_count         UInt32,

    computed_at                     DateTime64(6, 'UTC')
) ENGINE = MergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (org_id, team_id, day);
`

func TestTeamComplexityDailyMatchesOpsMigrationDDL(t *testing.T) {
	if teamComplexityDailyDDL == "" {
		t.Fatal("DDL constant must not be empty -- guards against an accidental blank pin")
	}

	wantColumns, wantEngine := parseColumnsFromDDL(t, teamComplexityDailyDDL)

	got, ok := ProductionColumns["team_complexity_daily"]
	if !ok {
		t.Fatal(`ProductionColumns["team_complexity_daily"] is not declared`)
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

	gotEngine, ok := EngineFull["team_complexity_daily"]
	if !ok {
		t.Fatal(`EngineFull["team_complexity_daily"] is not declared`)
	}
	if gotEngine != wantEngine {
		t.Errorf(
			"engine mismatch: got %q, want %q (parsed from the ops migration DDL's "+
				"ENGINE/PARTITION BY/ORDER BY clauses, plus the implicit SETTINGS suffix)",
			gotEngine, wantEngine,
		)
	}
}

// TestTeamComplexityDailyOrderByColumnsExist mirrors
// TestTeamCognitiveLoadDailyOrderByColumnsExist: catches the class of drift
// a straight string comparison can miss silently if someone "fixes" the
// engine string without updating the columns (or vice versa). Unlike that
// sibling test (which hard-codes its 3-column literal), the expected
// column list here is derived from teamComplexityDailyDDL's own ORDER BY
// clause via the shared orderByRE regexp, so a future ORDER BY edit that
// adds/renames a key is caught even if this test body is never touched.
func TestTeamComplexityDailyOrderByColumnsExist(t *testing.T) {
	om := orderByRE.FindStringSubmatch(teamComplexityDailyDDL)
	if om == nil {
		t.Fatal("could not locate ORDER BY clause in teamComplexityDailyDDL")
	}
	orderByCols := strings.Split(strings.Trim(om[1], "()"), ",")

	declared := map[string]bool{}
	for _, c := range ProductionColumns["team_complexity_daily"] {
		declared[c.Name] = true
	}
	for _, raw := range orderByCols {
		name := strings.TrimSpace(raw)
		if !declared[name] {
			t.Errorf("ORDER BY column %q is not in ProductionColumns[\"team_complexity_daily\"]", name)
		}
	}
}
