package readers

import "context"

// Neutral row readers over repo_metrics_daily/team_metrics_daily, extracted
// from acr's internal/contextfabric/devhealthfacts/metrics.go (CHAOS-4377).
// acr's MetricsProvider.readRepositoryMetrics/readTeamMetrics/
// readProjectMetrics interleaved this SQL-scan with building a
// contextfabric.CanonicalFact per row; this file carries only the scan and
// the (neutral) Go-side rollup aggregation, returning plain structs. acr's
// adapter wraps these into CanonicalFact/FactValue on top.
//
// Both tables are plain, append-only MergeTree tables: live data shows up
// to ~86 rows sharing one (repo_id|team_id, day) key (intraday reruns), and
// those reruns carry genuinely different values, not no-op repeats. Every
// statement below picks exactly one row per subject via
// row_number() OVER (PARTITION BY <id> ORDER BY day DESC, computed_at DESC,
// cityHash64(<value columns>) DESC) -- GROUP BY + independent per-field
// argMax(field, day) has no guarantee of breaking a day tie the same way
// across fields, which can stitch a fact together from different rows that
// were never actually true at the same instant. day/computed_at is still
// not a TOTAL order (two rows can share both), so cityHash64 of the row's
// own value columns is the final tiebreaker: arbitrary among an exact tie,
// but stable, so the same row wins every execution.

// RepositoryMetricsRow is one repository's latest repo_metrics_daily row.
type RepositoryMetricsRow struct {
	RepoID             string
	Day                string
	CommitsCount       int64
	PRsMerged          int64
	MedianPRCycleHours float64
	ChangeFailureRate  float64
	HasMTTRHours       bool
	MTTRHours          float64
	BusFactor          int64
	CodeOwnershipGini  float64
}

