package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadTeamThemeMix(t *testing.T) {
	t.Parallel()

	t.Run("happy path scans theme and subcategory rows", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: [][]any{
			{"CHAOS", "Fullchaos", "theme", "feature_delivery", float64(9607083.35)},
			{"CHAOS", "Fullchaos", "theme", "operational", float64(790605.22)},
			{"CHAOS", "Fullchaos", "subcategory", readers.BugfixSubcategoryKey, float64(1379891.25)},
		}}}}
		rows, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamThemeMix() error = %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("rows = %#v, want 3", rows)
		}
		if rows[0].Kind != "theme" || rows[0].Key != "feature_delivery" {
			t.Fatalf("rows[0] = %#v", rows[0])
		}
		if rows[2].Kind != "subcategory" || rows[2].Key != readers.BugfixSubcategoryKey {
			t.Fatalf("rows[2] = %#v, want the tracked bugfix subcategory", rows[2])
		}
		if client.orgIDBinding() != "org-1" {
			t.Fatalf("org_id binding = %q", client.orgIDBinding())
		}
	})

	t.Run("empty team ids short-circuits without a query", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{}
		rows, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, nil)", rows, err)
		}
		if len(client.queries) != 0 {
			t.Fatalf("expected no query to be issued for empty team ids")
		}
	})

	t.Run("reads the canonical work_unit_investments table, never investment_metrics_daily", func(t *testing.T) {
		// RED-first regression guard (CHAOS-4398 §0 scope correction): the
		// existing FactInvestment producer (#308) reads
		// investment_metrics_daily, fed by the deprecated legacy rule set
		// (investment_areas.yaml) whose investment_area values are NOT the
		// canonical 5-theme taxonomy. ReadTeamThemeMix must read the
		// canonical work_unit_investments/theme_distribution_json source
		// instead -- a regression back onto investment_metrics_daily would
		// silently reintroduce the deprecated, non-canonical taxonomy.
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		if _, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadTeamThemeMix() error = %v", err)
		}
		if len(client.queries) != 1 {
			t.Fatalf("expected exactly 1 query, got %d", len(client.queries))
		}
		statement := client.queries[0].statement
		if strings.Contains(statement, "investment_metrics_daily") {
			t.Fatalf("statement reads the deprecated investment_metrics_daily table: %q", statement)
		}
		if !strings.Contains(statement, "FROM work_unit_investments") {
			t.Fatalf("statement does not read work_unit_investments: %q", statement)
		}
		if !strings.Contains(statement, "theme_distribution_json") {
			t.Fatalf("statement does not read the canonical theme_distribution_json: %q", statement)
		}
	})

	t.Run("majority-vote team resolution reads work_item_team_attributions, never a membership walk", func(t *testing.T) {
		// CHAOS-4321 regression guard: team attribution must come only from
		// the CHAOS-2600 ownership-precedence work_item_team_attributions
		// source, never author_membership/assignee_membership (a person's
		// team memberships).
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		if _, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadTeamThemeMix() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "work_item_team_attributions") {
			t.Fatalf("statement does not read work_item_team_attributions: %q", statement)
		}
		if !strings.Contains(statement, "is_primary = 1") {
			t.Fatalf("statement does not filter to the primary attribution row: %q", statement)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", err: boom}}}
		rows, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != boom || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})

	t.Run("active time bound reaches the statement and bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		end := mustTime(t, "2026-08-28T00:00:00Z")
		start := mustTime(t, "2026-05-30T00:00:00Z")
		_, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{Active: true, HasStart: true, Start: start, End: end})
		if err != nil {
			t.Fatalf("ReadTeamThemeMix() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "{"+readers.BoundEndParam+":DateTime64(6,'UTC')}") || !strings.Contains(statement, "{"+readers.BoundStartParam+":DateTime64(6,'UTC')}") {
			t.Fatalf("statement = %q, want both window bindings referenced", statement)
		}
		foundStart, foundEnd := false, false
		for _, binding := range client.queries[0].bindings {
			if binding.Name == readers.BoundStartParam {
				foundStart = true
			}
			if binding.Name == readers.BoundEndParam {
				foundEnd = true
			}
		}
		if !foundStart || !foundEnd {
			t.Fatalf("bindings = %#v, want both %s and %s", client.queries[0].bindings, readers.BoundStartParam, readers.BoundEndParam)
		}
	})

	t.Run("inactive time bound adds no window predicate", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		_, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamThemeMix() error = %v", err)
		}
		statement := client.queries[0].statement
		if strings.Contains(statement, readers.BoundEndParam+":DateTime64") {
			t.Fatalf("statement = %q, want no window predicate for an inactive bound", statement)
		}
	})

	t.Run("bounds row count with a LIMIT applied to the whole union", func(t *testing.T) {
		// ClickHouse applies a bare `... UNION ALL ... LIMIT n` to the LAST
		// branch only, not the combined result -- confirmed against a live
		// server (`SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 LIMIT 1`
		// returns all 3 rows). The statement must wrap the union in a
		// subquery so LIMIT actually bounds the combined row count.
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_unit_investments", rows: nil}}}
		if _, err := readers.ReadTeamThemeMix(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadTeamThemeMix() error = %v", err)
		}
		statement := client.queries[0].statement
		unionIndex := strings.Index(statement, "UNION ALL")
		limitIndex := strings.LastIndex(statement, "LIMIT")
		if unionIndex < 0 || limitIndex < 0 || limitIndex < unionIndex {
			t.Fatalf("statement = %q, want LIMIT after UNION ALL", statement)
		}
		closeParenIndex := strings.LastIndex(statement[:limitIndex], ")")
		if closeParenIndex < unionIndex {
			t.Fatalf("statement = %q, want the union wrapped in a subquery before LIMIT", statement)
		}
	})
}
