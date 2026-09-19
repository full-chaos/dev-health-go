package readers

import "context"

// ProjectThemeMixRow is one project's canonical investment theme mix
// attributed through the project's OWN work items, scanned off
// ReadProjectThemeMix. It is one row per project that has at least one
// attributed work unit; a project with none has no row.
//
// The five theme fields are `effort_value * theme_share` summed across the
// project's work units with positive effort; a caller normalizes by their
// sum. BugfixWeighted is the same weighting over the one tracked
// subcategory (BugfixSubcategoryKey).
//
// The population fields are computed by the same uncapped aggregate as the
// theme sums, never from the capped output rows:
//
//   - WorkUnits: distinct work units attributed to the project.
//   - EffortUnits: the subset with effort_value > 0 (the only units that
//     carry weight in the theme sums).
//   - SpanningUnits: the subset that is ALSO attributed to at least one
//     other project in the organization. Each project's mix counts a
//     spanning unit in full: a project owns the mix of the work that touches
//     it, and the pair count is the disclosure of that overlap.
type ProjectThemeMixRow struct {
	ProjectSubjectKey string
	FeatureDelivery   float64
	Operational       float64
	Maintenance       float64
	Quality           float64
	Risk              float64
	BugfixWeighted    float64
	WorkUnits         uint64
	EffortUnits       uint64
	SpanningUnits     uint64
	// MultiPlacedUnits counts the work units attributed to this project
	// through at least one work item id that project membership places under
	// more than one repository. Such a unit counts in full for every project it
	// is placed in, exactly like a spanning unit; the count is the disclosure
	// of that placement, so a data defect (one item registered twice) is
	// visible instead of silently inflating a mix. It is a subset of
	// WorkUnits.
	MultiPlacedUnits uint64
}

// ReadProjectThemeMix reads the canonical investment theme distribution
// (`work_unit_investments.theme_distribution_json`, persisted at
// categorization time, never recomputed here) attributed to the given
// projects through the PROJECTS' OWN work items: a work unit belongs to a
// project when one of its structural_evidence_json `issues` refs is a work
// item that project_membership_presence places in that project.
//
// A work item id that project_membership_presence places under more than one
// repository attributes to every project it is placed in, and each such unit
// is counted in MultiPlacedUnits.
//
// Only issue refs carry a project. A `prs` ref names a pull request, which
// has no project of its own, so a work unit whose evidence is pull requests
// only is not attributed to any project here.
//
// ids are project subject keys (`provider:id`), resolved through
// ProjectIdentityJoinSQL like every other project reader. A work unit that
// touches several projects counts in full for each of them; one work unit is
// never counted twice within one project (a work unit naming several items of
// one project, or one item under both the project's id and its key, is one
// unit). Work units with no positive effort
// add nothing to the sums and are reported in WorkUnits only.
//
// timeBound is expected to be Active; the work unit's own [from_ts, to_ts)
// validity range is tested for overlap with the window, the same test
// ReadTeamThemeMix applies.
func ReadProjectThemeMix(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]ProjectThemeMixRow, error) {
	return ReadProjectThemeMixWithRowLimit(ctx, client, orgID, ids, timeBound, DefaultRowLimit)
}

