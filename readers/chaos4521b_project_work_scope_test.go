package readers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// CHAOS-4521b. The project readiness and workload readers used to reach a
// project through team_project_ownership, and that was wrong twice over --
// both observed on live data (org 70d529e0, 2026-08-29):
//
//  1. It could not reach a real project AT ALL. The join keys on
//     projects.project_key, and every real Linear project carries
//     project_key NULL; the only non-empty Linear key belongs to the
//     `{org}:linear:<teamKey>` pseudo-project a team-key fallback writes.
//     Every project rollup therefore returned zero rows (CHAOS-4530).
//  2. When it DID resolve, it returned the WRONG rows. The daily table was
//     joined on team_id alone with work_scope_id left unconstrained, so a
//     "project" fact was assembled from every work scope its owning team
//     touched -- other projects' rows included. That defect is invisible
//     to a fake-client row assertion, because the fake returns whatever it
//     is handed regardless of the SQL; only the statement text shows it.
//
// So this asserts the STATEMENT, the same discipline acr's
// assertQueryScopedToOrgAndSubjects uses for org/subject scoping: a reader
// can keep returning the rows a fake hands it long after the query stopped
// selecting the right ones.
func TestChaos4521b_ProjectReadersKeyOnTheProjectsOwnWorkScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		sourceTable string
		rnPartition string
		read        func(*fakeClient) error
	}{
		{
			name:        "readiness",
			sourceTable: "FROM estimate_coverage_metrics_daily",
			rnPartition: "PARTITION BY team_id, work_scope_id, provider",
			read: func(client *fakeClient) error {
				_, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
				return err
			},
		},
		{
			name:        "workload",
			sourceTable: "FROM capacity_forecasts",
			rnPartition: "PARTITION BY team_id, work_scope_id",
			read: func(client *fakeClient) error {
				_, err := readers.ReadProjectWorkload(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
				return err
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{tables: []fakeTable{{match: testCase.sourceTable, rows: nil}}}
			if err := testCase.read(client); err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(client.queries) != 1 {
				t.Fatalf("queries = %d, want exactly one", len(client.queries))
			}
			statement := client.queries[0].statement

			// (1) No ownership hop at all -- not a narrowed one.
			if strings.Contains(statement, "team_project_ownership") {
				t.Errorf("statement still joins team_project_ownership; a project's own work-scope rows need no ownership hop\n%s", statement)
			}
			// (2) The work scope is CONSTRAINED to the requested project,
			// by its canonical id (the Linear shape) or its project_key
			// (the GitLab shape). Selecting work_scope_id without
			// constraining it is the defect in 2 above.
			// CHAOS-4521b re-plan: the alternatives moved OUT of the ON
			// clause and into ProjectIdentityJoinSQL's rows, because 24.8
			// rejects an ON containing OR. The join is now a plain equality
			// against p.scope, and the two identity values are the two
			// unioned scope rows.
			if !strings.Contains(statement, "work_scope_id = p.scope") {
				t.Errorf("statement does not match work_scope_id against the resolved identity scope\n%s", statement)
			}
			// RESPELLED by CHAOS-4751, not relaxed: the two scope rows come
			// from one fan-out array now rather than two UNION branches, so
			// the pair of substrings became one literal naming both.
			if !strings.Contains(statement, "if(key_scope_emitted, [id, project_key], [id]) AS scope") {
				t.Errorf("the identity resolution does not expand BOTH the canonical id and the project key into scope rows\n%s", statement)
			}
			// (3) The project_key arm keeps the ambiguity guard: an
			// ambiguous key must never attribute one project's rows to
			// another. The id arm needs none -- projects.id is unique.
			if !strings.Contains(statement, "key_resolution_count = 1") {
				t.Errorf("statement drops the ambiguous-project_key guard\n%s", statement)
			}
			// (4) codex P1: team_id stays in the row_number partition.
			// An earlier revision dropped it, and the org this was measured
			// against has ONE team -- so row_number() keeping a single
			// team's row per work scope, and silently dropping every other
			// contributing team, was invisible in the data and would have
			// shipped. team_id is part of the source table's natural key
			// AND of the row shape these readers return.
			if !strings.Contains(statement, testCase.rnPartition) {
				t.Errorf("row_number partition is not %q; dropping team_id silently keeps one team per work scope\n%s", testCase.rnPartition, statement)
			}
			// (5) Org scoping survives the rewrite.
			if !strings.Contains(statement, "org_id = {org_id:String}") {
				t.Errorf("statement lost its org scoping\n%s", statement)
			}
		})
	}
}

// The team-scoped readers must NOT be dragged along: their source tables
// carry no project dimension, so the ownership hop is the only way to a
// project there and removing it would be a silent capability loss.
// ProjectOwnershipJoinSQL therefore has to stay, and stay used.
func TestChaos4521b_TeamScopedProjectRollupsKeepTheOwnershipHop(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM investment_metrics_daily", rows: nil}}}
	if _, err := readers.ReadProjectInvestment(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{}); err != nil {
		t.Fatalf("ReadProjectInvestment: %v", err)
	}
	if len(client.queries) != 1 {
		t.Fatalf("queries = %d, want exactly one", len(client.queries))
	}
	if !strings.Contains(client.queries[0].statement, "team_project_ownership") {
		t.Errorf("investment_metrics_daily is keyed by repo_id/team_id and has no project dimension; its project rollup must keep the ownership hop")
	}
}

