package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadTeamReadiness(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM estimate_coverage_metrics_daily", rows: [][]any{
				{"CHAOS", "scope-1", "linear", "2026-02-22", int64(18), int64(2), int64(20), uint8(1), float64(0.9)},
			}},
		}}
		rows, err := readers.ReadTeamReadiness(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamReadiness() error = %v", err)
		}
		want := readers.ReadinessCoverageRow{
			TeamID: "CHAOS", WorkScopeID: "scope-1", Provider: "linear", Day: "2026-02-22",
			EstimatedCount: 18, UnestimatedCount: 2, BacklogSize: 20, HasRatio: 1, Ratio: 0.9,
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

	t.Run("no ratio still scans", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM estimate_coverage_metrics_daily", rows: [][]any{
				{"CHAOS", "scope-1", "linear", "2026-02-22", int64(18), int64(2), int64(20), uint8(0), float64(0)},
			}},
		}}
		rows, err := readers.ReadTeamReadiness(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamReadiness() error = %v", err)
		}
		if len(rows) != 1 || rows[0].HasRatio != 0 {
			t.Fatalf("rows = %#v, want HasRatio=0", rows)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM estimate_coverage_metrics_daily", err: errors.New("boom")}}}
		rows, err := readers.ReadTeamReadiness(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})

	t.Run("active time bound reaches the statement and bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM estimate_coverage_metrics_daily", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadTeamReadiness(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadTeamReadiness() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "toDate({"+readers.BoundEndParam+":DateTime64(6,'UTC')})") {
			t.Fatalf("statement = %q, want the day-bound predicate", statement)
		}
	})
}

func TestReadProjectReadiness(t *testing.T) {
	t.Parallel()
	t.Run("happy path returns one row per work scope the project owns", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM estimate_coverage_metrics_daily", rows: [][]any{
			{"linear:proj-1", uint8(1), "team-1", "Team One", "scope-a", "linear", "2026-02-22", int64(18), int64(2), int64(20), uint8(1), float64(0.9)},
			{"linear:proj-1", uint8(1), "team-2", "Team Two", "scope-b", "gitlab", "2026-02-22", int64(5), int64(15), int64(20), uint8(1), float64(0.25)},
		}}}}
		rows, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectReadiness() error = %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %#v, want 2", rows)
		}
		if rows[0].TeamID != "team-1" || rows[0].EstimatedCount != 18 {
			t.Fatalf("rows[0] = %#v", rows[0])
		}
		if rows[1].TeamID != "team-2" || rows[1].Ratio != 0.25 {
			t.Fatalf("rows[1] = %#v", rows[1])
		}
	})

	t.Run("empty ids short-circuits without querying", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM estimate_coverage_metrics_daily", rows: [][]any{{"linear:proj-1", uint8(1), "team-1", "Team One", "scope-a", "linear", "2026-02-22", int64(1), int64(1), int64(1), uint8(1), float64(1)}}}}}
		rows, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, nil)", rows, err)
		}
		if len(client.queries) != 0 {
			t.Fatalf("queries = %#v, want no query issued for empty ids", client.queries)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM estimate_coverage_metrics_daily", err: errors.New("boom")}}}
		rows, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})
}
