package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadProjectThemeMix(t *testing.T) {
	t.Parallel()

	t.Run("scans one row per project", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: [][]any{
			{"linear:proj-1", 1.0, 2.0, 3.0, 4.0, 5.0, 0.5, uint64(9), uint64(7), uint64(2)},
		}}}}
		rows, err := readers.ReadProjectThemeMix(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectThemeMix() error = %v", err)
		}
		want := readers.ProjectThemeMixRow{ProjectSubjectKey: "linear:proj-1", FeatureDelivery: 1, Operational: 2, Maintenance: 3, Quality: 4, Risk: 5, BugfixWeighted: 0.5, WorkUnits: 9, EffortUnits: 7, SpanningUnits: 2}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
		if client.orgIDBinding() != "org-1" {
			t.Fatalf("org_id binding = %q", client.orgIDBinding())
		}
	})

	t.Run("empty ids short-circuits without a query", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		rows, err := readers.ReadProjectThemeMix(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil || len(client.queries) != 0 {
			t.Fatalf("rows = %#v, err = %v, queries = %d", rows, err, len(client.queries))
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", err: boom}}}
		rows, err := readers.ReadProjectThemeMix(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != boom || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})

	t.Run("statement reads persisted distributions and project membership, never team attribution", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		if _, err := readers.ReadProjectThemeMix(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadProjectThemeMix() error = %v", err)
		}
		statement := client.queries[0].statement
		for _, need := range []string{"theme_distribution_json", "project_membership_presence", "subject_kind = 'work_item'", "{org_id:String}"} {
			if !strings.Contains(statement, need) {
				t.Fatalf("statement is missing %q: %q", need, statement)
			}
		}
		if strings.Contains(statement, "work_item_team_attributions") || strings.Contains(statement, "investment_metrics_daily") {
			t.Fatalf("statement reads a team attribution or legacy investment source: %q", statement)
		}
	})

	t.Run("row limit is the caller's", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		if _, err := readers.ReadProjectThemeMixWithRowLimit(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{}, readers.ProbeRowLimit); err != nil {
			t.Fatalf("error = %v", err)
		}
		if !strings.HasSuffix(client.queries[0].statement, "LIMIT 201") {
			t.Fatalf("statement = %q, want LIMIT 201", client.queries[0].statement)
		}
	})

	t.Run("active time bound reaches the statement and bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		end := mustTime(t, "2026-08-28T00:00:00Z")
		start := mustTime(t, "2026-05-30T00:00:00Z")
		if _, err := readers.ReadProjectThemeMix(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{Active: true, HasStart: true, Start: start, End: end}); err != nil {
			t.Fatalf("error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "{"+readers.BoundEndParam+":DateTime64(6,'UTC')}") || !strings.Contains(statement, "{"+readers.BoundStartParam+":DateTime64(6,'UTC')}") {
			t.Fatalf("statement = %q, want both window bindings", statement)
		}
	})
}