// CHAOS-4521b addendum. The ownership join itself had to move off
// project_key, because it could not survive EITHER direction of the
// CHAOS-4530 producer rework:
//
//   - today it reaches no real Linear project (their project_key is NULL,
//     and `project_key != ”` dropped them before the ownership predicate
//     ran);
//   - after 4530 lands -- UUID-keyed ownership rows, the team-key
//     pseudo-project gone, project_key nulled on those rows -- a
//     project_key join would match NOTHING, taking health, investment and
//     landscape to zero on deploy.
//
// So the ownership edge is matched by the same two-armed project identity
// the work-scope readers use, through one named column constant.
func TestChaos4521b_TheOwnershipJoinKeysOnProjectIdentityNotProjectKey(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM investment_metrics_daily", rows: nil}}}
	if _, err := readers.ReadProjectInvestment(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{}); err != nil {
		t.Fatalf("ReadProjectInvestment: %v", err)
	}
	statement := client.queries[0].statement

	// The ownership edge is still joined -- investment_metrics_daily has no
	// project dimension, so removing the hop would be a capability loss.
	if !strings.Contains(statement, "team_project_ownership") {
		t.Fatalf("the team-scoped rollup must keep the ownership hop\n%s", statement)
	}
	// ...and it now ALSO keys on the project identity column, which is the
	// arm CHAOS-4530's UUID-keyed rows need.
	//
	// The column is spelled as a LITERAL here, not via
	// readers.ProjectOwnershipJoinColumn, so this test still COMPILES at
	// the parent commit and fails there on the assertion rather than on a
	// missing symbol -- a build error is not a behavioural red. The
	// constant is coupled to the SQL by its own test below.
	//
	// RESPELLED by CHAOS-4552 (union-once), not relaxed: both arms now
	// share ONE join predicate (`o.scope_value = p.scope`), and the
	// id-sourced rows project their identity column into that shared
	// `scope_value` alias rather than joining on their own column name
	// directly.
	if !strings.Contains(statement, "project_id AS scope_value") {
		t.Errorf("ownership join does not project project_id into the shared scope_value column\n%s", statement)
	}
	if !strings.Contains(statement, "o.scope_value = p.scope") {
		t.Errorf("ownership join does not key on o.scope_value = p.scope\n%s", statement)
	}
	// CHAOS-4521b: every JOIN ON must be a plain column equality. A single
	// ON carrying the arms as an OR -- what v0.5.0 shipped -- is rejected
	// by ClickHouse's OLD analyzer with "Code: 403 Unsupported JOIN ON
	// conditions", and acr's fixtures pin 24.8, where that analyzer is the
	// default. Prod runs 26.7 with the new one, so the OR form passed every
	// local proof and only CI caught it.
	for _, onClause := range strings.Split(statement, "ON ")[1:] {
		condition := strings.SplitN(onClause, "\n", 2)[0]
		if strings.Contains(condition, " OR ") || strings.Contains(condition, "has(") {
			t.Errorf("a JOIN ON condition is not a plain equality (%q); the old analyzer rejects it\n%s", condition, statement)
		}
	}
	if !strings.Contains(statement, "UNION ALL") {
		t.Errorf("the arms are not expressed as unioned equality joins\n%s", statement)
	}
	// The key-to-key arm STAYS (codex P1 on acr #331). Removing it looked
	// like the point of this change and was not: an ownership row can carry
	// a project_id correlating with nothing while its project_key is the
	// only column tying it to a project. Dropping it would report a false
	// "no owning teams" for exactly that shape, which acr's
	// chaos4347_metrics_widening_integration_test.go seeds on purpose
	// ("legacy-mismatched-project-id"). Three arms, all load-bearing.
	// Executed on a real ClickHouse with the chaos4347 shape as literals
	// (projects.id='proj-1-internal-id' / project_key='PROJ1' against
	// team_project_ownership.project_id='legacy-mismatched-project-id' /
	// project_key='PROJ1'): the pre-4521b key-to-key join matched 1 row,
	// v0.5.0's two-armed join matched 0, and the three-armed join matches 1
	// again. That 1 -> 0 -> 1 is the regression and its repair.
	//
	// CHAOS-4542 defect 6 RESPELLED this arm without removing it. It used to
	// read `o.project_key = p.project_key`, guarded by
	// `p.key_resolution_count = 1`. Both halves were wrong together:
	// p.project_key is carried by EVERY scope row, and after v0.5.4 an id
	// row's count is 1 by construction, so an id row satisfied a guard
	// written to mean "this key names exactly one project" and two projects
	// sharing a key both matched an ownership row that named neither.
	//
	// The arm now matches the KEY SCOPE ROW itself. An ambiguous key has no
	// such row -- the filter moved inside the expansion -- so the arm cannot
	// resolve one, and no consumer has to remember a guard. This assertion
	// still exists for its original purpose (the arm has been dropped three
	// separate times), only against the spelling that is now load-bearing.
	//
	// RESPELLED AGAIN by CHAOS-4552: the key arm no longer joins on its own
	// column (`o.project_key = p.scope`) or restricts with a standalone
	// `p.scope_kind = 'key'` WHERE -- it projects project_key into the
	// SAME shared scope_value column the id arm uses, tagged
	// `required_scope_kind = 'key'`, enforced by the shared WHERE OR. Both
	// halves must survive: the tag naming 'key', and the join actually
	// requiring it.
	if !strings.Contains(statement, "ifNull(project_key, '') AS scope_value") || !strings.Contains(statement, "'key' AS required_scope_kind") {
		t.Errorf("ownership join dropped the legacy key-to-key arm; an ownership row whose project_id correlates with nothing would stop resolving\n%s", statement)
	}
	if !strings.Contains(statement, "o.required_scope_kind = '' OR p.scope_kind = o.required_scope_kind") {
		t.Errorf("the key arm's scope_kind restriction is not enforced; an ownership row's project_key could cross-match another project's id scope row\n%s", statement)
	}
	// codex P1: the grain must stay one row per (provider, project_id,
	// team). team_project_ownership's sorting key carries `source` and
	// `valid_from`, so FINAL keeps several rows per (project_id, team) and
	// project_key -- which is NOT in that key -- can differ between them.
	// Grouping by project_key would split a group that previously
	// collapsed, duplicating the team and burning DefaultRowLimit before
	// the caller's dedup runs.
	if strings.Contains(statement, "GROUP BY provider, project_id, project_key, team_id") {
		t.Errorf("project_key is back in the GROUP BY; multi-source ownership rows would duplicate the team\n%s", statement)
	}
	// codex R2: the whole join collapses to the RESOLVED grain. Deduping
	// inside the ownership subquery is not enough -- during the 4530
	// transition a team can hold a legacy row (matching through arm 3) AND
	// a UUID row (arm 1) for one project, which are different groups by
	// construction and both match. Duplicates then consume DefaultRowLimit
	// before the caller's MarkSeen dedup runs, truncating OTHER teams out
	// of the answer.
	if !strings.Contains(statement, "GROUP BY provider, id, team_id") {
		t.Errorf("ownership join is not collapsed to the resolved (provider, project id, team) grain\n%s", statement)
	}
	// codex P1: the ownership edge keeps its provider equality. "Equal ids
	// are one project" is a statement about project identity, not a licence
	// to merge two providers' ownership catalogs -- and this join decides
	// which TEAMS a project inherits.
	if !strings.Contains(statement, "o.provider = p.provider") {
		t.Errorf("ownership join dropped provider equality; a project must not inherit another provider's teams\n%s", statement)
	}
	// The GitLab arm survives: those ownership rows carry the project KEY
	// in the identity column while projects.id is `{org}:gitlab:<numeric>`.
	// Dropping this arm would take GitLab to zero the other way.
	// The GitLab arm survives as a scope ROW rather than an ON alternative
	// -- since CHAOS-4751, as the key position of the fan-out array rather
	// than its own UNION branch. Same row, same guard, one read.
	if !strings.Contains(statement, "if(key_scope_emitted, [id, project_key], [id]) AS scope") {
		t.Errorf("ownership join dropped the project_key identity row; GitLab ownership rows key on it today\n%s", statement)
	}
	// A project whose project_key is NULL (every real Linear project) must
	// no longer be filtered out before the join is even evaluated.
	if strings.Contains(statement, "WHERE project_key != '' AND key_resolution_count = 1 AND concat(provider") {
		t.Errorf("subject resolution still drops projects with a NULL project_key\n%s", statement)
	}
}

// ProjectOwnershipJoinColumn exists so that CHAOS-4530 renaming or moving
// team_project_ownership's project column is a ONE-line change here. That
// promise is only true while the constant is actually what the SQL emits,
// which is what this couples -- the literals in the test above are there to
// keep that test compiling at the parent commit, and would otherwise be a
// second, drifting copy of the same fact.
func TestChaos4521b_TheOwnershipJoinColumnConstantIsWhatTheSQLUses(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM investment_metrics_daily", rows: nil}}}
	if _, err := readers.ReadProjectInvestment(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{}); err != nil {
		t.Fatalf("ReadProjectInvestment: %v", err)
	}
	statement := client.queries[0].statement
	for _, fragment := range []string{
		"SELECT provider, " + readers.ProjectOwnershipJoinColumn + " AS scope_value, team_id",
		"AND " + readers.ProjectOwnershipJoinColumn + " IS NOT NULL",
		"GROUP BY provider, " + readers.ProjectOwnershipJoinColumn + ", team_id",
	} {
		if !strings.Contains(statement, fragment) {
			t.Errorf("statement does not use ProjectOwnershipJoinColumn at %q; the one-line-change promise is broken\n%s", fragment, statement)
		}
	}
}
