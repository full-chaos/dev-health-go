package readers_test

import (
	"context"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// TestQueryOrgScopedPreservesOriginalSignature pins the pre-instrumentation
// exported signature of QueryOrgScoped (codex review: an exported symbol's
// signature is a public contract package consumers pin tagged versions
// against; widening it in place to add a reader parameter would break any
// external caller on upgrade, since Go has no overload or default
// parameter). Every reader inside this package now calls the reader-aware
// QueryOrgScopedNamed directly instead -- this test is the only thing
// exercising the back-compat QueryOrgScoped wrapper.
func TestQueryOrgScopedPreservesOriginalSignature(t *testing.T) {
	client := &fakeClient{tables: []fakeTable{{match: "FROM some_table", rows: [][]any{{"a"}, {"b"}}}}}
	var scanned []string
	err := readers.QueryOrgScoped(context.Background(), client, "SELECT id FROM some_table", "org-1", []string{"id-1"}, func(row readers.RowScanner) error {
		var id string
		if scanErr := row.Scan(&id); scanErr != nil {
			return scanErr
		}
		scanned = append(scanned, id)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryOrgScoped() error = %v", err)
	}
	if len(scanned) != 2 {
		t.Fatalf("scanned %d rows, want 2", len(scanned))
	}
}

// TestQueryOrgScopedReportsUnattributedToInstrumentation proves the
// back-compat wrapper still reports through any wired Instrumentation --
// under the generic "unattributed" reader label, since it has none of its
// own -- rather than silently bypassing telemetry.
func TestQueryOrgScopedReportsUnattributedToInstrumentation(t *testing.T) {
	recorder := &recordingInstrumentation{}
	ctx := readers.ContextWithInstrumentation(context.Background(), recorder)
	client := &fakeClient{tables: []fakeTable{{match: "FROM some_table", rows: [][]any{{"a"}}}}}

	err := readers.QueryOrgScoped(ctx, client, "SELECT id FROM some_table", "org-1", []string{"id-1"}, func(row readers.RowScanner) error {
		var id string
		return row.Scan(&id)
	})
	if err != nil {
		t.Fatalf("QueryOrgScoped() error = %v", err)
	}
	if len(recorder.queries) != 1 {
		t.Fatalf("expected 1 recorded query, got %d", len(recorder.queries))
	}
	if got := recorder.queries[0].reader; got != "unattributed" {
		t.Errorf("reader = %q, want unattributed", got)
	}
}
