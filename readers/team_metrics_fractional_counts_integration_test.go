//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// A team that co-owns a repository takes that repository's counters at its
// own share, so a team row can hold a fraction of a count (ops migration 124
// widens the counters from UInt32 to Float64). The readers must serve the
// whole value on both column types, because the reader and the table roll
// independently: nothing is rounded and nothing is truncated.
func TestIntegrationTeamMetricsReadersServeFractionalCountsOnBothColumnTypes(t *testing.T) {
	const day = "2026-03-01"
	cases := []struct {
		name            string
		counterType     string
		rows            []string
		commits, ah, wk float64
	}{
		{
			name: "whole-counts-on-uint32", counterType: "UInt32",
			rows:    []string{"7, 3, 1", "3, 1, 1"},
			commits: 10, ah: 4, wk: 2,
		},
		{
			name: "fractional-shares-on-float64", counterType: "Float64",
			rows:    []string{"0.3, 0.1, 0.2", "0.3, 0.2, 0.1"},
			commits: 0.6, ah: 0.3, wk: 0.3,
		},
		{
			name: "whole-counts-on-float64", counterType: "Float64",
			rows:    []string{"7, 3, 1", "3, 1, 1"},
			commits: 10, ah: 4, wk: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, seed := tmClientWithCounters(t, tc.counterType)
			ctx := context.Background()
			team := "t-fraction"
			project := tmSeedProject(t, seed, team)
			for i, values := range tc.rows {
				stmt := fmt.Sprintf("INSERT INTO team_metrics_daily (day, team_id, team_name, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio, computed_at, org_id, repo_id) VALUES ('%s', '%s', 'Team', %s, 0.77, 0.66, '2026-03-02 10:00:00', '%s', '%s')",
					day, team, values, tmOrg, tmRepo(i+1))
				if err := seed.Exec(ctx, stmt); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			near := func(label string, got, want float64) {
				t.Helper()
				if math.Abs(got-want) > 1e-9 {
					t.Errorf("%s = %v, want %v", label, got, want)
				}
			}
			teamRows, err := readers.ReadTeamMetrics(ctx, client, tmOrg, []string{team}, readers.TimeBound{})
			if err != nil {
				t.Fatalf("ReadTeamMetrics: %v", err)
			}
			if len(teamRows) != 1 {
				t.Fatalf("ReadTeamMetrics: %d rows, want 1", len(teamRows))
			}
			near("commits", teamRows[0].CommitsCount, tc.commits)
			near("after-hours commits", teamRows[0].AfterHoursCommitsCount, tc.ah)
			near("weekend commits", teamRows[0].WeekendCommitsCount, tc.wk)
			near("after-hours ratio", teamRows[0].AfterHoursCommitRatio, tc.ah/tc.commits)
			near("weekend ratio", teamRows[0].WeekendCommitRatio, tc.wk/tc.commits)

			breakdown, err := readers.ReadProjectMetricsBreakdown(ctx, client, tmOrg, []string{project}, readers.TimeBound{})
			if err != nil {
				t.Fatalf("ReadProjectMetricsBreakdown: %v", err)
			}
			rollups := readers.RollupProjectMetrics(breakdown)
			if len(rollups) != 1 {
				t.Fatalf("RollupProjectMetrics: %d rollups, want 1", len(rollups))
			}
			near("rollup commits", rollups[0].CommitsCount, tc.commits)
			near("rollup after-hours commits", rollups[0].AfterHoursCommitsCount, tc.ah)
			near("rollup weekend commits", rollups[0].WeekendCommitsCount, tc.wk)
		})
	}
}
