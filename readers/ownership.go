package readers

// Shared helpers for the project-rollup readers (metrics.go, investment.go,
// workload.go, readiness.go): every one of those rolls a project up through
// projects -> team_project_ownership -> a team-scoped daily table, using the
// SAME join shape and the SAME slowly-changing-dimension validity predicate.
// A single declaration here is what stops the join or the predicate drifting
// between the four readers the way acr's own devhealthfacts/shared.go
// comment warns about for its callers.
//
// Extracted from acr's internal/contextfabric/devhealthfacts/shared.go
// (CHAOS-4377): projectOwnershipJoinSQL, ownershipValidityPredicate, and
// dedupeTeamRow. Callers there passed acr's own factTimeBound; here they
// take this package's TimeBound instead.

// OwnershipValidityPredicate returns the valid_from/valid_to predicate a
// slowly-changing ownership edge (team_project_ownership, team_repo_ownership
// -- both carry the same valid_from/valid_to(DateTime64) shape) must satisfy
// for the requested time context: "currently active" on the current axis,
// "active AT THE END of the requested window" for a bounded historical
// query -- the same convention TimeBound.AsOfExpression documents for every
// other derived-state read. now64(3) is a literal ClickHouse function call,
// never caller-supplied text, so it carries no injection surface.
func OwnershipValidityPredicate(bound TimeBound) string {
	if bound.Active {
		return " AND valid_from <= {" + BoundEndParam + ":DateTime64(6,'UTC')} AND (valid_to IS NULL OR valid_to > {" + BoundEndParam + ":DateTime64(6,'UTC')})"
	}
	return " AND valid_from <= now64(3) AND valid_to IS NULL"
}

// ProjectOwnershipJoinSQL returns the SQL join fragment resolving every
// requested project subject (matched by "<provider>:<id>") to its owning
// teams via projects -> team_project_ownership.
//
// The join is NOT team_project_ownership.project_id -- that column is not
// projects.id for every provider (for gitlab rows it holds the project KEY).
// A join on project_id would silently drop exactly the ownership edges a
// project rollup exists to surface. This resolves the requested project's
// project_key from `projects` FIRST (canonical id still comes from
// `projects`), then joins team_project_ownership on (provider, project_key).
//
// Selects p.provider, p.id (so a caller can rebuild the "<provider>:<id>"
// project subject key) and tpo.team_id (one row per currently- or
// as-of-owning team; a team owning the project through more than one
// `source` row still yields one row per source and must be deduped by the
// caller with MarkSeen the same way ReadProjectMetrics does).
func ProjectOwnershipJoinSQL(ownershipPredicate string) string {
	return `(
	SELECT id, provider, project_key
	FROM (
		SELECT id, provider, ifNull(project_key, '') AS project_key,
			count() OVER (PARTITION BY provider, project_key) AS key_resolution_count
		FROM projects FINAL
		WHERE org_id = {org_id:String}
	)
	WHERE project_key != '' AND key_resolution_count = 1 AND concat(provider, ':', id) IN {ids:Array(String)}
) AS p
INNER JOIN (
	SELECT provider, project_key, team_id
	FROM team_project_ownership FINAL
	WHERE org_id = {org_id:String} AND project_key IS NOT NULL` + ownershipPredicate + `
	GROUP BY provider, project_key, team_id
) AS tpo ON tpo.provider = p.provider AND tpo.project_key = p.project_key`
}

// MarkSeen reports whether key has already been seen in seen, recording it
// if not. team_project_ownership's own ORDER BY key includes `source`, so
// the SAME team can legitimately appear more than once for one project (a
// native AND a manual ownership edge both current at once); every
// project-rollup reader must dedupe by team id (or a (team, scope) tuple)
// before aggregating, or a team owning a project through two sources would
// be double-counted.
func MarkSeen(seen map[string]bool, key string) bool {
	if seen[key] {
		return true
	}
	seen[key] = true
	return false
}

// RepresentableInt64 converts an unsigned source value, reporting false
// when it cannot be represented as a signed int64. Two ClickHouse columns
// this package's readers scan (backfill_log.duration_ms,
// investment_metrics_daily.churn_loc) are UInt64; wrapping them with
// ClickHouse's own toInt64() silently turns a value above MaxInt64
// negative, which is a wrong value, not a failed read (CHAOS-3781 round-3
// F2). The caller must scan the raw uint64, check representability here,
// and omit the row rather than report it wrong.
func RepresentableInt64(value uint64) (int64, bool) {
	const maxInt64 = 1<<63 - 1
	if value > maxInt64 {
		return 0, false
	}
	return int64(value), true
}
