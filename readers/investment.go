package readers

import "context"

// InvestmentDailyRow is one (team, investment_area, project_stream) row at
// that triple's latest day, scanned off ReadTeamInvestment's query. Its counts
// are the sum over the repositories that wrote rows for that day, each
// repository's NEWEST row only (see investmentLatestDaySQL); CycleP50Hours is
// not combined: see CycleP50Known.
//
// ChurnLOC is the raw source column value (UInt64 in ClickHouse, NOT
// wrapped with toInt64 in SQL): the column can exceed math.MaxInt64, and
// wrapping it would silently turn such a value negative. A caller that
// needs an int64 must range-check it itself (see acr devhealthfacts's
// representableInt64) rather than assume the wrap is safe.
type InvestmentDailyRow struct {
	TeamID             string
	InvestmentArea     string
	ProjectStream      string
	Day                string
	DeliveryUnits      int64
	WorkItemsCompleted int64
	PRsMerged          int64
	ChurnLOC           uint64
	// CycleP50Hours is the repository's median cycle time in hours. It is a
	// median and cannot be combined across repositories, so it is a value
	// ONLY when CycleP50Known is true; otherwise it is 0, and 0 is not a
	// value there.
	CycleP50Hours float64
	// CycleP50Known is true when exactly one repository row stands behind the
	// key, so CycleP50Hours is that row's own median.
	CycleP50Known bool
}

// investmentLatestDaySQL is the shared body of both investment readers: one
// row per (team_id, investment_area, project_stream) for that triple's latest
// day, summed across the repositories that wrote rows for that day.
//
// investment_metrics_daily is append-only and one run writes one row per
// repository, so the table's identity for "the newest row" is
// (org_id, day, repo_id, team_id, investment_area, project_stream). The
// dedupe therefore runs per (day, repo) and only then adds the repositories
// up; deduping on the triple alone served one repository's row and dropped
// the rest. A repository whose newest row is zero counts as zero: the
// dedupe never falls back to an older non-zero row.
//
// repo_id NULL and the nil UUID both mean "no repository" and are one key
// (ifNull), so a legacy NULL row and a nil-UUID row cannot both count.
//
// cycle_p50_hours is a median and cannot be added or blended. It is served as
// a value (cycle_p50_known = 1) only when exactly one repository row stands
// behind the key; with more than one it is 0 and cycle_p50_known = 0. The
// ops writer stores no population column for the median, so a row count is
// the only provable rule.
//
// Rows of one key that share computed_at fall to cityHash64 of the value
// columns (larger wins): arbitrary among an exact tie, but stable and always
// one whole row, never a stitched combination.
func investmentLatestDaySQL(filters string) string {
	return `SELECT team_id, investment_area, project_stream, day, total_delivery_units, total_work_items_completed, total_prs_merged, total_churn_loc, exact_cycle_p50_hours, cycle_p50_known
	FROM (
		SELECT team_id, investment_area, project_stream, day, total_delivery_units, total_work_items_completed, total_prs_merged, total_churn_loc, exact_cycle_p50_hours, cycle_p50_known,
			row_number() OVER (PARTITION BY team_id, investment_area, project_stream ORDER BY day DESC) AS day_rn
		FROM (
			SELECT team_id, investment_area, project_stream, day,
				toInt64(sum(delivery_units)) AS total_delivery_units, toInt64(sum(work_items_completed)) AS total_work_items_completed, toInt64(sum(prs_merged)) AS total_prs_merged, sum(churn_loc) AS total_churn_loc,
				if(count() = 1, sum(cycle_p50_hours), 0) AS exact_cycle_p50_hours, toUInt8(count() = 1) AS cycle_p50_known
			FROM (
				SELECT team_id, investment_area, project_stream, day, delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours,
					row_number() OVER (PARTITION BY team_id, investment_area, project_stream, day, ifNull(repo_id, toUUID('00000000-0000-0000-0000-000000000000')) ORDER BY computed_at DESC, cityHash64(tuple(delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours)) DESC) AS rn
				FROM investment_metrics_daily
				WHERE org_id = {org_id:String}` + filters + `
			)
			WHERE rn = 1
			GROUP BY team_id, investment_area, project_stream, day
		)
	)
	WHERE day_rn = 1`
}

