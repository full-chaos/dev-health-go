package readers

import "context"

// ReadinessCoverageRow is one estimate_coverage_metrics_daily row for a
// team, scanned verbatim off ReadTeamReadiness's query -- one row per
// (team_id, work_scope_id, provider) triple: a team can have several
// concurrent work scopes (e.g. sprints) tracked at once, and different
// source providers can share a work_scope_id string.
//
// HasRatio is the raw toUInt8(isNotNull(ratio)) column; a caller checks it
// before trusting Ratio, which is ifNull(ratio, 0) when HasRatio is 0.
type ReadinessCoverageRow struct {
	TeamID           string
	WorkScopeID      string
	Provider         string
	Day              string
	EstimatedCount   int64
	UnestimatedCount int64
	BacklogSize      int64
	HasRatio         uint8
	Ratio            float64
}

// ReadTeamReadiness reads estimate_coverage_metrics_daily for the given
// team ids.
//
// estimate_coverage_metrics_daily's own sort key is (org_id, day, provider,
// work_scope_id, team_id) -- two different source providers (live data:
// gitlab, linear) can report against the same work_scope_id string, so
// provider is part of this reader's partition key too, not folded away.
// The table is ReplacingMergeTree(computed_at): FINAL collapses an
// exact-key rerun, and row_number() ORDER BY day DESC, computed_at DESC
// (not day alone) still resolves the case where FINAL has not yet merged a
// same-day recompute.
//
// day/computed_at is still not a TOTAL order: estimate_coverage_metrics_daily
// has no per-row unique id beyond this partition's own key, so two rows
// could share both. cityHash64 of the value columns is the final
// tiebreaker -- arbitrary among an exact tie, but stable. Its
// ifNull(ratio, -1) sentinel is only unambiguous while -1 is outside
// ratio's real domain: ratio is estimated_count/backlog_size, a fraction;
// live data ranges [0, 1], never negative. There is no ClickHouse-level
// CHECK constraint enforcing this -- it is a domain assumption, not a type
// guarantee.
func ReadTeamReadiness(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]ReadinessCoverageRow, error) {
	statement := WithRowLimit(`SELECT team_id, work_scope_id, provider, toString(day), toInt64(estimated_count), toInt64(unestimated_count), toInt64(backlog_size), toUInt8(isNotNull(ratio)), toFloat64(ifNull(ratio, 0))
FROM (
	SELECT ifNull(team_id, '') AS team_id, work_scope_id, provider, day, estimated_count, unestimated_count, backlog_size, ratio,
		row_number() OVER (PARTITION BY team_id, work_scope_id, provider ORDER BY day DESC, computed_at DESC, cityHash64(tuple(estimated_count, unestimated_count, backlog_size, ifNull(ratio, -1))) DESC) AS rn
	FROM estimate_coverage_metrics_daily FINAL
	WHERE org_id = {org_id:String} AND team_id IN {ids:Array(String)}`+timeBound.DayPredicate("day")+`
)
WHERE rn = 1`, DefaultRowLimit)
	var rows []ReadinessCoverageRow
	err := QueryOrgScopedNamed(ctx, client, "ReadTeamReadiness", statement, orgID, ids, func(row RowScanner) error {
		var r ReadinessCoverageRow
		if err := row.Scan(&r.TeamID, &r.WorkScopeID, &r.Provider, &r.Day, &r.EstimatedCount, &r.UnestimatedCount, &r.BacklogSize, &r.HasRatio, &r.Ratio); err != nil {
			return err
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ReadinessProjectRow is one (project, team, work_scope, provider) row
// contributing to a project's readiness rollup, scanned verbatim off
// ReadProjectReadiness's team_project_ownership join. Multiple rows can
// share ProjectSubjectKey: estimate_coverage_metrics_daily partitions by
// (team, work_scope_id, provider), so summing estimated_count/backlog_size
// across teams that track DIFFERENT work scopes would mix unrelated
// backlogs into one meaningless total -- every owning team's own latest
// per-scope coverage row survives verbatim. Grouping/breakdown-table
// construction is the caller's job.
type ReadinessProjectRow struct {
	ProjectSubjectKey string
	TeamID            string
	TeamName          string
	WorkScopeID       string
	Provider          string
	Day               string
	EstimatedCount    int64
	UnestimatedCount  int64
	BacklogSize       int64
	HasRatio          uint8
	Ratio             float64
}

// ReadProjectReadiness rolls estimate_coverage_metrics_daily up for a
// project through projects -> team_project_ownership ->
// estimate_coverage_metrics_daily: every team owning the project
// contributes its own latest per-(work_scope, provider) coverage row,
// verbatim -- see ReadTeamReadiness's doc comment for why estimate/backlog
// counts are never summed across teams here.
func ReadProjectReadiness(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]ReadinessProjectRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// CHAOS-4521b: the project's OWN estimate_coverage_metrics_daily rows,
	// matched on work_scope_id, with no team-ownership hop. The team is
	// still reported -- from the ROW's own team_id, which is the team that
	// produced that coverage row, not "some team that owns this project".
	//
	// The rn partition drops team_id: the question is now "the latest row
	// per (work scope, provider)", and partitioning by team as well would
	// return one latest row PER TEAM for the same work scope whenever two
	// teams both wrote coverage for it.
	statement := WithRowLimit(`SELECT concat(p.provider, ':', p.id), ec.team_id, ifNull(t.name, ''), ec.work_scope_id, ec.provider, toString(ec.day), toInt64(ec.estimated_count), toInt64(ec.unestimated_count), toInt64(ec.backlog_size), toUInt8(isNotNull(ec.ratio)), toFloat64(ifNull(ec.ratio, 0))
FROM `+ProjectWorkScopeJoinSQL()+`
INNER JOIN (
	SELECT ifNull(team_id, '') AS team_id, work_scope_id, provider, day, estimated_count, unestimated_count, backlog_size, ratio,
		row_number() OVER (PARTITION BY work_scope_id, provider ORDER BY day DESC, computed_at DESC, cityHash64(tuple(estimated_count, unestimated_count, backlog_size, ifNull(ratio, -1))) DESC) AS rn
	FROM estimate_coverage_metrics_daily FINAL
	WHERE org_id = {org_id:String}`+timeBound.DayPredicate("day")+`
) AS ec ON `+ProjectWorkScopeMatchSQL("ec", "work_scope_id")+` AND ec.rn = 1
LEFT JOIN (SELECT id, name FROM teams FINAL WHERE org_id = {org_id:String}) AS t ON t.id = ec.team_id
ORDER BY p.id, ec.work_scope_id, ec.provider`, DefaultRowLimit)
	var rows []ReadinessProjectRow
	err := QueryOrgScopedNamed(ctx, client, "ReadProjectReadiness", statement, orgID, ids, func(row RowScanner) error {
		var r ReadinessProjectRow
		if err := row.Scan(&r.ProjectSubjectKey, &r.TeamID, &r.TeamName, &r.WorkScopeID, &r.Provider, &r.Day, &r.EstimatedCount, &r.UnestimatedCount, &r.BacklogSize, &r.HasRatio, &r.Ratio); err != nil {
			return err
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
