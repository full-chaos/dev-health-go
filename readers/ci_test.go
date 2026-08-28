package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadRunStatus(t *testing.T) {
	t.Parallel()

	t.Run("happy path, current axis", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", rows: [][]any{{"run-1", "success", "repo-1"}}}}}
		rows, err := readers.ReadRunStatus(context.Background(), client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadRunStatus() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.CIPipelineRunStatusRow{RunID: "run-1", Status: "success", RepoID: "repo-1"}) {
			t.Fatalf("rows = %#v", rows)
		}
		if got := client.orgIDBinding(); got != "org-1" {
			t.Fatalf("org_id binding = %q", got)
		}
	})

	t.Run("active time bound scopes and binds the requested instant", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", rows: [][]any{{"run-1", "running", "repo-1"}}}}}
		bound := readers.TimeBound{Active: true, End: time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)}
		rows, err := readers.ReadRunStatus(context.Background(), client, "org-1", []string{"repo-1:run-1"}, bound)
		if err != nil {
			t.Fatalf("ReadRunStatus() error = %v", err)
		}
		if len(rows) != 1 || rows[0].Status != "running" {
			t.Fatalf("rows = %#v", rows)
		}
		statement := client.queries[len(client.queries)-1].statement
		if !containsAll(statement, "if(c.finished_at IS NOT NULL", "'running'", "c.started_at <=") {
			t.Fatalf("statement = %q, want the active-time-bound status expression and existence predicate", statement)
		}
		foundEnd := false
		for _, binding := range client.queries[len(client.queries)-1].bindings {
			if binding.Name == readers.BoundEndParam {
				foundEnd = true
			}
		}
		if !foundEnd {
			t.Fatalf("bindings = %#v, want a %s binding", client.queries[len(client.queries)-1].bindings, readers.BoundEndParam)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", err: errors.New("boom")}}}
		rows, err := readers.ReadRunStatus(context.Background(), client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}

func cicdMetricsRow(repoID string) []any {
	return []any{repoID, "2026-02-21", int64(30), float64(0.9), uint8(1), float64(12.0), uint8(1), float64(25.0), uint8(1), float64(3.0)}
}

func TestReadCICDMetricsDaily(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM cicd_metrics_daily", rows: [][]any{cicdMetricsRow("repo-1")}}}}
		rows, err := readers.ReadCICDMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadCICDMetricsDaily() error = %v", err)
		}
		want := readers.CICDMetricsDailyRow{
			RepoID: "repo-1", Day: "2026-02-21", PipelinesCount: 30, SuccessRate: 0.9,
			HasAvgDuration: true, AvgDuration: 12.0,
			HasP90Duration: true, P90Duration: 25.0,
			HasAvgQueue: true, AvgQueue: 3.0,
		}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
	})

	t.Run("null durations report has-flags false", func(t *testing.T) {
		t.Parallel()
		row := cicdMetricsRow("repo-1")
		row[4], row[5] = uint8(0), float64(0)
		row[6], row[7] = uint8(0), float64(0)
		row[8], row[9] = uint8(0), float64(0)
		client := &fakeClient{tables: []fakeTable{{match: "FROM cicd_metrics_daily", rows: [][]any{row}}}}
		rows, err := readers.ReadCICDMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadCICDMetricsDaily() error = %v", err)
		}
		if len(rows) != 1 || rows[0].HasAvgDuration || rows[0].HasP90Duration || rows[0].HasAvgQueue {
			t.Fatalf("rows = %#v, want every has-flag false", rows)
		}
	})

	t.Run("row limit applied", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM cicd_metrics_daily", rows: [][]any{cicdMetricsRow("repo-1")}}}}
		if _, err := readers.ReadCICDMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadCICDMetricsDaily() error = %v", err)
		}
		statement := client.queries[len(client.queries)-1].statement
		if !containsAll(statement, "LIMIT 200") {
			t.Fatalf("statement = %q, want a LIMIT 200 clause", statement)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM cicd_metrics_daily", err: errors.New("boom")}}}
		rows, err := readers.ReadCICDMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
