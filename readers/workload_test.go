package readers_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadTeamWorkload(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM capacity_forecasts", rows: [][]any{
				{"CHAOS", "scope-a", float64(3.2), float64(0.8), uint8(1), int64(14), uint8(0), uint8(1), int64(120), "2026-07-27 04:00:00"},
			}},
		}}
		rows, err := readers.ReadTeamWorkload(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamWorkload() error = %v", err)
		}
		want := readers.WorkloadForecastRow{
			TeamID: "CHAOS", WorkScopeID: "scope-a", ThroughputMean: 3.2, ThroughputStddev: 0.8,
			HasP50Days: 1, P50Days: 14, InsufficientHistory: 0, HighVariance: 1, BacklogSize: 120,
			ComputedAt: "2026-07-27 04:00:00",
		}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want [%#v]", rows, want)
		}
		if client.orgIDBinding() != "org-1" {
			t.Fatalf("org_id binding = %q", client.orgIDBinding())
		}
		if got := client.idsBinding(); len(got) != 1 || got[0] != "CHAOS" {
			t.Fatalf("ids binding = %#v", got)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", err: errors.New("boom")}}}
		rows, err := readers.ReadTeamWorkload(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})

	t.Run("active time bound reaches the statement and bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadTeamWorkload(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadTeamWorkload() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "computed_at <= {"+readers.BoundEndParam+":DateTime64(6,'UTC')}") {
			t.Fatalf("statement = %q, want the timestamp-bound predicate", statement)
		}
	})
}

func TestReadProjectWorkload(t *testing.T) {
	t.Parallel()
	t.Run("happy path returns one row per work scope the project owns", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", rows: [][]any{
			{"linear:proj-1", uint8(1), "team-1", "Team One", "scope-a", float64(3.2), float64(0.8), uint8(0), int64(0), uint8(0), uint8(1), int64(120), "2026-07-27 04:00:00"},
			{"linear:proj-1", uint8(1), "team-2", "Team Two", "scope-b", float64(9.0), float64(2.1), uint8(0), int64(0), uint8(0), uint8(0), int64(40), "2026-07-27 04:00:00"},
		}}}}
		rows, err := readers.ReadProjectWorkload(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectWorkload() error = %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %#v, want 2", rows)
		}
		if rows[0].TeamID != "team-1" || rows[0].ThroughputMean != 3.2 {
			t.Fatalf("rows[0] = %#v", rows[0])
		}
		if rows[1].TeamID != "team-2" || rows[1].BacklogSize != 40 {
			t.Fatalf("rows[1] = %#v", rows[1])
		}
	})

	t.Run("empty ids short-circuits without querying", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", rows: [][]any{{"linear:proj-1", uint8(1), "team-1", "Team One", "scope-a", float64(1), float64(1), uint8(0), int64(0), uint8(0), uint8(0), int64(1), "2026-07-27 04:00:00"}}}}}
		rows, err := readers.ReadProjectWorkload(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, nil)", rows, err)
		}
		if len(client.queries) != 0 {
			t.Fatalf("queries = %#v, want no query issued for empty ids", client.queries)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", err: errors.New("boom")}}}
		rows, err := readers.ReadProjectWorkload(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})
}

func TestReadTeamCompletionForecasts(t *testing.T) {
	t.Parallel()
	t.Run("scans every column verbatim", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM capacity_forecasts", rows: [][]any{
				{"CHAOS", uint8(0), "", "2026-10-30 04:00:00.000", uint32(10000), int64(42),
					uint8(1), int64(12), uint8(1), int64(20), uint8(0), int64(0),
					"2026-11-11", "2026-11-19", "", uint8(0), uint8(1),
					uint64(3), uint64(3), []uint16{5, 12, 365}, []uint32{100, 9000, 900}},
			}},
		}}
		rows, err := readers.ReadTeamCompletionForecasts(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamCompletionForecasts() error = %v", err)
		}
		want := []readers.CompletionForecastRow{{
			TeamID: "CHAOS", HasWorkScope: 0, WorkScopeID: "", ComputedAt: "2026-10-30 04:00:00.000",
			SimulationCount: 10000, BacklogSize: 42,
			HasP50Days: 1, P50Days: 12, HasP85Days: 1, P85Days: 20, HasP95Days: 0, P95Days: 0,
			P50Date: "2026-11-11", P85Date: "2026-11-19", P95Date: "", InsufficientHistory: 0, HighVariance: 1,
			CompletionDayBins: 3, CompletionCountBins: 3,
			CompletionDaysValues: []uint16{5, 12, 365}, CompletionDaysCounts: []uint32{100, 9000, 900},
		}}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
		if client.orgIDBinding() != "org-1" {
			t.Fatalf("org_id binding = %q", client.orgIDBinding())
		}
		if got := client.idsBinding(); len(got) != 1 || got[0] != "CHAOS" {
			t.Fatalf("ids binding = %#v", got)
		}
		statement := client.queries[0].statement
		for _, part := range []string{
			// The latest row per (team, scope), with scope partitioned as stored.
			"PARTITION BY team_id, work_scope_id ORDER BY computed_at DESC, forecast_id DESC",
			"toUInt8(isNotNull(work_scope_id)) AS has_work_scope",
			// One entry past the limit is read from each array, and both stored lengths.
			"arraySlice(completion_days_values, 1, 367)",
			"arraySlice(completion_days_counts, 1, 367)",
			"toUInt64(length(completion_days_values))",
			"toUInt64(length(completion_days_counts))",
			"\nLIMIT 200",
		} {
			if !strings.Contains(statement, part) {
				t.Errorf("statement lacks %q:\n%s", part, statement)
			}
		}
	})

	t.Run("no ids reads nothing", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		rows, err := readers.ReadTeamCompletionForecasts(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil || len(client.queries) != 0 {
			t.Fatalf("rows = %#v, err = %v, queries = %d, want no read", rows, err, len(client.queries))
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", err: errors.New("boom")}}}
		rows, err := readers.ReadTeamCompletionForecasts(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})

	t.Run("time bound and row limit reach the statement", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM capacity_forecasts", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadTeamCompletionForecastsWithRowLimit(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{Active: true, End: end}, readers.ProbeRowLimit)
		if err != nil {
			t.Fatalf("ReadTeamCompletionForecastsWithRowLimit() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "computed_at <= {"+readers.BoundEndParam+":DateTime64(6,'UTC')}") {
			t.Fatalf("statement = %q, want the timestamp-bound predicate", statement)
		}
		if !strings.HasSuffix(statement, "\nLIMIT 201") {
			t.Fatalf("statement = %q, want LIMIT 201", statement)
		}
	})
}
