package readers

import (
	"context"
	"strconv"
)

// WorkloadForecastRow is one capacity_forecasts row for a team, scanned
// verbatim off ReadTeamWorkload's query -- one row per (team_id,
// work_scope_id) pair a team is forecast under, never collapsed into a
// single arbitrarily-chosen scope.
//
// HasP50Days/InsufficientHistory/HighVariance are the raw
// toUInt8(...)-projected boolean columns, scanned as the source represents
// them; a caller converts to Go bool (!= 0) as needed.
type WorkloadForecastRow struct {
	TeamID              string
	WorkScopeID         string
	ThroughputMean      float64
	ThroughputStddev    float64
	HasP50Days          uint8
	P50Days             int64
	InsufficientHistory uint8
	HighVariance        uint8
	BacklogSize         int64
	ComputedAt          string
}

// ReadTeamWorkload reads capacity_forecasts for the given team ids.
//
// row_number() OVER (PARTITION BY team_id, work_scope_id ORDER BY
// computed_at DESC) picks the single most recently computed forecast for
// EACH scope a team has been forecast under, never collapsing distinct
// scopes into one another -- live data shows one team with 12 concurrent
// scopes, computed within the same batch, with wildly different
// throughput/percentile values. FINAL is defensive: capacity_forecasts is
// ReplacingMergeTree(computed_at) sorted on (org_id, forecast_id), so FINAL
// only collapses a re-emitted identical forecast_id, not distinct scopes --
// the row_number() partition is what actually resolves that.
//
// computed_at DESC alone is not a TOTAL order: two forecasts for the same
// scope could share a computed_at. Unlike the other readers in this
// package, capacity_forecasts DOES carry a real per-row unique id --
// forecast_id -- so this reader uses that as the final tiebreaker instead
// of a value hash.
func ReadTeamWorkload(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]WorkloadForecastRow, error) {
	statement := WithRowLimit(`SELECT team_id, ifNull(work_scope_id, ''), throughput_mean, throughput_stddev, toUInt8(isNotNull(p50_days)), toInt64(ifNull(p50_days, 0)), insufficient_history, high_variance, toInt64(backlog_size), toString(computed_at)
FROM (
	SELECT ifNull(team_id, '') AS team_id, work_scope_id, throughput_mean, throughput_stddev, p50_days, insufficient_history, high_variance, backlog_size, computed_at,
		row_number() OVER (PARTITION BY team_id, work_scope_id ORDER BY computed_at DESC, forecast_id DESC) AS rn
	FROM capacity_forecasts FINAL
	WHERE org_id = {org_id:String} AND team_id IN {ids:Array(String)}`+timeBound.TimestampPredicate("computed_at")+`
)
WHERE rn = 1
ORDER BY computed_at DESC, team_id, work_scope_id`, DefaultRowLimit)
	var rows []WorkloadForecastRow
	err := QueryOrgScopedNamed(ctx, client, "ReadTeamWorkload", statement, orgID, ids, func(row RowScanner) error {
		var r WorkloadForecastRow
		if err := row.Scan(&r.TeamID, &r.WorkScopeID, &r.ThroughputMean, &r.ThroughputStddev, &r.HasP50Days, &r.P50Days, &r.InsufficientHistory, &r.HighVariance, &r.BacklogSize, &r.ComputedAt); err != nil {
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

// WorkloadProjectRow is one (project, team, work_scope) row contributing to
// a project's workload rollup, scanned verbatim off ReadProjectWorkload's
// project-identity join. Multiple rows can share ProjectSubjectKey:
// Monte Carlo throughput/percentile stats are never additive across teams
// (summing two independent forecasts' throughput_mean is not a meaningful
// number), so every owning team's own latest per-scope forecast survives
// verbatim. Grouping/breakdown-table construction is the caller's job.
type WorkloadProjectRow struct {
	ProjectSubjectKey string
	// HasTeam distinguishes an UNATTRIBUTED row from an attributed one --
	// see ReadinessProjectRow.HasTeam for why the case became reachable.
	// capacity_forecasts.team_id is Nullable too.
	HasTeam             uint8
	TeamID              string
	TeamName            string
	WorkScopeID         string
	ThroughputMean      float64
	ThroughputStddev    float64
	HasP50Days          uint8
	P50Days             int64
	InsufficientHistory uint8
	HighVariance        uint8
	BacklogSize         int64
	ComputedAt          string
}

// ReadProjectWorkload rolls capacity_forecasts up for a project through
// projects -> team_project_ownership -> capacity_forecasts: every team
// owning the project contributes its own latest per-scope forecast,
// verbatim -- Monte Carlo throughput/percentile stats are never summed or
// averaged across teams.
func ReadProjectWorkload(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]WorkloadProjectRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// CHAOS-4521b: the project's OWN capacity_forecasts rows, matched on
	// work_scope_id, with no team-ownership hop. Same reasoning as
	// ReadProjectReadiness -- including keeping team_id in the rn
	// partition, and the reported team coming from the forecast row
	// itself. Monte Carlo statistics from different teams cannot be
	// merged, so one forecast per (team, work scope) is the only correct
	// grain here.
	//
	// capacity_forecasts has no `provider` column at all, which is the
	// second reason ProjectIdentityMatchSQL does not match on provider:
	// a predicate this table cannot express must not be one the shared
	// helper requires.
	// has_team beside team_id, and team_id back in the ORDER BY -- same
	// reasoning as ReadProjectReadiness. (scope, team) is unique per
	// project after rn = 1, so the ordering is total; capacity_forecasts
	// has no provider column to add.
	statement := WithRowLimit(`SELECT concat(p.provider, ':', p.id), cf.has_team, cf.team_key, ifNull(t.name, ''), ifNull(cf.work_scope_id, ''), cf.throughput_mean, cf.throughput_stddev, toUInt8(isNotNull(cf.p50_days)), toInt64(ifNull(cf.p50_days, 0)), cf.insufficient_history, cf.high_variance, toInt64(cf.backlog_size), toString(cf.computed_at)
FROM `+ProjectIdentityJoinSQL()+`
INNER JOIN (
	SELECT toUInt8(isNotNull(team_id)) AS has_team, ifNull(team_id, '') AS team_key, work_scope_id, throughput_mean, throughput_stddev, p50_days, insufficient_history, high_variance, backlog_size, computed_at,
		row_number() OVER (PARTITION BY team_id, work_scope_id ORDER BY computed_at DESC, forecast_id DESC) AS rn
	FROM capacity_forecasts FINAL
	WHERE org_id = {org_id:String}`+timeBound.TimestampPredicate("computed_at")+`
) AS cf ON `+ProjectIdentityMatchSQL("cf", "work_scope_id")+` AND cf.rn = 1
LEFT JOIN (SELECT id, name FROM teams FINAL WHERE org_id = {org_id:String}) AS t ON t.id = cf.team_key
ORDER BY p.id, p.provider, cf.work_scope_id, cf.has_team, cf.team_key`, DefaultRowLimit)
	var rows []WorkloadProjectRow
	err := QueryOrgScopedNamed(ctx, client, "ReadProjectWorkload", statement, orgID, ids, func(row RowScanner) error {
		var r WorkloadProjectRow
		if err := row.Scan(&r.ProjectSubjectKey, &r.HasTeam, &r.TeamID, &r.TeamName, &r.WorkScopeID, &r.ThroughputMean, &r.ThroughputStddev, &r.HasP50Days, &r.P50Days, &r.InsufficientHistory, &r.HighVariance, &r.BacklogSize, &r.ComputedAt); err != nil {
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

// CompletionDayBinLimit is the most completion-day bins one forecast row
// carries: the ops Monte Carlo caps a run at a 365-day horizon, so a
// fixed-scope distribution holds at most 366 distinct values (0 to 365). The
// reader reads one entry more than this from each array, as evidence, so a
// caller can tell a row at the cap from a row past it (CompletionDayBins
// says how long the stored array really is).
const CompletionDayBinLimit = 366

// CompletionForecastRow is the latest capacity_forecasts row for one
// (team, work scope), with its fixed-scope Monte Carlo distribution, scanned
// verbatim. It serves the completion forecast render (CHAOS-4613,
// CHAOS-12584); nothing about the curve is computed here.
//
// HasWorkScope keeps the two empty scopes apart that the ops writer keeps
// apart: 0 is a NULL work_scope_id (a forecast scoped to no work scope), 1
// with WorkScopeID "" is discovery's unscoped row. Both are real, distinct
// forecasts; collapsing them would serve one as the other.
//
// The HasP*Days flags and the empty date strings carry the NULLs of the
// Nullable percentile columns: no percentile is not a percentile of 0.
//
// CompletionDaysValues and CompletionDaysCounts are parallel (values
// ascending, counts summing to SimulationCount when present). Empty arrays
// mean no distribution (a row written before ops migration 101, or a run
// that produced none), never a distribution of zeros. Each array is read to
// at most CompletionDayBinLimit+1 entries; CompletionDayBins and
// CompletionCountBins are the stored lengths, so a caller can refuse a
// distribution that is over the limit or whose arrays disagree in length.
type CompletionForecastRow struct {
	TeamID          string
	HasWorkScope    uint8
	WorkScopeID     string
	ComputedAt      string
	SimulationCount uint32
	BacklogSize     int64
	// HasTargetItems and TargetItems (CHAOS-12634) are the item target the
	// fixed-scope run forecast: the scope's own target, or its backlog as
	// ops' worker falls back to it. A NULL target_items is HasTargetItems 0,
	// never a target of 0.
	HasTargetItems       uint8
	TargetItems          int64
	HasP50Days           uint8
	P50Days              int64
	HasP85Days           uint8
	P85Days              int64
	HasP95Days           uint8
	P95Days              int64
	P50Date              string
	P85Date              string
	P95Date              string
	InsufficientHistory  uint8
	HighVariance         uint8
	CompletionDayBins    uint64
	CompletionCountBins  uint64
	CompletionDaysValues []uint16
	CompletionDaysCounts []uint32
}

// ReadTeamCompletionForecasts reads, for the given team ids, the latest
// capacity_forecasts row of each (team, work scope) with its percentiles and
// its fixed-scope distribution. See ReadTeamCompletionForecastsWithRowLimit.
func ReadTeamCompletionForecasts(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]CompletionForecastRow, error) {
	return ReadTeamCompletionForecastsWithRowLimit(ctx, client, orgID, ids, timeBound, DefaultRowLimit)
}

// ReadTeamCompletionForecastsWithRowLimit is ReadTeamCompletionForecasts with
// the output row bound named by the caller; one row per (team, work scope),
// so a caller passing ProbeRowLimit can tell a population at its cap from
// one past it.
//
// The latest row is chosen as ReadTeamWorkload chooses it, and for the same
// reasons: capacity_forecasts is ReplacingMergeTree(computed_at) on
// (org_id, forecast_id) with a new forecast_id per write, so FINAL does not
// pick a latest forecast; row_number() over (team_id, work_scope_id) by
// computed_at, with forecast_id as the final tiebreaker, does. work_scope_id
// is partitioned as stored (Nullable), so NULL and "" stay two scopes.
//
// Rows are ordered by team, then NULL scope before any named scope, then
// scope, which is a total order on the output.
func ReadTeamCompletionForecastsWithRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound, rowLimit int) ([]CompletionForecastRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	binProbe := strconv.Itoa(CompletionDayBinLimit + 1)
	statement := WithRowLimit(`SELECT team_key, has_work_scope, ifNull(work_scope_id, ''), toString(computed_at), simulation_count, toInt64(backlog_size),
	toUInt8(isNotNull(target_items)), toInt64(ifNull(target_items, 0)),
	toUInt8(isNotNull(p50_days)), toInt64(ifNull(p50_days, 0)),
	toUInt8(isNotNull(p85_days)), toInt64(ifNull(p85_days, 0)),
	toUInt8(isNotNull(p95_days)), toInt64(ifNull(p95_days, 0)),
	ifNull(toString(p50_date), ''), ifNull(toString(p85_date), ''), ifNull(toString(p95_date), ''),
	insufficient_history, high_variance,
	toUInt64(length(completion_days_values)), toUInt64(length(completion_days_counts)),
	arraySlice(completion_days_values, 1, `+binProbe+`), arraySlice(completion_days_counts, 1, `+binProbe+`)
FROM (
	SELECT ifNull(team_id, '') AS team_key, toUInt8(isNotNull(work_scope_id)) AS has_work_scope, work_scope_id, computed_at, simulation_count, backlog_size,
		target_items, p50_days, p85_days, p95_days, p50_date, p85_date, p95_date, insufficient_history, high_variance,
		completion_days_values, completion_days_counts,
		row_number() OVER (PARTITION BY team_id, work_scope_id ORDER BY computed_at DESC, forecast_id DESC) AS rn
	FROM capacity_forecasts FINAL
	WHERE org_id = {org_id:String} AND team_id IN {ids:Array(String)}`+timeBound.TimestampPredicate("computed_at")+`
)
WHERE rn = 1
ORDER BY team_key, has_work_scope, work_scope_id`, rowLimit)
	var rows []CompletionForecastRow
	err := QueryOrgScopedNamed(ctx, client, "ReadTeamCompletionForecasts", statement, orgID, ids, func(row RowScanner) error {
		var r CompletionForecastRow
		if err := row.Scan(&r.TeamID, &r.HasWorkScope, &r.WorkScopeID, &r.ComputedAt, &r.SimulationCount, &r.BacklogSize,
			&r.HasTargetItems, &r.TargetItems,
			&r.HasP50Days, &r.P50Days, &r.HasP85Days, &r.P85Days, &r.HasP95Days, &r.P95Days,
			&r.P50Date, &r.P85Date, &r.P95Date, &r.InsufficientHistory, &r.HighVariance,
			&r.CompletionDayBins, &r.CompletionCountBins, &r.CompletionDaysValues, &r.CompletionDaysCounts); err != nil {
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
