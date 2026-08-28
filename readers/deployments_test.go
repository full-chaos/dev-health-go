package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadDeploymentStatus(t *testing.T) {
	t.Parallel()

	t.Run("happy path, current axis", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM deployments", rows: [][]any{{"deploy-1", "success", "production", "repo-1"}}}}}
		rows, err := readers.ReadDeploymentStatus(context.Background(), client, "org-1", []string{"repo-1:deploy-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadDeploymentStatus() error = %v", err)
		}
		want := readers.DeploymentStatusRow{DeploymentID: "deploy-1", Status: "success", Environment: "production", RepoID: "repo-1"}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
	})

	t.Run("active time bound scopes and binds the requested instant", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM deployments", rows: [][]any{{"deploy-1", "in_progress", "", "repo-1"}}}}}
		bound := readers.TimeBound{Active: true, End: time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)}
		rows, err := readers.ReadDeploymentStatus(context.Background(), client, "org-1", []string{"repo-1:deploy-1"}, bound)
		if err != nil {
			t.Fatalf("ReadDeploymentStatus() error = %v", err)
		}
		if len(rows) != 1 || rows[0].Status != "in_progress" {
			t.Fatalf("rows = %#v", rows)
		}
		statement := client.queries[len(client.queries)-1].statement
		if !strings.Contains(statement, "'in_progress'") || !strings.Contains(statement, "coalesce(d.started_at, d.deployed_at) <=") {
			t.Fatalf("statement = %q, want the active-time-bound status expression and existence predicate", statement)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM deployments", err: errors.New("boom")}}}
		rows, err := readers.ReadDeploymentStatus(context.Background(), client, "org-1", []string{"repo-1:deploy-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}

func deployMetricsRow(repoID string) []any {
	return []any{repoID, "2026-02-21", int64(12), int64(2), uint8(1), float64(1.5), uint8(1), float64(3.0)}
}

func TestReadDeployMetricsDaily(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM deploy_metrics_daily", rows: [][]any{deployMetricsRow("repo-1")}}}}
		rows, err := readers.ReadDeployMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadDeployMetricsDaily() error = %v", err)
		}
		want := readers.DeployMetricsDailyRow{
			RepoID: "repo-1", Day: "2026-02-21", DeploymentsCount: 12, FailedDeploymentsCount: 2,
			HasDeployTime: true, DeployTime: 1.5, HasLeadTime: true, LeadTime: 3.0,
		}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
	})

	t.Run("null durations report has-flags false", func(t *testing.T) {
		t.Parallel()
		row := deployMetricsRow("repo-1")
		row[4], row[5] = uint8(0), float64(0)
		row[6], row[7] = uint8(0), float64(0)
		client := &fakeClient{tables: []fakeTable{{match: "FROM deploy_metrics_daily", rows: [][]any{row}}}}
		rows, err := readers.ReadDeployMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadDeployMetricsDaily() error = %v", err)
		}
		if len(rows) != 1 || rows[0].HasDeployTime || rows[0].HasLeadTime {
			t.Fatalf("rows = %#v, want both has-flags false", rows)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM deploy_metrics_daily", err: errors.New("boom")}}}
		rows, err := readers.ReadDeployMetricsDaily(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}
