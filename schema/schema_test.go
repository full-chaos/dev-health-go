package schema

import "testing"

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

// teamCognitiveLoadDailyExpectedColumns is teamCognitiveLoadDailyDDL's
// column list, transcribed by hand in production-position order (this
// table's ops migration is also its only writer to date, so migration
// order and position order coincide -- unlike most entries in
// ProductionColumns, which are read live off drifted production tables).
var teamCognitiveLoadDailyExpectedColumns = []Column{
	{Name: "org_id", Type: "String"},
	{Name: "team_id", Type: "String"},
	{Name: "day", Type: "Date"},
	{Name: "pr_interruption_load", Type: "Float64"},
	{Name: "context_spread_count", Type: "Float64"},
	{Name: "review_request_load", Type: "Float64"},
	{Name: "after_hours_commit_ratio", Type: "Nullable(Float64)"},
	{Name: "weekend_commit_ratio", Type: "Nullable(Float64)"},
	{Name: "contributing_repo_count", Type: "UInt32"},
	{Name: "sample_author_count", Type: "UInt32"},
	{Name: "computed_at", Type: "DateTime64(6, 'UTC')"},
}

const teamCognitiveLoadDailyExpectedEngine = "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, team_id, day) SETTINGS index_granularity = 8192"

func TestTeamCognitiveLoadDailyMatchesOpsMigrationDDL(t *testing.T) {
	if teamCognitiveLoadDailyDDL == "" {
		t.Fatal("DDL constant must not be empty -- guards against an accidental blank pin")
	}

	got, ok := ProductionColumns["team_cognitive_load_daily"]
	if !ok {
		t.Fatal(`ProductionColumns["team_cognitive_load_daily"] is not declared`)
	}
	if len(got) != len(teamCognitiveLoadDailyExpectedColumns) {
		t.Fatalf(
			"column count mismatch: ProductionColumns has %d, ops migration DDL has %d\n"+
				"got:      %+v\nexpected: %+v",
			len(got), len(teamCognitiveLoadDailyExpectedColumns),
			got, teamCognitiveLoadDailyExpectedColumns,
		)
	}
	for i, want := range teamCognitiveLoadDailyExpectedColumns {
		if got[i] != want {
			t.Errorf("column %d: got %+v, want %+v (from ops migration DDL)", i, got[i], want)
		}
	}

	gotEngine, ok := EngineFull["team_cognitive_load_daily"]
	if !ok {
		t.Fatal(`EngineFull["team_cognitive_load_daily"] is not declared`)
	}
	if gotEngine != teamCognitiveLoadDailyExpectedEngine {
		t.Errorf(
			"engine mismatch: got %q, want %q (derived from the ops migration DDL's "+
				"ENGINE/PARTITION BY/ORDER BY clauses)",
			gotEngine, teamCognitiveLoadDailyExpectedEngine,
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
