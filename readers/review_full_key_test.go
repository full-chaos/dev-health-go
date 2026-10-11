package readers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// git_pull_request_reviews is keyed (org_id, repo_id, number, review_id), and
// a review id recurs across one repository's pull requests (one provider
// names an approval by its approver). The read keys on the full key, so a
// subject naming one pull request's review never reads another's.
func TestReadPullRequestReviewsKeysOnTheFullReviewKey(t *testing.T) {
	t.Parallel()
	client := &fakeClient{tables: []fakeTable{{match: "FROM git_pull_request_reviews", rows: nil}}}
	if _, err := readers.ReadPullRequestReviews(context.Background(), client, "org-1", []string{"repo-1:1042:approval-42"}, readers.TimeBound{}); err != nil {
		t.Fatalf("ReadPullRequestReviews() error = %v", err)
	}
	statement := client.queries[0].statement
	want := "concat(toString(r.repo_id), ':', toString(r.number), ':', r.review_id) IN {ids:Array(String)}"
	if !strings.Contains(statement, want) {
		t.Fatalf("statement = %q, want the ids key %q", statement, want)
	}
	if !strings.Contains(statement, "SELECT r.review_id, ifNull(r.state, ''), toString(r.repo_id), r.number\n") {
		t.Fatalf("statement = %q, want the pull request number selected", statement)
	}
}
