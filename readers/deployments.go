package readers

import "context"

// DeploymentStatusRow is one deployments row scanned for a single requested
// (repo_id, deployment_id) composite key.
type DeploymentStatusRow struct {
	DeploymentID string
	Status       string
	Environment  string
	RepoID       string
}

// ReadDeploymentStatus reads deployments.status/environment for the given
// "repoID:deploymentID" composite ids.
//
// Extracted verbatim from acr devhealthfacts's deployments.go
// readDeploymentStatus (CHAOS-4377); CHAOS-3780's original deployments read.
//
// CHAOS-3781 Tier B: same shape as a CI run -- a deployment's final status
// is only true once it finished. environment is an immutable attribute of
// the deployment, so it needs no temporal treatment.
func ReadDeploymentStatus(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]DeploymentStatusRow, error) {
	statusExpression := "ifNull(d.status, '')"
	if timeBound.Active {
		statusExpression = "if(d.finished_at IS NOT NULL AND d.finished_at <= " + timeBound.AsOfExpression() +
			", ifNull(d.status, ''), 'in_progress')"
	}
	statement := WithRowLimit(`SELECT d.deployment_id, `+statusExpression+`, ifNull(d.environment, ''), toString(d.repo_id)
FROM deployments AS d FINAL
WHERE d.org_id = {org_id:String} AND concat(toString(d.repo_id), ':', d.deployment_id) IN {ids:Array(String)}`+timeBound.ExistencePredicate("coalesce(d.started_at, d.deployed_at)"), DefaultRowLimit)

	var rows []DeploymentStatusRow
	err := QueryOrgScopedNamed(ctx, client, "ReadDeploymentStatus", statement, orgID, ids, func(row RowScanner) error {
		var r DeploymentStatusRow
		if err := row.Scan(&r.DeploymentID, &r.Status, &r.Environment, &r.RepoID); err != nil {
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

// DeployMetricsDailyRow is one deploy_metrics_daily row scanned for a single
// requested repository, already resolved to its latest (deduped) day.
//
// HasDeployTime/HasLeadTime convert the SQL's toUInt8 has-flags to bool for
// a more natural public API; the corresponding duration field is
// meaningless (reads as its ClickHouse-side zero-fill) whenever its flag is
// false.
type DeployMetricsDailyRow struct {
	RepoID                 string
	Day                    string
	DeploymentsCount       int64
	FailedDeploymentsCount int64
	HasDeployTime          bool
	DeployTime             float64
	HasLeadTime            bool
	LeadTime               float64
}

// ReadDeployMetricsDaily reads deploy_metrics_daily (latest day per
// repository) -- CHAOS-4347's repository-scoped deployment aggregate.
//
// Extracted verbatim from acr devhealthfacts's deployments.go
// readRepositoryAggregate (CHAOS-4377). Same tiebreak discipline as
// ReadCICDMetricsDaily, for the identical reason (plain append-only
// MergeTree, no per-row unique id, populated by
// ops/src/dev_health_ops/metrics/compute_deployments.py's daily batch job).
func ReadDeployMetricsDaily(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]DeployMetricsDailyRow, error) {
	statement := WithRowLimit(`SELECT toString(repo_id), toString(day), toInt64(deployments_count), toInt64(failed_deployments_count), toUInt8(isNotNull(deploy_time_p50_hours)), toFloat64(ifNull(deploy_time_p50_hours, 0)), toUInt8(isNotNull(lead_time_p50_hours)), toFloat64(ifNull(lead_time_p50_hours, 0))
FROM (
	SELECT repo_id, day, deployments_count, failed_deployments_count, deploy_time_p50_hours, lead_time_p50_hours,
		row_number() OVER (PARTITION BY repo_id ORDER BY day DESC, computed_at DESC, cityHash64(tuple(deployments_count, failed_deployments_count, ifNull(deploy_time_p50_hours, -1), ifNull(lead_time_p50_hours, -1))) DESC) AS rn
	FROM deploy_metrics_daily
	WHERE org_id = {org_id:String} AND toString(repo_id) IN {ids:Array(String)}`+timeBound.DayPredicate("day")+`
)
WHERE rn = 1`, DefaultRowLimit)

	var rows []DeployMetricsDailyRow
	err := QueryOrgScopedNamed(ctx, client, "ReadDeployMetricsDaily", statement, orgID, ids, func(row RowScanner) error {
		var repoID, day string
		var deploymentsCount, failedDeploymentsCount int64
		var hasDeployTime, hasLeadTime uint8
		var deployTime, leadTime float64
		if err := row.Scan(&repoID, &day, &deploymentsCount, &failedDeploymentsCount, &hasDeployTime, &deployTime, &hasLeadTime, &leadTime); err != nil {
			return err
		}
		rows = append(rows, DeployMetricsDailyRow{
			RepoID: repoID, Day: day, DeploymentsCount: deploymentsCount, FailedDeploymentsCount: failedDeploymentsCount,
			HasDeployTime: hasDeployTime != 0, DeployTime: deployTime,
			HasLeadTime: hasLeadTime != 0, LeadTime: leadTime,
		})
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
