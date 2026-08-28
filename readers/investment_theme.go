package readers

import "context"

// TeamThemeMixRow is one (team, canonical-theme-or-tracked-subcategory)
// weighted-effort contribution, scanned off ReadTeamThemeMix's query.
//
// Key is either one of the five canonical investment themes
// (feature_delivery, operational, maintenance, quality, risk --
// ops/src/dev_health_ops/investment_taxonomy.py's THEMES, deterministic
// theme roll-up, AGENTS.md: "no synonyms/overrides") when Kind is "theme",
// or the single tracked subcategory "quality.bugfix" when Kind is
// "subcategory" -- the only subcategory CHAOS-4398's cohort-ranking
// reactive-share signal needs (a team's unplanned/bug work is theme
// `operational` PLUS this one bugfix subcategory share; see
// ops/.../investment_mix_explain.py's deterministic label pattern this
// mirrors). WeightedEffort is `effort_value * theme_share` (or
// `effort_value * subcategory_share`) summed across every work unit
// attributed to the team inside the requested window -- a caller
// normalizes by the team's own theme total (the sum of the five "theme"
// rows) to get a share in [0,1], never by a cross-team total.
type TeamThemeMixRow struct {
	TeamID         string
	TeamName       string
	Kind           string // "theme" | "subcategory"
	Key            string
	WeightedEffort float64
}

// BugfixSubcategoryKey is the one subcategory ReadTeamThemeMix tracks
// alongside the five canonical themes -- see TeamThemeMixRow's doc comment.
const BugfixSubcategoryKey = "quality.bugfix"

