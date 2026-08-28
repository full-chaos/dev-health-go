package readers

import "context"

// PullRequestStateRow is one row of git_pull_requests's state, keyed by
// repository and pull request number (git_pull_requests has no single
// column primary key).
type PullRequestStateRow struct {
	RepoID string
	// Number is scanned as uint32, matching git_pull_requests.number's
	// production column type in ClickHouse. clickhouse-go's native driver
	// REJECTS scanning a UInt32 column into an *int64 destination outright
	// ("converting UInt32 to *int64 is unsupported"), so a caller that
	// needs it as an int64 (a composite row key, an arithmetic use) must
	// convert AFTER this reader hands the value back -- the same class of
	// defect CHAOS-3789 fixed in devhealthsource's queryPullRequests. The
	// conversion happens once the value is safely in Go, never inside the
	// Scan itself.
	Number uint32
	State  string
}

// ReadPullRequestState reads git_pull_requests.state (falling back to a
// derived merged/closed/open state on a bounded historical read), the same
// column devhealthsource/tables.go's queryPullRequests already reads.
//
// CHAOS-3781 Tier B: a pull request's state at a past instant is a pure
// function of the immutable event timestamps the row already carries, so
// this is a reconstruction of a RECORDED fact, not of an unrecorded one
// (§19.8.3). Order matters -- merged wins over closed, because a merged
// pull request is also closed.
//
// The existence guard is the outer WHERE (timeBound.ExistencePredicate): a
// pull request created after the requested time is not returned at all, so
// the subject reports no fact rather than a current-state one (AC-3781-3).
func ReadPullRequestState(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]PullRequestStateRow, error) {
	stateExpression := "ifNull(p.state, '')"
	if timeBound.Active {
		asOf := timeBound.AsOfExpression()
		stateExpression = "multiIf(" +
			"p.merged_at IS NOT NULL AND p.merged_at <= " + asOf + ", 'merged', " +
			"p.closed_at IS NOT NULL AND p.closed_at <= " + asOf + ", 'closed', " +
			"'open')"
	}
	statement := WithRowLimit(`SELECT toString(p.repo_id), p.number, `+stateExpression+`
FROM git_pull_requests AS p FINAL
WHERE p.org_id = {org_id:String} AND concat(toString(p.repo_id), ':', toString(p.number)) IN {ids:Array(String)}`+timeBound.ExistencePredicate("p.created_at"), DefaultRowLimit)

	var rows []PullRequestStateRow
	err := QueryOrgScoped(ctx, client, statement, orgID, ids, func(row RowScanner) error {
		var r PullRequestStateRow
		if scanErr := row.Scan(&r.RepoID, &r.Number, &r.State); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// PullRequestReviewRow is one row of git_pull_request_reviews's state.
type PullRequestReviewRow struct {
	ReviewID string
	State    string
	RepoID   string
}

// ReadPullRequestReviews reads git_pull_request_reviews.state, the same
// column devhealthsource/tables.go's queryPullRequestReviews already reads.
//
// CHAOS-3781 Tier B: a review is an immutable point event -- its state is
// decided when it is submitted and is never revised -- so the only
// temporal question is whether it had been submitted yet.
func ReadPullRequestReviews(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]PullRequestReviewRow, error) {
	statement := WithRowLimit(`SELECT r.review_id, ifNull(r.state, ''), toString(r.repo_id)
FROM git_pull_request_reviews AS r FINAL
WHERE r.org_id = {org_id:String} AND concat(toString(r.repo_id), ':', r.review_id) IN {ids:Array(String)}`+timeBound.ExistencePredicate("r.submitted_at"), DefaultRowLimit)

	var rows []PullRequestReviewRow
	err := QueryOrgScoped(ctx, client, statement, orgID, ids, func(row RowScanner) error {
		var r PullRequestReviewRow
		if scanErr := row.Scan(&r.ReviewID, &r.State, &r.RepoID); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
