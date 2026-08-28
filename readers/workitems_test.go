package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", value, err)
	}
	return parsed
}

func TestReadWorkItemStatus(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", rows: [][]any{{"WIDGET-101", "in_progress", "repo-1"}}}}}
		rows, err := readers.ReadWorkItemStatus(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"})
		if err != nil {
			t.Fatalf("ReadWorkItemStatus() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.WorkItemStatusRow{ID: "WIDGET-101", Status: "in_progress", RepoID: "repo-1"}) {
			t.Fatalf("rows = %#v", rows)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", err: errors.New("boom")}}}
		rows, err := readers.ReadWorkItemStatus(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}

func TestReadWorkItemTitle(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", rows: [][]any{{"WIDGET-101", "Investigate checkout flake", "repo-1"}}}}}
	rows, err := readers.ReadWorkItemTitle(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"})
	if err != nil {
		t.Fatalf("ReadWorkItemTitle() error = %v", err)
	}
	if len(rows) != 1 || rows[0] != (readers.WorkItemTitleRow{ID: "WIDGET-101", Title: "Investigate checkout flake", RepoID: "repo-1"}) {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestReadWorkItemCompletion(t *testing.T) {
	t.Parallel()
	t.Run("completed row scans the timestamp", func(t *testing.T) {
		t.Parallel()
		completedAt := mustTime(t, "2026-01-14T12:00:00Z")
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", rows: [][]any{{"WIDGET-101", uint8(1), completedAt, "repo-1"}}}}}
		rows, err := readers.ReadWorkItemCompletion(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadWorkItemCompletion() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.WorkItemCompletionRow{ID: "WIDGET-101", IsCompleted: 1, CompletedAt: completedAt, RepoID: "repo-1"}) {
			t.Fatalf("rows = %#v", rows)
		}
	})

	t.Run("active time bound reaches the statement", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadWorkItemCompletion(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadWorkItemCompletion() error = %v", err)
		}
		if got := client.queries[0].statement; !strings.Contains(got, "toUInt8(w.completed_at IS NOT NULL") {
			t.Fatalf("statement = %q, want the historical completedExpression", got)
		}
	})
}
