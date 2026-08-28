package readers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadRepositoryIdentity(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM repos", rows: [][]any{{"repo-1", "example-org/widget-service", "synthetic"}}}}}
		rows, err := readers.ReadRepositoryIdentity(context.Background(), client, "org-1", []string{"repo-1"})
		if err != nil {
			t.Fatalf("ReadRepositoryIdentity() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.RepositoryIdentityRow{ID: "repo-1", Slug: "example-org/widget-service", Provider: "synthetic"}) {
			t.Fatalf("rows = %#v", rows)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM repos", err: errors.New("boom")}}}
		rows, err := readers.ReadRepositoryIdentity(context.Background(), client, "org-1", []string{"repo-1"})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}

func TestReadWorkItemIdentity(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", rows: [][]any{{"WIDGET-101", "Investigate checkout flake", "repo-1"}}}}}
	rows, err := readers.ReadWorkItemIdentity(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"})
	if err != nil {
		t.Fatalf("ReadWorkItemIdentity() error = %v", err)
	}
	if len(rows) != 1 || rows[0] != (readers.WorkItemIdentityRow{ID: "WIDGET-101", Title: "Investigate checkout flake", RepoID: "repo-1"}) {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestReadRepositoryIDs(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM repos", rows: [][]any{{"repo-1"}}}}}
	rows, err := readers.ReadRepositoryIDs(context.Background(), client, "org-1", []string{"repo-1"})
	if err != nil {
		t.Fatalf("ReadRepositoryIDs() error = %v", err)
	}
	if len(rows) != 1 || rows[0] != (readers.RepositoryIDRow{ID: "repo-1"}) {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestReadWorkItemRepository(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", rows: [][]any{{"WIDGET-101", "repo-1", "example-org/widget-service"}}}}}
		rows, err := readers.ReadWorkItemRepository(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"})
		if err != nil {
			t.Fatalf("ReadWorkItemRepository() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.WorkItemRepositoryRow{ID: "WIDGET-101", RepoID: "repo-1", RepoSlug: "example-org/widget-service"}) {
			t.Fatalf("rows = %#v", rows)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM work_items", err: errors.New("boom")}}}
		rows, err := readers.ReadWorkItemRepository(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}
