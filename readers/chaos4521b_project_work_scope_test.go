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
		read        func(*fakeClient) error
	}{
		{
			name:        "readiness",
			sourceTable: "FROM estimate_coverage_metrics_daily",
			read: func(client *fakeClient) error {
				_, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
				return err
			},
		},
		{
			name:        "workload",
			sourceTable: "FROM capacity_forecasts",
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
			// (4) Org scoping survives the rewrite.
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
