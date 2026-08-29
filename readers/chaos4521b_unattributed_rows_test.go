package readers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// CHAOS-4521b, codex on acr #331. Before the project reads keyed on
// work_scope_id, a project row's team came from the ownership join, where
// it could not be NULL and was always part of the ORDER BY. Reading the
// daily row directly changed BOTH properties, and the first version of that
// change carried neither forward.
//
//  1. `estimate_coverage_metrics_daily.team_id` and
//     `capacity_forecasts.team_id` are Nullable. Coalescing to "" reports an
//     UNATTRIBUTED row as a team with an empty id -- counted in team_count,
//     cited as `acr:v1:team:`. Missing is not a team whose name is blank.
//  2. Several teams can contribute rows for one work scope. Without team_id
//     in the ORDER BY the tie is unordered, and the caller preserves that
//     order and caps the first rows -- so identical reads can reorder the
//     public table or truncate DIFFERENT teams.
//
// Neither was observable in the org this was measured against: zero NULL
// team_ids across 1411/47/923 rows, and one team per scope. The schema
// permits both.
func TestChaos4521b_ProjectReadersCarryUnattributedRowsAndOrderTotally(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		sourceTable string
		orderBy     string
		read        func(*fakeClient) error
	}{
		{
			name:        "readiness",
			sourceTable: "FROM estimate_coverage_metrics_daily",
			// (scope, provider, team) is unique per project after rn = 1.
			orderBy: "ORDER BY p.id, ec.work_scope_id, ec.provider, ec.team_key",
			read: func(client *fakeClient) error {
				_, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
				return err
			},
		},
		{
			name:        "workload",
			sourceTable: "FROM capacity_forecasts",
			// capacity_forecasts has no provider column; (scope, team) is
			// unique per project after rn = 1.
			orderBy: "ORDER BY p.id, cf.work_scope_id, cf.team_key",
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
			statement := client.queries[0].statement

			// (1) The NULL is CARRIED, not coalesced away: the flag must be
			// selected beside the id, and reach the caller.
			// The coalesced value MUST be aliased away from team_id.
			// ClickHouse resolves aliases within a SELECT, so
			// `isNotNull(team_id)` beside `ifNull(team_id,'') AS team_id`
			// binds to the alias and reports has_team = 1 for every row --
			// measured on 24.8 before the rename.
			if strings.Contains(statement, "ifNull(team_id, '') AS team_id") {
				t.Errorf("the coalesced value shadows team_id; isNotNull() would bind to the alias and has_team would be 1 for unattributed rows\n%s", statement)
			}
			if !strings.Contains(statement, "toUInt8(isNotNull(team_id)) AS has_team") {
				t.Errorf("the reader does not carry a has_team flag; a NULL team_id would be indistinguishable from an empty one\n%s", statement)
			}
			if !strings.Contains(statement, ".has_team") {
				t.Errorf("has_team is computed but never selected out to the caller\n%s", statement)
			}
			// (2) The ordering is total: the tie-break on team is present.
			if !strings.Contains(statement, testCase.orderBy) {
				t.Errorf("ordering is not total (want %q); tied teams could reorder or truncate differently between identical reads\n%s", testCase.orderBy, statement)
			}
		})
	}
}

// The scanned row must actually expose the distinction, not merely compute
// it in SQL -- a flag the caller cannot read is the same as no flag.
func TestChaos4521b_AnUnattributedRowIsScannedAsUnattributed(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM estimate_coverage_metrics_daily", rows: [][]any{
		// has_team = 0: the source row carried team_id NULL.
		{"linear:proj-1", uint8(0), "", "", "scope-a", "linear", "2026-02-22", int64(3), int64(1), int64(4), uint8(1), float64(0.75)},
		{"linear:proj-1", uint8(1), "team-1", "Team One", "scope-a", "linear", "2026-02-22", int64(18), int64(2), int64(20), uint8(1), float64(0.9)},
	}}}}
	rows, err := readers.ReadProjectReadiness(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadProjectReadiness: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 -- an unattributed row is still a row and must not be dropped", len(rows))
	}
	if rows[0].HasTeam != 0 || rows[0].TeamID != "" {
		t.Errorf("rows[0] = %#v, want an unattributed row (HasTeam 0)", rows[0])
	}
	if rows[1].HasTeam != 1 || rows[1].TeamID != "team-1" {
		t.Errorf("rows[1] = %#v, want the attributed row intact", rows[1])
	}
	// The counts still belong to the project even when the team is unknown:
	// dropping the row would lose real coverage, which is why the flag
	// exists instead of a filter.
	if rows[0].EstimatedCount != 3 || rows[0].BacklogSize != 4 {
		t.Errorf("rows[0] lost its measurements: %#v", rows[0])
	}
}