// ReadTeamThemeMix reads the canonical investment theme distribution
// (`latest_work_unit_investments`' `theme_distribution_json` /
// `subcategory_distribution_json` -- computed once at categorization time,
// never recomputed here) attributed to the given teams over timeBound,
// weighted by each work unit's `effort_value`.
//
// CHAOS-4398 §0 (cohort-answer-plan.md): this is a NEW acr producer join,
// not a reuse of investment.go's ReadTeamInvestment/ReadProjectInvestment
// -- those read `investment_metrics_daily`, fed by the deprecated legacy
// rule set (`investment_areas.yaml`: "do not use this file for canonical
// WorkUnit categorization"), whose `investment_area` values are NOT the
// canonical 5-theme taxonomy. The canonical distribution lives on
// `work_unit_investments` and is team-scoped only through a work unit's
// OWN evidence (structural_evidence_json's `issues`/`prs` refs), resolved
// to the CHAOS-2600 ownership-precedence winner recorded in
// `work_item_team_attributions` (is_primary = 1, latest by computed_at) --
// exactly the majority-vote bridge
// `ops/src/dev_health_ops/api/queries/investment.py`'s
// `build_unit_team_subquery`/`PRIMARY_WORK_ITEM_TEAM_ATTRIBUTION_SOURCE`
// already computes for the Investment view; this reader ports that SQL
// shape (never `author_membership`/`assignee_membership` -- CHAOS-4321:
// ownership only, never a person's memberships).
//
// A `prs` evidence ref only resolves through `repos` when its repo UUID
// maps to EXACTLY one provider inside the org (a `github`/`gitlab` UUID
// collision fails closed to an empty, non-matching id -- see
// investment.py's `RESOLVED_EVIDENCE_WORK_ITEM_ID` comment): this reader
// reproduces that same fail-closed arm rather than guessing a provider.
//
// A work unit whose evidence resolves to more than one team is attributed
// to the team with the most distinct resolved work items (ties broken by
// the lexicographically largest team_id) -- the same majority vote
// `build_unit_team_subquery` performs, never summed or split across teams.
// A work unit with no resolvable team contributes to no row at all (never
// a synthetic "unassigned" team_id).
//
// timeBound is expected to be Active (a point-in-time-free range: the
// caller resolves both the current and prior comparable window as two
// separate calls with two different TimeBounds -- CHAOS-4040: never an
// model-inferred date, both windows are the caller's own explicit,
// disclosed resolution).
func ReadTeamThemeMix(ctx context.Context, client QueryClient, orgID string, teamIDs []string, timeBound TimeBound) ([]TeamThemeMixRow, error) {
	if len(teamIDs) == 0 {
		return nil, nil
	}
	statement := WithRowLimit(`
SELECT team_id, team_name, kind, key, weighted_effort FROM (
WITH latest AS (
    SELECT
        work_unit_id,
        argMax(from_ts, computed_at) AS from_ts,
        argMax(to_ts, computed_at) AS to_ts,
        argMax(effort_value, computed_at) AS effort_value,
        argMax(theme_distribution_json, computed_at) AS theme_distribution_json,
        argMax(subcategory_distribution_json, computed_at) AS subcategory_distribution_json,
        argMax(structural_evidence_json, computed_at) AS structural_evidence_json,
        org_id
    FROM work_unit_investments
    WHERE org_id = {org_id:String}
    GROUP BY org_id, work_unit_id
),
windowed AS (
    SELECT * FROM latest`+timeBound.rangePredicate("from_ts", "to_ts")+`
),
repo_lookup AS (
    SELECT
        org_id, toString(id) AS repo_uuid,
        argMax(repo, last_synced) AS repo,
        if(uniqExact(provider) = 1, argMax(provider, last_synced), '') AS provider
    FROM repos
    WHERE org_id = {org_id:String}
    GROUP BY org_id, id
),
wita AS (
    SELECT work_item_id, team_id, team_name
    FROM work_item_team_attributions FINAL
    WHERE org_id = {org_id:String}
      AND is_primary = 1
      AND (work_item_id, computed_at) IN (
          SELECT work_item_id, max(computed_at)
          FROM work_item_team_attributions
          WHERE org_id = {org_id:String}
          GROUP BY work_item_id
      )
),
resolved AS (
    SELECT
        windowed.work_unit_id AS work_unit_id,
        multiIf(
            NOT match(evidence_ref, '^[0-9a-fA-F-]{36}#pr[0-9]+$'), evidence_ref,
            evidence_repo.repo = '' OR evidence_repo.provider = '', '',
            concat(if(evidence_repo.provider = 'gitlab', 'gitlab:', 'ghpr:'), evidence_repo.repo,
                if(evidence_repo.provider = 'gitlab', '!', '#'), splitByString('#pr', evidence_ref)[2])
        ) AS resolved_wi_id
    FROM windowed
    ARRAY JOIN arrayDistinct(arrayConcat(
        JSONExtract(structural_evidence_json, 'issues', 'Array(String)'),
        JSONExtract(structural_evidence_json, 'prs', 'Array(String)')
    )) AS evidence_ref
    LEFT JOIN repo_lookup AS evidence_repo
        ON evidence_repo.org_id = windowed.org_id
        AND evidence_repo.repo_uuid = splitByString('#pr', evidence_ref)[1]
),
votes AS (
    SELECT
        work_unit_id,
        argMax(vote_team_id, (cnt, vote_team_id)) AS team_id,
        argMax(vote_team_name, (cnt, vote_team_id)) AS team_name
    FROM (
        SELECT
            resolved.work_unit_id AS work_unit_id,
            ifNull(nullIf(t.team_id, ''), '') AS vote_team_id,
            max(ifNull(nullIf(t.team_name, ''), nullIf(t.team_id, ''))) AS vote_team_name,
            uniqExactIf(resolved.resolved_wi_id, ifNull(nullIf(t.team_name, ''), nullIf(t.team_id, '')) IS NOT NULL) AS cnt
        FROM resolved
        LEFT JOIN wita AS t ON t.work_item_id = resolved.resolved_wi_id
        GROUP BY work_unit_id, vote_team_id
    )
    GROUP BY work_unit_id
),
attributed AS (
    SELECT
        votes.team_id AS team_id,
        votes.team_name AS team_name,
        windowed.effort_value AS effort_value,
        windowed.theme_distribution_json AS theme_distribution_json,
        ifNull(windowed.subcategory_distribution_json[{bugfix_key:String}], 0.0) AS bugfix_share
    FROM votes
    JOIN windowed ON windowed.work_unit_id = votes.work_unit_id
    WHERE votes.team_id != '' AND votes.team_id IN {ids:Array(String)}
)
SELECT team_id, team_name, 'theme' AS kind, theme_kv.1 AS key, sum(theme_kv.2 * effort_value) AS weighted_effort
FROM attributed
ARRAY JOIN CAST(theme_distribution_json AS Array(Tuple(String, Float64))) AS theme_kv
GROUP BY team_id, team_name, key
UNION ALL
SELECT team_id, any(team_name) AS team_name, 'subcategory' AS kind, {bugfix_key:String} AS key, sum(bugfix_share * effort_value) AS weighted_effort
FROM attributed
GROUP BY team_id
)`, DefaultRowLimit)

	extra := append(append([]Binding{}, timeBound.Bindings()...), Binding{Name: "bugfix_key", Value: BugfixSubcategoryKey})
	var rows []TeamThemeMixRow
	err := QueryOrgScopedNamed(ctx, client, "ReadTeamThemeMix", statement, orgID, teamIDs, func(row RowScanner) error {
		var r TeamThemeMixRow
		if scanErr := row.Scan(&r.TeamID, &r.TeamName, &r.Kind, &r.Key, &r.WeightedEffort); scanErr != nil {
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

// rangePredicate bounds a work unit's own validity range
// (`from_ts`/`to_ts`) to the requested window: "this work unit's evidence
// window overlaps the requested [start, end)". Unlike TimeBound's
// DayPredicate/TimestampPredicate (which bound a single row-level column
// against the END of a window, or a range), a work unit is INCLUDED
// whenever any part of its own [from_ts, to_ts) overlaps the requested
// range -- the same overlap test
// `ops/src/dev_health_ops/api/queries/investment.py`'s
// `fetch_investment_breakdown` applies
// (`from_ts < %(end_ts)s AND to_ts >= %(start_ts)s`). An inactive bound
// (the zero TimeBound) applies no predicate at all -- "current", same as
// every other reader in this package.
func (b TimeBound) rangePredicate(fromColumn, toColumn string) string {
	if !b.Active {
		return ""
	}
	predicate := "\n    WHERE " + fromColumn + " < {" + BoundEndParam + ":DateTime64(6,'UTC')}"
	if b.HasStart {
		predicate += "\n      AND " + toColumn + " >= {" + BoundStartParam + ":DateTime64(6,'UTC')}"
	}
	return predicate
}
