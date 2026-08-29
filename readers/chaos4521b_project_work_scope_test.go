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
			if !strings.Contains(statement, "work_scope_id = p.id") {
				t.Errorf("statement does not match work_scope_id against the project id\n%s", statement)
			}
			if !strings.Contains(statement, "work_scope_id = p.project_key") {
				t.Errorf("statement does not match work_scope_id against the project key (the GitLab shape)\n%s", statement)
			}
			// (3) The project_key arm keeps the ambiguity guard: an
			// ambiguous key must never attribute one project's rows to
			// another. The id arm needs none -- projects.id is unique.
			if !strings.Contains(statement, "p.key_resolution_count = 1") {
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
	// ...but it is keyed on the project identity column, not project_key.
	//
	// The column is spelled as a LITERAL here, not via
	// readers.ProjectOwnershipJoinColumn, so this test still COMPILES at
	// the parent commit and fails there on the assertion rather than on a
	// missing symbol -- a build error is not a behavioural red. The
	// constant is coupled to the SQL by its own test below.
	if !strings.Contains(statement, "tpo.project_id = p.id") {
		t.Errorf("ownership join does not key on tpo.project_id = p.id\n%s", statement)
	}
	if strings.Contains(statement, "tpo.project_key = p.project_key") {
		t.Errorf("ownership join still keys on project_key, which reaches no real Linear project today and matches nothing after CHAOS-4530\n%s", statement)
	}
	// codex P1: the ownership edge keeps its provider equality. "Equal ids
	// are one project" is a statement about project identity, not a licence
	// to merge two providers' ownership catalogs -- and this join decides
	// which TEAMS a project inherits.
	if !strings.Contains(statement, "tpo.provider = p.provider") {
		t.Errorf("ownership join dropped provider equality; a project must not inherit another provider's teams\n%s", statement)
	}
	// The GitLab arm survives: those ownership rows carry the project KEY
	// in the identity column while projects.id is `{org}:gitlab:<numeric>`.
	// Dropping this arm would take GitLab to zero the other way.
	if !strings.Contains(statement, "tpo.project_id = p.project_key") {
		t.Errorf("ownership join dropped the project_key arm; GitLab ownership rows key on it today\n%s", statement)
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
		"SELECT provider, " + readers.ProjectOwnershipJoinColumn + ", team_id",
		"AND " + readers.ProjectOwnershipJoinColumn + " IS NOT NULL",
		"GROUP BY provider, " + readers.ProjectOwnershipJoinColumn + ", team_id",
		"tpo." + readers.ProjectOwnershipJoinColumn + " = p.id",
	} {
		if !strings.Contains(statement, fragment) {
			t.Errorf("statement does not use ProjectOwnershipJoinColumn at %q; the one-line-change promise is broken\n%s", fragment, statement)
		}
	}
}