// ReadRepositoryMetrics reads the latest repo_metrics_daily row (subject to
// timeBound) for each of repoIDs, scoped to orgID.
func ReadRepositoryMetrics(ctx context.Context, client QueryClient, orgID string, repoIDs []string, timeBound TimeBound) ([]RepositoryMetricsRow, error) {
	if len(repoIDs) == 0 {
		return nil, nil
	}
	// The hash tiebreak's ifNull(mttr_hours, -1) sentinel is only
	// unambiguous while -1 is outside mttr_hours' real domain: a
	// mean-time-to-recovery duration is semantically always >= 0 (a domain
	// assumption, not a type guarantee -- no ClickHouse CHECK enforces it).
	statement := WithRowLimit(`SELECT toString(repo_id), toString(day), toInt64(commits_count), toInt64(prs_merged), toFloat64(median_pr_cycle_hours), toFloat64(change_failure_rate), toUInt8(isNotNull(mttr_hours)), toFloat64(ifNull(mttr_hours, 0)), toInt64(bus_factor), toFloat64(code_ownership_gini)
FROM (
	SELECT repo_id, day, commits_count, prs_merged, median_pr_cycle_hours, change_failure_rate, mttr_hours, bus_factor, code_ownership_gini,
		row_number() OVER (PARTITION BY repo_id ORDER BY day DESC, computed_at DESC, cityHash64(tuple(commits_count, prs_merged, median_pr_cycle_hours, change_failure_rate, ifNull(mttr_hours, -1), bus_factor, code_ownership_gini)) DESC) AS rn
	FROM repo_metrics_daily
	WHERE org_id = {org_id:String} AND toString(repo_id) IN {ids:Array(String)}`+timeBound.DayPredicate("day")+`
)
WHERE rn = 1`, DefaultRowLimit)
	var rows []RepositoryMetricsRow
	err := QueryOrgScoped(ctx, client, statement, orgID, repoIDs, func(row RowScanner) error {
		var r RepositoryMetricsRow
		var hasMTTR uint8
		if err := row.Scan(&r.RepoID, &r.Day, &r.CommitsCount, &r.PRsMerged, &r.MedianPRCycleHours, &r.ChangeFailureRate, &hasMTTR, &r.MTTRHours, &r.BusFactor, &r.CodeOwnershipGini); err != nil {
			return err
		}
		r.HasMTTRHours = hasMTTR != 0
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	return rows, err
}

// TeamMetricsRow is one team's latest team_metrics_daily row -- a genuinely
// team-scoped rollup, not a proxy through any repository the team touches.
type TeamMetricsRow struct {
	TeamID                 string
	Day                    string
	CommitsCount           int64
	AfterHoursCommitsCount int64
	WeekendCommitsCount    int64
	AfterHoursCommitRatio  float64
	WeekendCommitRatio     float64
}

// ReadTeamMetrics reads the latest team_metrics_daily row (subject to
// timeBound) for each of teamIDs, scoped to orgID.
func ReadTeamMetrics(ctx context.Context, client QueryClient, orgID string, teamIDs []string, timeBound TimeBound) ([]TeamMetricsRow, error) {
	if len(teamIDs) == 0 {
		return nil, nil
	}
	statement := WithRowLimit(`SELECT toString(team_id), toString(day), toInt64(commits_count), toInt64(after_hours_commits_count), toInt64(weekend_commits_count), toFloat64(after_hours_commit_ratio), toFloat64(weekend_commit_ratio)
FROM (
	SELECT team_id, day, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio,
		row_number() OVER (PARTITION BY team_id ORDER BY day DESC, computed_at DESC, cityHash64(tuple(team_name, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio)) DESC) AS rn
	FROM team_metrics_daily
	WHERE org_id = {org_id:String} AND toString(team_id) IN {ids:Array(String)}`+timeBound.DayPredicate("day")+`
)
WHERE rn = 1`, DefaultRowLimit)
	var rows []TeamMetricsRow
	err := QueryOrgScoped(ctx, client, statement, orgID, teamIDs, func(row RowScanner) error {
		var r TeamMetricsRow
		if err := row.Scan(&r.TeamID, &r.Day, &r.CommitsCount, &r.AfterHoursCommitsCount, &r.WeekendCommitsCount, &r.AfterHoursCommitRatio, &r.WeekendCommitRatio); err != nil {
			return err
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	return rows, err
}

// ProjectTeamMetricsRow is one (project, team) pair's contribution to a
// project's metrics rollup, scanned off the team_project_ownership join
// before aggregation.
type ProjectTeamMetricsRow struct {
	ProjectKey string // "<provider>:<id>", matching the project subject's identity encoding.
	TeamID     string
	TeamName   string
	TeamMetricsRow
}

// ReadProjectMetricsBreakdown rolls repository/team metrics up for a
// project through projects -> team_project_ownership -> team_metrics_daily:
// every team owning the project (as of the requested instant, or currently
// on the current axis) contributes its own latest team_metrics_daily row.
// Rows are returned in a deterministic (project, team) order -- the
// query's own ORDER BY makes the scan order itself deterministic, which
// RollupProjectMetrics' caller-visible ordering depends on.
func ReadProjectMetricsBreakdown(ctx context.Context, client QueryClient, orgID string, projectKeys []string, timeBound TimeBound) ([]ProjectTeamMetricsRow, error) {
	if len(projectKeys) == 0 {
		return nil, nil
	}
	ownershipPredicate := OwnershipValidityPredicate(timeBound)
	statement := WithRowLimit(`SELECT concat(p.provider, ':', p.id), tm.team_id, tm.team_name, toString(tm.day), toInt64(tm.commits_count), toInt64(tm.after_hours_commits_count), toInt64(tm.weekend_commits_count), toFloat64(tm.after_hours_commit_ratio), toFloat64(tm.weekend_commit_ratio)
FROM `+ProjectOwnershipJoinSQL(ownershipPredicate)+`
INNER JOIN (
	SELECT team_id, team_name, day, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio,
		row_number() OVER (PARTITION BY team_id ORDER BY day DESC, computed_at DESC, cityHash64(tuple(team_name, commits_count, after_hours_commits_count, weekend_commits_count, after_hours_commit_ratio, weekend_commit_ratio)) DESC) AS rn
	FROM team_metrics_daily
	WHERE org_id = {org_id:String}`+timeBound.DayPredicate("day")+`
) AS tm ON tm.team_id = tpo.team_id AND tm.rn = 1
ORDER BY p.id, tm.team_id`, DefaultRowLimit)
	var rows []ProjectTeamMetricsRow
	err := QueryOrgScoped(ctx, client, statement, orgID, projectKeys, func(row RowScanner) error {
		var r ProjectTeamMetricsRow
		if err := row.Scan(&r.ProjectKey, &r.TeamID, &r.TeamName, &r.Day, &r.CommitsCount, &r.AfterHoursCommitsCount, &r.WeekendCommitsCount, &r.AfterHoursCommitRatio, &r.WeekendCommitRatio); err != nil {
			return err
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	return rows, err
}

// ProjectMetricsRollup is one project's aggregated metrics: additive counts
// SUMMED across owning teams (sound, because a count is additive regardless
// of population size), while ratios are NEVER averaged across teams of
// different sizes (that silently misrepresents the population) -- each
// team's own ratio survives, unmodified, in TeamBreakdown.
type ProjectMetricsRollup struct {
	ProjectKey             string
	TeamCount              int
	CommitsCount           int64
	AfterHoursCommitsCount int64
	WeekendCommitsCount    int64
	TeamBreakdown          []ProjectTeamMetricsRow
}

// RollupProjectMetrics aggregates ReadProjectMetricsBreakdown's rows into
// one ProjectMetricsRollup per project, in first-seen order. A team owning
// a project through more than one `source` row in team_project_ownership
// yields more than one input row for that team; RollupProjectMetrics
// deduplicates by team id before summing, via MarkSeen, so such a team is
// never double-counted.
func RollupProjectMetrics(rows []ProjectTeamMetricsRow) []ProjectMetricsRollup {
	byProject := make(map[string]*ProjectMetricsRollup)
	var order []string
	for _, r := range rows {
		rollup, ok := byProject[r.ProjectKey]
		if !ok {
			rollup = &ProjectMetricsRollup{ProjectKey: r.ProjectKey}
			byProject[r.ProjectKey] = rollup
			order = append(order, r.ProjectKey)
		}
	}
	seenTeams := make(map[string]map[string]bool, len(order))
	for _, key := range order {
		seenTeams[key] = make(map[string]bool)
	}
	for _, r := range rows {
		rollup := byProject[r.ProjectKey]
		if MarkSeen(seenTeams[r.ProjectKey], r.TeamID) {
			continue
		}
		rollup.CommitsCount += r.CommitsCount
		rollup.AfterHoursCommitsCount += r.AfterHoursCommitsCount
		rollup.WeekendCommitsCount += r.WeekendCommitsCount
		rollup.TeamBreakdown = append(rollup.TeamBreakdown, r)
	}
	rollups := make([]ProjectMetricsRollup, 0, len(order))
	for _, key := range order {
		rollup := byProject[key]
		rollup.TeamCount = len(rollup.TeamBreakdown)
		if rollup.TeamCount == 0 {
			continue
		}
		rollups = append(rollups, *rollup)
	}
	return rollups
}
