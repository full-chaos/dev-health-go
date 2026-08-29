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
//
// CHAOS-4521b: the join moved OFF project_key and onto
// ProjectOwnershipJoinColumn, resolved through the same
// ProjectIdentityJoinSQL/ProjectIdentityMatchSQL pair the work-scope
// readers use.
//
// The old form joined projects.project_key to
// team_project_ownership.project_key, and could not survive either
// direction of the CHAOS-4530 rework:
//
//   - TODAY it reaches no real Linear project at all. Every real Linear
//     project carries project_key NULL, so `project_key != ”` dropped
//     them before the ownership predicate ran; the only non-empty Linear
//     key belongs to the `{org}:linear:<teamKey>` pseudo-project a
//     team-key fallback writes.
//   - AFTER CHAOS-4530 lands -- UUID-keyed ownership rows, the pseudo-
//     project gone, project_key nulled on those rows and real Linear
//     projects' project_key still nil -- a project_key join would match
//     NOTHING, taking health/investment/landscape to zero on deploy.
//
// The two-armed identity match survives both: the id arm picks up the
// UUID-keyed rows the moment they land, and the project_key arm keeps
// matching the key-shaped GitLab rows that exist today (their
// team_project_ownership.project_id holds `full.chaos/dev-health-ops`,
// which IS projects.project_key for that row).
func ProjectOwnershipJoinSQL(ownershipPredicate string) string {
	// The match is THREE-armed, and each arm exists for a shape that really
	// occurs (codex P1 on acr #331 caught the missing third):
	//
	//  1. tpo.<column> = projects.id -- the UUID-keyed rows CHAOS-4530
	//     writes, matched the moment they land.
	//  2. tpo.<column> = projects.project_key -- today's GitLab ownership
	//     rows, whose project_id holds the project KEY while projects.id is
	//     "{org}:gitlab:<numeric>".
	//  3. tpo.project_key = projects.project_key -- the ORIGINAL join, kept.
	//     An ownership row may carry a project_id correlating with nothing
	//     (a legacy or mismatched value) while its project_key is the only
	//     column tying it to a project. Dropping this arm would silently
	//     stop resolving those rows and report a false "no owning teams" --
	//     the exact regression acr's
	//     chaos4347_metrics_widening_integration_test.go was written to
	//     catch, with its deliberately mismatched
	//     "legacy-mismatched-project-id".
	//
	// Keeping arm 3 makes this change strictly ADDITIVE: nothing that
	// resolved before stops resolving, and the UUID rows newly do.
	legacyKeyArm := "(tpo.project_key != '' AND p.project_key != '' AND p.key_resolution_count = 1 AND tpo.project_key = p.project_key)"
	return ProjectIdentityJoinSQL() + `
INNER JOIN (
	SELECT provider, ` + ProjectOwnershipJoinColumn + `, ifNull(project_key, '') AS project_key, team_id
	FROM team_project_ownership FINAL
	WHERE org_id = {org_id:String} AND ` + ProjectOwnershipJoinColumn + ` IS NOT NULL` + ownershipPredicate + `
	GROUP BY provider, ` + ProjectOwnershipJoinColumn + `, project_key, team_id
) AS tpo ON tpo.provider = p.provider AND (` + ProjectIdentityMatchSQL("tpo", ProjectOwnershipJoinColumn) + ` OR ` + legacyKeyArm + `)`
}

// ProjectOwnershipJoinColumn names the team_project_ownership column that
// carries the project's identity.
//
// A single named constant, by design: CHAOS-4530 is reworking the producer,
// and if it renames or moves that column this is the ONE line that changes.
// It is an internal Go string literal, never caller-supplied, so inlining
// it into the statement carries no injection surface -- the same discipline
// WithRowLimit's own limit literal follows.
const ProjectOwnershipJoinColumn = "project_id"

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

