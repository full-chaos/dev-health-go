package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadIncidents(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM operational_incidents", rows: [][]any{{"incident-1", "open", "high"}}}}}
		rows, err := readers.ReadIncidents(context.Background(), client, "org-1", []string{"incident-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadIncidents() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.IncidentRow{ID: "incident-1", Status: "open", Severity: "high"}) {
			t.Fatalf("rows = %#v", rows)
		}
		if !strings.Contains(client.queries[0].statement, "i.is_deleted = 0") {
			t.Fatalf("statement = %q, want the soft-delete guard", client.queries[0].statement)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM operational_incidents", err: errors.New("boom")}}}
		rows, err := readers.ReadIncidents(context.Background(), client, "org-1", []string{"incident-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})

	t.Run("active time bound forces severity to a constant empty string", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM operational_incidents", rows: [][]any{{"incident-1", "open", ""}}}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		rows, err := readers.ReadIncidents(context.Background(), client, "org-1", []string{"incident-1"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadIncidents() error = %v", err)
		}
		if len(rows) != 1 || rows[0].Severity != "" {
			t.Fatalf("rows = %#v, want severity omitted (empty) on a historical read", rows)
		}
		if !strings.Contains(client.queries[0].statement, "i.resolved_at IS NOT NULL") {
			t.Fatalf("statement = %q, want the historical status expression", client.queries[0].statement)
		}
	})
}
