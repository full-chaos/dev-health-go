package readers

import "context"

// CIPipelineRunStatusRow is one ci_pipeline_runs row scanned for a single
// requested (repo_id, run_id) composite key.
type CIPipelineRunStatusRow struct {
	RunID  string
	Status string
	RepoID string
}

// ReadRunStatus reads ci_pipeline_runs.status for the given "repoID:runID"
// composite ids.
//
// Extracted verbatim from acr devhealthfacts's ci.go readRunStatus
// (CHAOS-4377); CHAOS-3780's original ci_pipeline_runs read.
//
// CHAOS-3781 Tier B: a run's FINAL status only becomes true when the run
// finishes. Reporting it for an instant while the run was still executing
// would report an outcome that had not happened yet, so a run unfinished at
// the requested time reports 'running' instead. A run that had not started
// is excluded outright (AC-3781-3).
func ReadRunStatus(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]CIPipelineRunStatusRow, error) {
	statusExpression := "ifNull(c.status, '')"
	if timeBound.Active {
		statusExpression = "if(c.finished_at IS NOT NULL AND c.finished_at <= " + timeBound.AsOfExpression() +
			", ifNull(c.status, ''), 'running')"
	}
	statement := WithRowLimit(`SELECT c.run_id, `+statusExpression+`, toString(c.repo_id)
FROM ci_pipeline_runs AS c FINAL
WHERE c.org_id = {org_id:String} AND concat(toString(c.repo_id), ':', c.run_id) IN {ids:Array(String)}`+timeBound.ExistencePredicate("c.started_at"), DefaultRowLimit)

	var rows []CIPipelineRunStatusRow
	err := QueryOrgScopedNamed(ctx, client, "ReadRunStatus", statement, orgID, ids, func(row RowScanner) error {
		var r CIPipelineRunStatusRow
		if err := row.Scan(&r.RunID, &r.Status, &r.RepoID); err != nil {
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

// CICDMetricsDailyRow is one cicd_metrics_daily row scanned for a single
// requested repository, already resolved to its latest (deduped) day.
//
// HasAvgDuration/HasP90Duration/HasAvgQueue convert the SQL's toUInt8
// has-flags to bool for a more natural public API; the corresponding
// duration field is meaningless (reads as its ClickHouse-side zero-fill)
// whenever its flag is false.
type CICDMetricsDailyRow struct {
	RepoID         string
	Day            string
	PipelinesCount int64
	SuccessRate    float64
	HasAvgDuration bool
	AvgDuration    float64
	HasP90Duration bool
	P90Duration    float64
	HasAvgQueue    bool
	AvgQueue       float64
}

// ReadCICDMetricsDaily reads cicd_metrics_daily (latest day per repository)
// -- CHAOS-4347's repository-scoped CI aggregate.
//
// Extracted verbatim from acr devhealthfacts's ci.go readRepositoryAggregate
// (CHAOS-4377).
//
// cicd_metrics_daily is a plain, append-only MergeTree table: the same
// intraday-rerun shape documented for repo_metrics_daily/team_metrics_daily
// (see ReadRepoMetricsDaily's doc comment) -- row_number() OVER (PARTITION BY
// repo_id ORDER BY day DESC, computed_at DESC, cityHash64(...) DESC), picking
// rn=1, is required for the identical reason -- not verified separately
// against this specific table's live data, but the shape (plain MergeTree,
// no per-row unique id, populated by a daily batch job per
// ops/src/dev_health_ops/metrics/compute_cicd.py) is the same one that
// produced confirmed reruns and ties on every other table in this family, so
// the same defensive tiebreak is applied rather than assumed safe by a
// difference that isn't actually there.
func ReadCICDMetricsDaily(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]CICDMetricsDailyRow, error) {
	statement := WithRowLimit(`SELECT toString(repo_id), toString(day), toInt64(pipelines_count), toFloat64(success_rate), toUInt8(isNotNull(avg_duration_minutes)), toFloat64(ifNull(avg_duration_minutes, 0)), toUInt8(isNotNull(p90_duration_minutes)), toFloat64(ifNull(p90_duration_minutes, 0)), toUInt8(isNotNull(avg_queue_minutes)), toFloat64(ifNull(avg_queue_minutes, 0))
FROM (
	SELECT repo_id, day, pipelines_count, success_rate, avg_duration_minutes, p90_duration_minutes, avg_queue_minutes,
		row_number() OVER (PARTITION BY repo_id ORDER BY day DESC, computed_at DESC, cityHash64(tuple(pipelines_count, success_rate, ifNull(avg_duration_minutes, -1), ifNull(p90_duration_minutes, -1), ifNull(avg_queue_minutes, -1))) DESC) AS rn
	FROM cicd_metrics_daily
	WHERE org_id = {org_id:String} AND toString(repo_id) IN {ids:Array(String)}`+timeBound.DayPredicate("day")+`
)
WHERE rn = 1`, DefaultRowLimit)

	var rows []CICDMetricsDailyRow
	err := QueryOrgScopedNamed(ctx, client, "ReadCICDMetricsDaily", statement, orgID, ids, func(row RowScanner) error {
		var repoID, day string
		var pipelinesCount int64
		var successRate float64
		var hasAvgDuration, hasP90Duration, hasAvgQueue uint8
		var avgDuration, p90Duration, avgQueue float64
		if err := row.Scan(&repoID, &day, &pipelinesCount, &successRate, &hasAvgDuration, &avgDuration, &hasP90Duration, &p90Duration, &hasAvgQueue, &avgQueue); err != nil {
			return err
		}
		rows = append(rows, CICDMetricsDailyRow{
			RepoID: repoID, Day: day, PipelinesCount: pipelinesCount, SuccessRate: successRate,
			HasAvgDuration: hasAvgDuration != 0, AvgDuration: avgDuration,
			HasP90Duration: hasP90Duration != 0, P90Duration: p90Duration,
			HasAvgQueue: hasAvgQueue != 0, AvgQueue: avgQueue,
		})
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
