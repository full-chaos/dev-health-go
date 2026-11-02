package schema

import (
	"strings"
	"testing"
)

// capacityForecastsBaselineDDL is the ops ClickHouse baseline's CREATE TABLE
// for capacity_forecasts, copied verbatim from
// internal/chmigrate/baseline/head.json in full-chaos/dev-health-ops at
// 8dcc0377 (CHAOS-12584). This package cannot read that repository at test time,
// so the baseline is pinned here. ProductionColumns declares only the columns
// the readers select, so the check is that every declared column appears in
// the baseline, in the same relative order, with the same type: a fixture
// rendered from ProductionColumns is then a positional replica of production
// for the columns it carries.
const capacityForecastsBaselineDDL = "" +
	"CREATE TABLE capacity_forecasts (`forecast_id` String, " +
	"`computed_at` DateTime64(3, 'UTC'), " +
	"`team_id` Nullable(String), " +
	"`work_scope_id` Nullable(String), " +
	"`backlog_size` UInt32, " +
	"`target_items` Nullable(UInt32), " +
	"`target_date` Nullable(Date), " +
	"`history_days` UInt16, " +
	"`simulation_count` UInt32, " +
	"`p50_days` Nullable(UInt16), " +
	"`p85_days` Nullable(UInt16), " +
	"`p95_days` Nullable(UInt16), " +
	"`p50_date` Nullable(Date), " +
	"`p85_date` Nullable(Date), " +
	"`p95_date` Nullable(Date), " +
	"`p50_items` Nullable(UInt32), " +
	"`p85_items` Nullable(UInt32), " +
	"`p95_items` Nullable(UInt32), " +
	"`throughput_mean` Float64, " +
	"`throughput_stddev` Float64, " +
	"`insufficient_history` UInt8 DEFAULT 0, " +
	"`high_variance` UInt8 DEFAULT 0, " +
	"`org_id` String DEFAULT 'default', " +
	"`completion_days_values` Array(UInt16) DEFAULT [] COMMENT 'Distinct completion-day values of the fixed-scope Monte Carlo run, ascending. Parallel to completion_days_counts. Empty = no distribution (rows before migration 101, or the mode did not run).', " +
	"`completion_days_counts` Array(UInt32) DEFAULT [] COMMENT 'How many simulation runs produced each completion_days_values entry. Sums to simulation_count when present.', " +
	"`completion_items_values` Array(UInt32) DEFAULT [] COMMENT 'Distinct items-completed values of the fixed-date Monte Carlo run, ascending. Parallel to completion_items_counts. Empty = no distribution (rows before migration 101, or the mode did not run).', " +
	"`completion_items_counts` Array(UInt32) DEFAULT [] COMMENT 'How many simulation runs produced each completion_items_values entry. Sums to simulation_count when present.', INDEX idx_team_scope (team_id, work_scope_id) TYPE bloom_filter GRANULARITY 4) ENGINE = ReplacingMergeTree(computed_at) ORDER BY (org_id, forecast_id) SETTINGS index_granularity = 8192"

// baselineColumns parses "`name` Type [DEFAULT x] [COMMENT '...']" column
// definitions out of a one-line baseline CREATE TABLE, and its ENGINE clause.
func baselineColumns(t *testing.T, ddl string) ([]Column, string) {
	t.Helper()
	open := strings.Index(ddl, "(`")
	indexAt := strings.Index(ddl, ", INDEX ")
	engineAt := strings.Index(ddl, ") ENGINE = ")
	if open < 0 || indexAt < open || engineAt < indexAt {
		t.Fatalf("baseline DDL does not have the expected shape: %q", ddl)
	}
	var columns []Column
	for _, part := range strings.Split(ddl[open+2:indexAt], ", `") {
		name, rest, ok := strings.Cut(part, "` ")
		if !ok {
			t.Fatalf("baseline column %q has no name", part)
		}
		for _, marker := range []string{" DEFAULT ", " COMMENT "} {
			if cut := strings.Index(rest, marker); cut >= 0 {
				rest = rest[:cut]
			}
		}
		columns = append(columns, Column{Name: name, Type: rest})
	}
	return columns, ddl[engineAt+len(") ENGINE = "):]
}

// TestCapacityForecastsMatchTheOpsBaseline checks the declared
// capacity_forecasts columns against the pinned ops baseline: each is a
// baseline column, in baseline order, with the baseline type, and the engine
// is the baseline engine.
func TestCapacityForecastsMatchTheOpsBaseline(t *testing.T) {
	baseline, engine := baselineColumns(t, capacityForecastsBaselineDDL)
	if len(baseline) != 27 {
		t.Fatalf("parsed %d baseline columns, want the 27 the ops baseline declares: %+v", len(baseline), baseline)
	}
	declared, ok := ProductionColumns["capacity_forecasts"]
	if !ok {
		t.Fatal(`ProductionColumns["capacity_forecasts"] is not declared`)
	}
	next := 0
	for _, column := range declared {
		found := false
		for next < len(baseline) {
			candidate := baseline[next]
			next++
			if candidate.Name == column.Name {
				if candidate.Type != column.Type {
					t.Errorf("column %s: declared %q, baseline %q", column.Name, column.Type, candidate.Type)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("column %s is not in the baseline after the column before it: declared out of order, or not a production column", column.Name)
		}
	}
	if got := EngineFull["capacity_forecasts"]; got != engine {
		t.Errorf("engine: declared %q, baseline %q", got, engine)
	}
}