// ReadProjectThemeMixWithRowLimit is ReadProjectThemeMix with the output row
// bound named by the caller. The statement returns at most one row per
// requested project, so a caller passing ProbeRowLimit can tell a population
// exactly at its cap from one that overflowed it.
func ReadProjectThemeMixWithRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound, rowLimit int) ([]ProjectThemeMixRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	statement := WithRowLimit(`
SELECT project_key, feature_delivery, operational, maintenance, quality, risk, bugfix_weighted, work_units, effort_units, spanning_units, multi_placed_units FROM (
WITH latest AS (
    SELECT
        work_unit_id,
        argMax(from_ts, computed_at) AS from_ts,
        argMax(to_ts, computed_at) AS to_ts,
        argMax(effort_value, computed_at) AS effort_value,
        argMax(theme_distribution_json, computed_at) AS theme_distribution_json,
        argMax(subcategory_distribution_json, computed_at) AS subcategory_distribution_json,
        argMax(structural_evidence_json, computed_at) AS structural_evidence_json
    FROM work_unit_investments
    WHERE org_id = {org_id:String}
    GROUP BY work_unit_id
),
windowed AS (
    SELECT * FROM latest`+timeBound.rangePredicate("from_ts", "to_ts")+`
),
unit_issue AS (
    SELECT work_unit_id, issue_ref
    FROM windowed
    ARRAY JOIN JSONExtract(structural_evidence_json, 'issues', 'Array(String)') AS issue_ref
),
item_project AS (
    SELECT work_item_id, project_id, toUInt8(repo_count > 1) AS multi_placed
    FROM (
        SELECT subject_id AS work_item_id, project_id,
            uniqExact(repo_id) OVER (PARTITION BY subject_id) AS repo_count
        FROM project_membership_presence
        WHERE org_id = {org_id:String} AND subject_kind = 'work_item'
    )
),
unit_project AS (
    SELECT ui.work_unit_id AS work_unit_id, ip.project_id AS project_id, ip.multi_placed AS multi_placed
    FROM unit_issue AS ui
    INNER JOIN item_project AS ip ON ip.work_item_id = ui.issue_ref
),
resolved AS (
    SELECT project_provider, project_id, work_unit_id, max(multi_placed) AS multi_placed
    FROM (
        SELECT p.provider AS project_provider, p.id AS project_id, up.work_unit_id AS work_unit_id, up.multi_placed AS multi_placed
        FROM `+ProjectIdentityCatalogSQL()+`
        INNER JOIN unit_project AS up ON up.project_id = p.scope
    )
    GROUP BY project_provider, project_id, work_unit_id
),
unit_span AS (
    SELECT work_unit_id, uniqExact(project_provider, project_id) AS project_count
    FROM resolved
    GROUP BY work_unit_id
),
attributed AS (
    SELECT project_provider, project_id, work_unit_id, multi_placed
    FROM resolved
    WHERE concat(project_provider, ':', project_id) IN {ids:Array(String)}
)
SELECT
    concat(a.project_provider, ':', a.project_id) AS project_key,
    sumIf(w.theme_distribution_json['feature_delivery'] * w.effort_value, w.effort_value > 0) AS feature_delivery,
    sumIf(w.theme_distribution_json['operational'] * w.effort_value, w.effort_value > 0) AS operational,
    sumIf(w.theme_distribution_json['maintenance'] * w.effort_value, w.effort_value > 0) AS maintenance,
    sumIf(w.theme_distribution_json['quality'] * w.effort_value, w.effort_value > 0) AS quality,
    sumIf(w.theme_distribution_json['risk'] * w.effort_value, w.effort_value > 0) AS risk,
    sumIf(ifNull(w.subcategory_distribution_json[{bugfix_key:String}], 0.0) * w.effort_value, w.effort_value > 0) AS bugfix_weighted,
    count() AS work_units,
    countIf(w.effort_value > 0) AS effort_units,
    countIf(s.project_count > 1) AS spanning_units,
    countIf(a.multi_placed = 1) AS multi_placed_units
FROM attributed AS a
INNER JOIN windowed AS w ON w.work_unit_id = a.work_unit_id
LEFT JOIN unit_span AS s ON s.work_unit_id = a.work_unit_id
GROUP BY a.project_provider, a.project_id
ORDER BY project_key
)`, rowLimit)

	extra := append(append([]Binding{}, timeBound.Bindings()...), Binding{Name: "bugfix_key", Value: BugfixSubcategoryKey})
	var rows []ProjectThemeMixRow
	err := QueryOrgScopedNamed(ctx, client, "ReadProjectThemeMix", statement, orgID, ids, func(row RowScanner) error {
		var r ProjectThemeMixRow
		if scanErr := row.Scan(&r.ProjectSubjectKey, &r.FeatureDelivery, &r.Operational, &r.Maintenance, &r.Quality, &r.Risk, &r.BugfixWeighted, &r.WorkUnits, &r.EffortUnits, &r.SpanningUnits, &r.MultiPlacedUnits); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, extra...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