// ReadTeamInvestment reads investment_metrics_daily for the given team ids.
//
// investment_metrics_daily is a plain, append-only MergeTree: live data
// shows up to 25 rows sharing one (team_id, investment_area, project_stream,
// day, repo_id) key (intraday reruns, confirmed against real ClickHouse
// data), and one run writes one row per repository. The newest row per key
// is taken with row_number() (not per-field argMax), so the winner is
// always one whole row, never a stitched combination; the repositories are
// then summed. See investmentLatestDaySQL for the full rule, including the
// computed_at tie-break.
func ReadTeamInvestment(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]InvestmentDailyRow, error) {
	statement := WithRowLimit(`SELECT team_id, investment_area, project_stream, toString(day), total_delivery_units, total_work_items_completed, total_prs_merged, total_churn_loc, exact_cycle_p50_hours, cycle_p50_known
FROM (
`+investmentLatestDaySQL(" AND team_id IN {ids:Array(String)}"+timeBound.DayPredicate("day"))+`
)
ORDER BY day DESC, team_id, investment_area, project_stream`, DefaultRowLimit)
	var rows []InvestmentDailyRow
	err := QueryOrgScopedNamed(ctx, client, "ReadTeamInvestment", statement, orgID, ids, func(row RowScanner) error {
		var r InvestmentDailyRow
		var cycleKnown uint8
		if err := row.Scan(&r.TeamID, &r.InvestmentArea, &r.ProjectStream, &r.Day, &r.DeliveryUnits, &r.WorkItemsCompleted, &r.PRsMerged, &r.ChurnLOC, &r.CycleP50Hours, &cycleKnown); err != nil {
			return err
		}
		r.CycleP50Known = cycleKnown != 0
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// InvestmentProjectRow is one (project, team, investment_area,
// project_stream) row contributing to a project's investment rollup,
// scanned off ReadProjectInvestment's team_project_ownership join. Its counts
// are summed over repositories exactly as InvestmentDailyRow's are.
// Multiple rows can share ProjectSubjectKey: a project rolls up through
// every owning team's own (area, stream) rows, never summed across teams
// (investment counts are not additive across areas/streams -- there is no
// shared unit across areas at all). Grouping/breakdown-table construction
// is the caller's job; this reader returns one Row per ClickHouse row,
// nothing smarter.
type InvestmentProjectRow struct {
	ProjectSubjectKey  string
	TeamID             string
	TeamName           string
	InvestmentArea     string
	ProjectStream      string
	Day                string
	DeliveryUnits      int64
	WorkItemsCompleted int64
	PRsMerged          int64
	ChurnLOC           uint64
	// CycleP50Hours is the repository's median cycle time in hours. It is a
	// median and cannot be combined across repositories, so it is a value
	// ONLY when CycleP50Known is true; otherwise it is 0, and 0 is not a
	// value there.
	CycleP50Hours float64
	// CycleP50Known is true when exactly one repository row stands behind the
	// key, so CycleP50Hours is that row's own median.
	CycleP50Known bool
}

// ReadProjectInvestment rolls investment_metrics_daily up for a project
// through projects -> team_project_ownership -> investment_metrics_daily:
// every team owning the project contributes its own latest (area, stream)
// rows, each summed across that team's repositories (the dedupe is per
// repository, see investmentLatestDaySQL). Investment rows are never summed
// across teams here: a team's
// investment breakdown is partitioned by (investment_area, project_stream),
// and summing across teams that report against DIFFERENT areas would mix
// unrelated categories into one meaningless total.
func ReadProjectInvestment(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]InvestmentProjectRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ownershipPredicate := OwnershipValidityPredicate(timeBound)
	statement := WithRowLimit(`SELECT concat(p.provider, ':', p.id), p.team_id, ifNull(t.name, ''), im.investment_area, im.project_stream, toString(im.day), im.total_delivery_units, im.total_work_items_completed, im.total_prs_merged, im.total_churn_loc, im.exact_cycle_p50_hours, im.cycle_p50_known
FROM `+ProjectOwnershipJoinSQL(ownershipPredicate)+`
INNER JOIN (
`+investmentLatestDaySQL(timeBound.DayPredicate("day"))+`
) AS im ON im.team_id = p.team_id
LEFT JOIN (SELECT id, name FROM teams FINAL WHERE org_id = {org_id:String}) AS t ON t.id = p.team_id
ORDER BY p.id, p.provider, p.team_id, im.investment_area, im.project_stream`, DefaultRowLimit)
	var rows []InvestmentProjectRow
	err := QueryOrgScopedNamed(ctx, client, "ReadProjectInvestment", statement, orgID, ids, func(row RowScanner) error {
		var r InvestmentProjectRow
		var cycleKnown uint8
		if err := row.Scan(&r.ProjectSubjectKey, &r.TeamID, &r.TeamName, &r.InvestmentArea, &r.ProjectStream, &r.Day, &r.DeliveryUnits, &r.WorkItemsCompleted, &r.PRsMerged, &r.ChurnLOC, &r.CycleP50Hours, &cycleKnown); err != nil {
			return err
		}
		r.CycleP50Known = cycleKnown != 0
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
