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
	// TWO equality-joined arms, UNION ALL'd, then collapsed to the resolved
	// grain. Every JOIN ON here is a plain column equality -- see
	// ProjectIdentityJoinSQL for why ClickHouse 24.8 makes that mandatory.
	//
	//  A. o.project_id = p.scope        -- covers BOTH id spaces at once,
	//     because p.scope already carries the canonical id AND the project
	//     key as separate rows: CHAOS-4530's UUID-keyed rows match the id
	//     row, today's GitLab rows match the key row.
	//  B. o.project_key = p.project_key -- the ORIGINAL join, kept. An
	//     ownership row may carry a project_id correlating with nothing
	//     while its project_key is the only column tying it to a project;
	//     dropping this arm reported a false "no owning teams" for exactly
	//     the shape acr's chaos4347 fixture seeds on purpose.
	//
	// The outer GROUP BY is what makes the arms safe to union. A team can
	// match through both at once (during the CHAOS-4530 transition it holds
	// a legacy AND a UUID-keyed row), and it can hold several ownership
	// rows per project because `source` and `valid_from` are in
	// team_project_ownership's sorting key. Either would duplicate the team
	// -- and a duplicate consumes DefaultRowLimit before the caller's
	// MarkSeen dedup runs, silently truncating OTHER teams out of the
	// answer. Collapsing at the grain the caller consumes, (provider,
	// resolved project id, team), removes both at once.
	projects := ProjectIdentityJoinSQL()
	ownershipByID := `(
		SELECT provider, ` + ProjectOwnershipJoinColumn + `, team_id
		FROM team_project_ownership FINAL
		WHERE org_id = {org_id:String} AND ` + ProjectOwnershipJoinColumn + ` IS NOT NULL` + ownershipPredicate + `
		GROUP BY provider, ` + ProjectOwnershipJoinColumn + `, team_id
	) AS o`
	ownershipByKey := `(
		SELECT provider, ifNull(project_key, '') AS project_key, team_id
		FROM team_project_ownership FINAL
		WHERE org_id = {org_id:String} AND project_key IS NOT NULL` + ownershipPredicate + `
		GROUP BY provider, project_key, team_id
	) AS o`
	return `(
	SELECT provider, id, team_id
	FROM (
		SELECT p.provider AS provider, p.id AS id, o.team_id AS team_id
		FROM ` + projects + `
		INNER JOIN ` + ownershipByID + ` ON o.provider = p.provider AND ` + ProjectIdentityMatchSQL("o", ProjectOwnershipJoinColumn) + `

		UNION ALL

		SELECT p.provider AS provider, p.id AS id, o.team_id AS team_id
		FROM ` + projects + `
		INNER JOIN ` + ownershipByKey + ` ON o.provider = p.provider AND o.project_key = p.project_key
		WHERE p.project_key != '' AND p.key_resolution_count = 1
	)
	GROUP BY provider, id, team_id
) AS p`
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
// identity values it answers to, ONE ROW PER VALUE, so a caller can join a
// project-identity column on a plain equality against `p.scope`.
//
// WHY rows instead of an OR in the caller's ON clause (CHAOS-4521b,
// executed): ClickHouse 24.8 -- which acr's fixtures pin deliberately,
// asserting the server version prefix "24.8." -- rejects a JOIN ON
// containing OR or a function call, under BOTH analyzer settings:
//
//	Code: 403. DB::Exception: Unsupported JOIN ON conditions.
//
// Prod runs 26.7 with the new analyzer, where the OR form is valid, which
// is why it passed every local proof and only CI's pinned fixtures caught
// it. Expanding the alternatives into rows and joining on equality is
// portable across both engines and needs no analyzer setting.
//
// The two identity values, and why each exists:
//
//   - the canonical id: a Linear project UUID, and the space CHAOS-4530's
//     ownership rows key on;
//   - the project key: the GitLab shape, where work_scope_id and
//     team_project_ownership.project_id both hold
//     `full.chaos/dev-health-ops` while projects.id is
//     `{org}:gitlab:<numeric>`.
//
// The key row is emitted only for a NON-EMPTY, UNAMBIGUOUS key: every real
// Linear project carries project_key NULL (coalesced to ”), which must
// never match a stray empty identity, and an ambiguous key must never
// attribute one project's rows to another.
//
// The GROUP BY collapses id == project_key to a single scope row, so a
// caller cannot double-count a project whose two identity values coincide.
//
// `provider` is carried but deliberately NOT compared by
// ProjectIdentityMatchSQL -- see that function.
func ProjectIdentityJoinSQL() string {
	return projectIdentityExpansionSQL(projectIdentityRowsSQL)
}

