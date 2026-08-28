package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadTeamInvestment(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM investment_metrics_daily", rows: [][]any{
				{"CHAOS", "product", "growth", "2026-02-22", int64(30), int64(12), int64(4), uint64(850), float64(18.5)},
			}},
		}}
		rows, err := readers.ReadTeamInvestment(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadTeamInvestment() error = %v", err)
		}
		want := readers.InvestmentDailyRow{
			TeamID: "CHAOS", InvestmentArea: "product", ProjectStream: "growth", Day: "2026-02-22",
			DeliveryUnits: 30, WorkItemsCompleted: 12, PRsMerged: 4, ChurnLOC: 850, CycleP50Hours: 18.5,
		}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want [%#v]", rows, want)
		}
		if client.orgIDBinding() != "org-1" {
			t.Fatalf("org_id binding = %q", client.orgIDBinding())
		}
		if got := client.idsBinding(); len(got) != 1 || got[0] != "CHAOS" {
			t.Fatalf("ids binding = %#v", got)
		}
		if !strings.Contains(strings.ToUpper(client.queries[0].statement), "LIMIT") {
			t.Fatalf("statement = %q, want a LIMIT clause", client.queries[0].statement)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM investment_metrics_daily", err: errors.New("boom")}}}
		rows, err := readers.ReadTeamInvestment(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})

	t.Run("active time bound reaches the statement and bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM investment_metrics_daily", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadTeamInvestment(context.Background(), client, "org-1", []string{"CHAOS"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadTeamInvestment() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "toDate({"+readers.BoundEndParam+":DateTime64(6,'UTC')})") {
			t.Fatalf("statement = %q, want the day-bound predicate", statement)
		}
		foundEnd := false
		for _, binding := range client.queries[0].bindings {
			if binding.Name == readers.BoundEndParam {
				foundEnd = true
			}
		}
		if !foundEnd {
			t.Fatalf("bindings = %#v, want a %s binding", client.queries[0].bindings, readers.BoundEndParam)
		}
	})
}

func TestReadProjectInvestment(t *testing.T) {
	t.Parallel()
	t.Run("happy path returns one row per contributing team", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", rows: [][]any{
			{"linear:proj-1", "team-1", "Team One", "product", "growth", "2026-02-22", int64(30), int64(12), int64(4), uint64(850), float64(18.5)},
			{"linear:proj-1", "team-2", "Team Two", "quality", "", "2026-02-22", int64(10), int64(5), int64(2), uint64(100), float64(4.0)},
		}}}}
		rows, err := readers.ReadProjectInvestment(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadProjectInvestment() error = %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %#v, want 2", rows)
		}
		if rows[0].TeamID != "team-1" || rows[0].DeliveryUnits != 30 {
			t.Fatalf("rows[0] = %#v", rows[0])
		}
		if rows[1].TeamID != "team-2" || rows[1].DeliveryUnits != 10 {
			t.Fatalf("rows[1] = %#v", rows[1])
		}
	})

	t.Run("empty ids short-circuits without querying", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", rows: [][]any{{"linear:proj-1", "team-1", "Team One", "product", "growth", "2026-02-22", int64(1), int64(1), int64(1), uint64(1), float64(1)}}}}}
		rows, err := readers.ReadProjectInvestment(context.Background(), client, "org-1", nil, readers.TimeBound{})
		if err != nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, nil)", rows, err)
		}
		if len(client.queries) != 0 {
			t.Fatalf("queries = %#v, want no query issued for empty ids", client.queries)
		}
	})

	t.Run("query error propagates unwrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM team_project_ownership", err: errors.New("boom")}}}
		rows, err := readers.ReadProjectInvestment(context.Background(), client, "org-1", []string{"linear:proj-1"}, readers.TimeBound{})
		if err == nil || err.Error() != "boom" || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, boom)", rows, err)
		}
	})
}
