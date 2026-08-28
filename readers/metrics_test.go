package readers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func repoMetricsRow(repoID string) []any {
	return []any{repoID, "2026-02-21", int64(42), int64(7), float64(12.5), float64(0.1), uint8(1), float64(3.5), int64(4), float64(0.2)}
}

func TestReadRepositoryMetrics(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM repo_metrics_daily", rows: [][]any{repoMetricsRow("repo-1")}}}}
		rows, err := readers.ReadRepositoryMetrics(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadRepositoryMetrics() error = %v", err)
		}
		want := readers.RepositoryMetricsRow{
			RepoID: "repo-1", Day: "2026-02-21", CommitsCount: 42, PRsMerged: 7,
			MedianPRCycleHours: 12.5, ChangeFailureRate: 0.1, HasMTTRHours: true, MTTRHours: 3.5,
			BusFactor: 4, CodeOwnershipGini: 0.2,
		}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
	})

	t.Run("no mttr reports has-flag false", func(t *testing.T) {
		t.Parallel()
		row := repoMetricsRow("repo-1")
		row[6], row[7] = uint8(0), float64(0)
		client := &fakeClient{tables: []fakeTable{{match: "FROM repo_metrics_daily", rows: [][]any{row}}}}
		rows, err := readers.ReadRepositoryMetrics(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadRepositoryMetrics() error = %v", err)
		}
		if len(rows) != 1 || rows[0].HasMTTRHours {
			t.Fatalf("rows = %#v, want HasMTTRHours false", rows)
		}
	})

	t.Run("empty ids short-circuits without querying", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		rows, err := readers.ReadRepositoryMetrics(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, nil)", rows, err)
		}
		if len(client.queries) != 0 {
			t.Fatalf("queries = %#v, want none issued for an empty id set", client.queries)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM repo_metrics_daily", err: errors.New("boom")}}}
		rows, err := readers.ReadRepositoryMetrics(context.Background(), client, "org-1", []string{"repo-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}

func teamMetricsRow(teamID string) []any {
	return []any{teamID, "2026-02-21", int64(10), int64(2), int64(1), float64(0.2), float64(0.1)}
}

func TestReadTeamMetrics(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM team_metrics_daily", rows: [][]any{teamMetricsRow("team-1")}}}}
	rows, err := readers.ReadTeamMetrics(context.Background(), client, "org-1", []string{"team-1"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadTeamMetrics() error = %v", err)
	}
	want := readers.TeamMetricsRow{
		TeamID: "team-1", Day: "2026-02-21", CommitsCount: 10,
		AfterHoursCommitsCount: 2, WeekendCommitsCount: 1,
		AfterHoursCommitRatio: 0.2, WeekendCommitRatio: 0.1,
	}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows = %#v, want %#v", rows, want)
	}
}

func projectRollupRow(provider, projectID, teamID, teamName string, commits, afterHoursCommits, weekendCommits int64, afterHoursRatio, weekendRatio float64) []any {
	return []any{provider + ":" + projectID, teamID, teamName, "2026-02-21", commits, afterHoursCommits, weekendCommits, afterHoursRatio, weekendRatio}
}

func TestReadProjectMetricsBreakdownAndRollupProjectMetrics(t *testing.T) {
	t.Parallel()

	t.Run("sums counts, keeps per-team rates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", rows: [][]any{
			projectRollupRow("linear", "proj-1", "team-1", "Team One", 10, 4, 2, 0.4, 0.2),
			projectRollupRow("linear", "proj-1", "team-2", "Team Two", 5, 0, 0, 0.0, 0.0),
		}}}}
		breakdown, err := readers.ReadProjectMetricsBreakdown(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectMetricsBreakdown() error = %v", err)
		}
		if len(breakdown) != 2 {
			t.Fatalf("breakdown = %#v, want 2 rows", breakdown)
		}
		rollups := readers.RollupProjectMetrics(breakdown)
		if len(rollups) != 1 {
			t.Fatalf("rollups = %#v, want 1", rollups)
		}
		rollup := rollups[0]
		if rollup.CommitsCount != 15 {
			t.Fatalf("CommitsCount = %v, want summed 15", rollup.CommitsCount)
		}
		if rollup.AfterHoursCommitsCount != 4 {
			t.Fatalf("AfterHoursCommitsCount = %v, want summed 4", rollup.AfterHoursCommitsCount)
		}
		if rollup.TeamCount != 2 {
			t.Fatalf("TeamCount = %v, want 2", rollup.TeamCount)
		}
		if len(rollup.TeamBreakdown) != 2 {
			t.Fatalf("TeamBreakdown = %#v, want 2 rows", rollup.TeamBreakdown)
		}
		// Nothing here may equal an averaged rate ((0.4+0.0)/2 = 0.2): each
		// row must carry its OWN team's ratio, unmodified.
		if rollup.TeamBreakdown[0].AfterHoursCommitRatio != 0.4 {
			t.Fatalf("TeamBreakdown[0].AfterHoursCommitRatio = %v, want team-1's own 0.4, not an average", rollup.TeamBreakdown[0].AfterHoursCommitRatio)
		}
		if rollup.TeamBreakdown[1].AfterHoursCommitRatio != 0.0 {
			t.Fatalf("TeamBreakdown[1].AfterHoursCommitRatio = %v, want team-2's own 0.0, not an average", rollup.TeamBreakdown[1].AfterHoursCommitRatio)
		}
	})

	t.Run("dedupes a team owned through two ownership sources", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", rows: [][]any{
			projectRollupRow("linear", "proj-1", "team-1", "Team One", 10, 4, 2, 0.4, 0.2),
			projectRollupRow("linear", "proj-1", "team-1", "Team One", 10, 4, 2, 0.4, 0.2),
		}}}}
		breakdown, err := readers.ReadProjectMetricsBreakdown(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectMetricsBreakdown() error = %v", err)
		}
		rollups := readers.RollupProjectMetrics(breakdown)
		if len(rollups) != 1 || rollups[0].CommitsCount != 10 || rollups[0].TeamCount != 1 {
			t.Fatalf("rollups = %#v, want 1 rollup deduped to CommitsCount=10, TeamCount=1", rollups)
		}
	})

	t.Run("no owning teams yields no rollup", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", rows: nil}}}
		breakdown, err := readers.ReadProjectMetricsBreakdown(context.Background(), client, "org-1", []string{"linear:proj-404"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectMetricsBreakdown() error = %v", err)
		}
		if rollups := readers.RollupProjectMetrics(breakdown); len(rollups) != 0 {
			t.Fatalf("rollups = %#v, want none", rollups)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", err: errors.New("boom")}}}
		breakdown, err := readers.ReadProjectMetricsBreakdown(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err == nil || breakdown != nil {
			t.Fatalf("breakdown = %#v, err = %v, want (nil, err)", breakdown, err)
		}
	})
}