// ProjectIdentityCatalogSQL is ProjectIdentityJoinSQL for a caller that
// walks the WHOLE catalog rather than resolving a requested subject list
// (CHAOS-4542).
//
// The distinction is not cosmetic. ProjectIdentityJoinSQL's row source ends
// `WHERE concat(provider, ':', id) IN {ids:Array(String)}`, so a caller
// that binds no `ids` parameter gets
//
//	Code: 456. DB::Exception: Substitution `ids` is not set.
//
// which is exactly how devhealthsource's queryProjectTeams failed when it
// first tried to reuse the filtered form: it is a catalog producer paginating
// by cursor, and has no subject list to bind. A design mismatch, not a syntax
// error -- and one worth naming here, because "reuse the identity helper" is
// the obvious instinct and it is wrong for that caller.
//
// Both forms are built from projectIdentityExpansionSQL over the same row
// builder, so the two scope rows and their guards have ONE definition: the
// filtered form is the catalog form plus the `ids` predicate, and neither
// can drift in what an identity value means.
func ProjectIdentityCatalogSQL() string {
	return projectIdentityExpansionSQL(projectIdentityCatalogRowsSQL)
}

// projectIdentityExpansionSQL turns a project row source into one row per
// (project, identity value it answers to) -- see ProjectIdentityJoinSQL for
// why the alternatives are rows rather than an OR in the caller's ON.
func projectIdentityExpansionSQL(rows string) string {
	// key_resolution_count is emitted PER SCOPE ROW, not per project
	// (CHAOS-4542). The two rows answer different questions and conflating
	// them cost a whole review round:
	//
	//   - the ID row is unambiguous BY CONSTRUCTION -- projects.id is
	//     unique -- so its count is always 1;
	//   - the KEY row carries the key partition's count, which is what the
	//     ambiguity guard is actually about.
	//
	// Emitting the project-level number on both looked harmless and was
	// not. Every real Linear project has project_key NULL, they therefore
	// all share the empty-key partition, and the count on their ID rows
	// came back as "however many NULL-key projects this org has" -- 17 on
	// the org this was measured against. A consumer that gates on
	// key_resolution_count > 1 then discards a perfectly unambiguous
	// project_id = projects.id match. devhealthsource's queryProjectTeams
	// did exactly that and still emitted zero Linear edges after being
	// "fixed"; no fixture caught it, because they all seed projects WITH
	// keys.
	//
	// A project-level number that reads like a per-match one is a footgun,
	// so it is removed rather than documented.
	return `(
	SELECT provider, id, project_key, key_resolution_count, scope
	FROM (
		SELECT provider, id, project_key, toUInt64(1) AS key_resolution_count, id AS scope
		FROM (` + rows + `)

		UNION ALL

		SELECT provider, id, project_key, key_resolution_count, project_key AS scope
		FROM (` + rows + `)
		WHERE project_key != '' AND key_resolution_count = 1
	)
	GROUP BY provider, id, project_key, key_resolution_count, scope
) AS p`
}

// projectIdentityRowsSQL resolves the REQUESTED project subjects out of
// `projects`, carrying the ambiguity count the key row is guarded on. It is
// the single place `projects` is read, so the two identity rows above
// cannot drift in what they resolve.
const projectIdentityRowsSQL = projectIdentityCatalogRowsSQL + `
		WHERE concat(provider, ':', id) IN {ids:Array(String)}`

// projectIdentityCatalogRowsSQL is every project in the organization, with
// the ambiguity count the key row is guarded on.
//
// The window runs over the WHOLE org and is not narrowed by the subject
// filter above -- that ordering is load-bearing in both forms:
// key_resolution_count must count across every project sharing a
// (provider, project_key), not just the requested ones, or an ambiguous key
// stops being detected and the guard it feeds becomes decorative.
const projectIdentityCatalogRowsSQL = `
		SELECT id, provider, project_key, key_resolution_count
		FROM (
			SELECT id, provider, ifNull(project_key, '') AS project_key,
				countIf(ifNull(project_key, '') != '') OVER (PARTITION BY provider, ifNull(project_key, '')) AS key_resolution_count
			FROM projects FINAL
			WHERE org_id = {org_id:String}
		)`

// ProjectIdentityMatchSQL returns the ON predicate pairing a column that
// carries PROJECT IDENTITY -- a work_scope_id, or team_project_ownership's
// own project column -- with the subject ProjectIdentityJoinSQL resolved.
//
// A plain column equality by construction: the alternatives live in
// ProjectIdentityJoinSQL's rows, not in this predicate, because 24.8
// rejects an ON containing OR or a function call outright.
//
// `provider` is not compared here. Cross-provider equal ids are ONE project
// by design in this data model (Linear imports GitHub), so requiring
// provider equality on a work-scope read would DROP legitimate rows rather
// than prevent a leak -- and capacity_forecasts has no provider column at
// all. ProjectOwnershipJoinSQL adds `o.provider = p.provider` itself,
// because the ownership edge decides which TEAMS a project inherits and
// already had that equality before CHAOS-4521b.
//
// alias and column are internal Go string literals at every call site,
// never caller-supplied, so inlining them carries no injection surface.
func ProjectIdentityMatchSQL(alias, column string) string {
	return alias + "." + column + " = p.scope"
}
