package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadPullRequestState(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM git_pull_requests", rows: [][]any{{"repo-1", uint32(1042), "open"}}},
		}}
		rows, err := readers.ReadPullRequestState(context.Background(), client, "org-1", []string{"repo-1:1042"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadPullRequestState() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.PullRequestStateRow{RepoID: "repo-1", Number: 1042, State: "open"}) {
			t.Fatalf("rows = %#v", rows)
		}
		if !strings.Contains(client.queries[0].statement, "FROM git_pull_requests") {
			t.Fatalf("statement = %q, want git_pull_requests table", client.queries[0].statement)
		}
		if client.orgIDBinding() != "org-1" {
			t.Fatalf("org_id binding = %q", client.orgIDBinding())
		}
		if got := client.idsBinding(); len(got) != 1 || got[0] != "repo-1:1042" {
			t.Fatalf("ids binding = %#v", got)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM git_pull_requests", err: errors.New("boom")}}}
		rows, err := readers.ReadPullRequestState(context.Background(), client, "org-1", []string{"repo-1:1042"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})

	t.Run("active time bound reaches the statement and bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM git_pull_requests", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadPullRequestState(context.Background(), client, "org-1", []string{"repo-1:1042"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadPullRequestState() error = %v", err)
		}
		statement := client.queries[0].statement
		if !strings.Contains(statement, "multiIf(") {
			t.Fatalf("statement = %q, want the historical multiIf state expression", statement)
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

func TestReadPullRequestReviews(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM git_pull_request_reviews", rows: [][]any{{"review-1", "approved", "repo-1"}}},
		}}
		rows, err := readers.ReadPullRequestReviews(context.Background(), client, "org-1", []string{"repo-1:review-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadPullRequestReviews() error = %v", err)
		}
		if len(rows) != 1 || rows[0] != (readers.PullRequestReviewRow{ReviewID: "review-1", State: "approved", RepoID: "repo-1"}) {
			t.Fatalf("rows = %#v", rows)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM git_pull_request_reviews", err: errors.New("boom")}}}
		rows, err := readers.ReadPullRequestReviews(context.Background(), client, "org-1", []string{"repo-1:review-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})
}