// ProjectIdentityJoinSQL resolves each requested project subject to the
// work-scope ids its OWN rows carry, with NO team-ownership hop
// (CHAOS-4521b).
//
// WHY a second project join exists beside ProjectOwnershipJoinSQL, rather
// than replacing it: the two answer different questions, and only some
// source tables can answer the first.
//
//   - A work-scope-keyed table (estimate_coverage_metrics_daily,
//     capacity_forecasts, work_item_metrics_daily) carries the project's
//     OWN rows: `work_scope_id` is work_items.project_id, verified against
//     live data and asserted by dev-health-ops' own oracle
//     (github_work_item_derived_surfaces_oracle_test.go: "same
//     work_scope_id (project_id)"). A project fact reads from those rows
//     directly.
//   - A team-scoped-by-construction table (investment_metrics_daily keyed
//     by repo_id/team_id, compounding_risk_daily keyed by
//     scope='repo'/'team') has no project dimension at all. Reaching a
//     project there REQUIRES the ownership hop, and
//     ProjectOwnershipJoinSQL stays the only way.
//
// The ownership hop was wrong for the first group in two independent ways,
// both observed on live data (org 70d529e0, 2026-08-29):
//
//  1. It could not reach a real project AT ALL. It joins
//     projects.project_key to team_project_ownership.project_key, and every
//     real Linear project carries project_key NULL -- the only non-empty
//     Linear key is the `{org}:linear:<teamKey>` pseudo-project a team-key
//     fallback writes. So the join matched the team pseudo-row and nothing
//     else, and each project rollup returned zero rows (CHAOS-4530).
//  2. When it DID resolve, it returned the wrong rows. The readers joined
//     the daily table on team_id alone and never constrained
//     work_scope_id, so a "project" fact was built from every work scope
//     its owning team touched -- other projects' rows included.
//
// Matching: `work_scope_id` equals either the project's canonical id (the
// Linear shape: a project UUID) or its project_key (the GitLab shape:
// `full.chaos/dev-health-ops`, while projects.id is
// `{org}:gitlab:<numeric>`). Resolving both from `projects` once, here,
// is what keeps this provider-agnostic without a per-provider branch.
//
// The project_key arm keeps ProjectOwnershipJoinSQL's
// key_resolution_count = 1 guard: an ambiguous key must not attribute one
// project's rows to another. The id arm needs no such guard -- projects.id
// is unique by construction.
//
// `provider` is deliberately NOT part of THIS predicate, and the two
// callers differ on it for reasons specific to each (codex P1, adjudicated
// rather than applied wholesale):
//
//   - The work-scope readers do not add it. Cross-provider equal ids are
//     ONE project by design in this data model (Linear imports GitHub), so
//     requiring provider equality would drop legitimate rows rather than
//     prevent a leak -- and capacity_forecasts has no provider column to
//     match on at all.
//   - ProjectOwnershipJoinSQL DOES add `tpo.provider = p.provider`
//     alongside it. The ownership edge already had that equality before
//     CHAOS-4521b, and dropping it would have been an unrequested widening
//     on a join that decides which TEAMS a project inherits. "Equal ids are
//     one project" is a statement about project identity, not a licence to
//     merge two providers' ownership catalogs.
//
// Org scoping is unaffected either way: every subquery here and in every
// caller filters on {org_id:String}.
//
// Selects p.provider and p.id so a caller can rebuild the
// "<provider>:<id>" project subject key, exactly as ProjectOwnershipJoinSQL
// does, and p.project_key / p.key_resolution_count so the caller's own ON
// clause can express the match through ProjectIdentityMatchSQL.
//
// The name is deliberately NOT "work scope": ProjectOwnershipJoinSQL now
// resolves its project the same way, so one subject resolution serves both
// the work-scope-keyed tables and the ownership edge.
func ProjectIdentityJoinSQL() string {
	return `(
	SELECT id, provider, project_key, key_resolution_count
	FROM (
		SELECT id, provider, ifNull(project_key, '') AS project_key,
			count() OVER (PARTITION BY provider, project_key) AS key_resolution_count
		FROM projects FINAL
		WHERE org_id = {org_id:String}
	)
	WHERE concat(provider, ':', id) IN {ids:Array(String)}
) AS p`
}

// ProjectIdentityMatchSQL returns the ON predicate pairing a column that
// carries PROJECT IDENTITY -- a work_scope_id, or
// team_project_ownership's own project column -- with the project subject
// ProjectIdentityJoinSQL resolved.
//
// Two id spaces coexist in those columns and neither is going away, which
// is why the predicate has two arms rather than one:
//
//   - the canonical id (`p.id`): a Linear project UUID, and the space
//     CHAOS-4530's reworked ownership rows key on;
//   - the project key (`p.project_key`): the GitLab shape, where
//     work_scope_id and team_project_ownership.project_id both hold
//     `full.chaos/dev-health-ops` while projects.id is
//     `{org}:gitlab:<numeric>`.
//
// Keeping both arms is what makes the ownership join survive CHAOS-4530
// in BOTH directions: it matches the UUID-keyed rows the moment they land,
// and it keeps matching the key-shaped GitLab rows that exist today. A
// single-arm join on either space alone would take a provider to zero.
//
// alias and column are internal Go string literals at every call site,
// never caller-supplied text, so they carry no injection surface -- the
// same discipline WithRowLimit's own limit literal follows.
//
// The empty-key test is what stops a project whose project_key is NULL
// (every real Linear project) from matching a daily row whose
// work_scope_id happens to be the empty string.
func ProjectIdentityMatchSQL(alias, column string) string {
	qualified := alias + "." + column
	return "(" + qualified + " = p.id OR (p.project_key != '' AND p.key_resolution_count = 1 AND " + qualified + " = p.project_key))"
}
