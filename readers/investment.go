package readers

import "context"

// InvestmentDailyRow is one investment_metrics_daily row for a team,
// scanned verbatim off ReadTeamInvestment's query -- one row per
// (team, investment_area, project_stream) triple, never summed or
// aggregated by this reader.
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
	CycleP50Hours      float64
}

// ReadTeamInvestment reads investment_metrics_daily for the given team ids.
//
// investment_metrics_daily is a plain, append-only MergeTree: live data
// shows up to 25 rows sharing one (team_id, investment_area, project_stream,
// day) key (intraday reruns, confirmed against real ClickHouse data). ORDER
// BY day DESC alone leaves that same-day tie unresolved -- computed_at DESC
// breaks it deterministically, and because row_number() (not per-field
// argMax) is used, the winning row is always one whole row, never a
// stitched combination.
//
// day/computed_at is still not a TOTAL order: investment_metrics_daily has
// no per-row unique id, so two rows could share both. cityHash64 of the
// value columns is the final tiebreaker -- arbitrary among an exact tie,
// but stable.
func ReadTeamInvestment(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]InvestmentDailyRow, error) {
	statement := WithRowLimit(`SELECT team_id, investment_area, project_stream, toString(day), toInt64(delivery_units), toInt64(work_items_completed), toInt64(prs_merged), churn_loc, cycle_p50_hours
FROM (
	SELECT team_id, investment_area, project_stream, day, delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours,
		row_number() OVER (PARTITION BY team_id, investment_area, project_stream ORDER BY day DESC, computed_at DESC, cityHash64(tuple(delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours)) DESC) AS rn
	FROM investment_metrics_daily
	WHERE org_id = {org_id:String} AND team_id IN {ids:Array(String)}`+timeBound.DayPredicate("day")+`
)
WHERE rn = 1`, DefaultRowLimit)
	var rows []InvestmentDailyRow
	err := QueryOrgScopedNamed(ctx, client, "ReadTeamInvestment", statement, orgID, ids, func(row RowScanner) error {
		var r InvestmentDailyRow
		if err := row.Scan(&r.TeamID, &r.InvestmentArea, &r.ProjectStream, &r.Day, &r.DeliveryUnits, &r.WorkItemsCompleted, &r.PRsMerged, &r.ChurnLOC, &r.CycleP50Hours); err != nil {
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

// InvestmentProjectRow is one (project, team, investment_area,
// project_stream) row contributing to a project's investment rollup,
// scanned verbatim off ReadProjectInvestment's team_project_ownership join.
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
	CycleP50Hours      float64
}

// ReadProjectInvestment rolls investment_metrics_daily up for a project
// through projects -> team_project_ownership -> investment_metrics_daily:
// every team owning the project contributes its own latest (area, stream)
// rows. Investment rows are never summed across teams here: a team's
// investment breakdown is partitioned by (investment_area, project_stream),
// and summing across teams that report against DIFFERENT areas would mix
// unrelated categories into one meaningless total.
func ReadProjectInvestment(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]InvestmentProjectRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ownershipPredicate := OwnershipValidityPredicate(timeBound)
	statement := WithRowLimit(`SELECT concat(p.provider, ':', p.id), p.team_id, ifNull(t.name, ''), im.investment_area, im.project_stream, toString(im.day), toInt64(im.delivery_units), toInt64(im.work_items_completed), toInt64(im.prs_merged), im.churn_loc, im.cycle_p50_hours
FROM `+ProjectOwnershipJoinSQL(ownershipPredicate)+`
INNER JOIN (
	SELECT team_id, investment_area, project_stream, day, delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours,
		row_number() OVER (PARTITION BY team_id, investment_area, project_stream ORDER BY day DESC, computed_at DESC, cityHash64(tuple(delivery_units, work_items_completed, prs_merged, churn_loc, cycle_p50_hours)) DESC) AS rn
	FROM investment_metrics_daily
	WHERE org_id = {org_id:String}`+timeBound.DayPredicate("day")+`
) AS im ON im.team_id = p.team_id AND im.rn = 1
LEFT JOIN (SELECT id, name FROM teams FINAL WHERE org_id = {org_id:String}) AS t ON t.id = p.team_id
ORDER BY p.id, p.team_id, im.investment_area, im.project_stream`, DefaultRowLimit)
	var rows []InvestmentProjectRow
	err := QueryOrgScopedNamed(ctx, client, "ReadProjectInvestment", statement, orgID, ids, func(row RowScanner) error {
		var r InvestmentProjectRow
		if err := row.Scan(&r.ProjectSubjectKey, &r.TeamID, &r.TeamName, &r.InvestmentArea, &r.ProjectStream, &r.Day, &r.DeliveryUnits, &r.WorkItemsCompleted, &r.PRsMerged, &r.ChurnLOC, &r.CycleP50Hours); err != nil {
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
