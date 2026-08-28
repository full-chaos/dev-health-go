package readers

import (
	"context"
	"time"
)

// WorkItemStatusRow is one row of work_items.status.
type WorkItemStatusRow struct {
	ID     string
	Status string
	RepoID string
}

// ReadWorkItemStatus reads work_items.status, the same column
// devhealthsource/tables.go's queryWorkItems already reads. This fact has
// no recorded history (work_items.status is overwritten in place with no
// history column), so a caller only ever queries it on the current axis --
// there is no TimeBound parameter here for that reason.
func ReadWorkItemStatus(ctx context.Context, client QueryClient, orgID string, ids []string) ([]WorkItemStatusRow, error) {
	statement := WithRowLimit(`SELECT w.work_item_id, ifNull(w.status, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}`, DefaultRowLimit)

	var rows []WorkItemStatusRow
	err := QueryOrgScoped(ctx, client, statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemStatusRow
		if scanErr := row.Scan(&r.ID, &r.Status, &r.RepoID); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// WorkItemTitleRow is one row of work_items.title.
type WorkItemTitleRow struct {
	ID     string
	Title  string
	RepoID string
}

// ReadWorkItemTitle reads work_items.title -- minimal work descriptors --
// the same column devhealthsource/tables.go's queryWorkItems already reads.
// Like status, title has no recorded history, so there is no TimeBound
// parameter here.
func ReadWorkItemTitle(ctx context.Context, client QueryClient, orgID string, ids []string) ([]WorkItemTitleRow, error) {
	statement := WithRowLimit(`SELECT w.work_item_id, ifNull(w.title, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}`, DefaultRowLimit)

	var rows []WorkItemTitleRow
	err := QueryOrgScoped(ctx, client, statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemTitleRow
		if scanErr := row.Scan(&r.ID, &r.Title, &r.RepoID); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// WorkItemCompletionRow is one row of work_items.completed_at, projected as
// a completion flag plus the timestamp.
type WorkItemCompletionRow struct {
	ID string
	// IsCompleted is scanned as uint8 (ClickHouse's isNotNull/toUInt8
	// result type), never bool.
	IsCompleted uint8
	CompletedAt time.Time
	RepoID      string
}

// ReadWorkItemCompletion reads work_items.completed_at.
//
// Deviation from devhealthsource: devhealthsource/tables.go's
// queryWorkItems never selects completed_at (it only needed
// status/title/url/updated_at for projection), but the column is real --
// it is seeded by
// testdata/fullstack/v1/seed/clickhouse/001_widget_service.sql's
// `INSERT INTO work_items (... completed_at, closed_at ...)`.
// isNotNull/ifNull avoid ever scanning a bare Nullable(DateTime64) column
// into Go, matching devhealthsource/tables.go's convention of only ever
// scanning coalesced, non-null timestamps.
//
// CHAOS-3781 Tier B: completion is the one work-item fact with a recorded
// timestamp, so "was it done at T" is answerable exactly -- unlike the
// status vocabulary next door, which has no history at all. An item
// completed AFTER the requested time reads as not completed then, which is
// what the row actually records.
func ReadWorkItemCompletion(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]WorkItemCompletionRow, error) {
	completedExpression := "isNotNull(w.completed_at)"
	if timeBound.Active {
		completedExpression = "toUInt8(w.completed_at IS NOT NULL AND w.completed_at <= " + timeBound.AsOfExpression() + ")"
	}
	statement := WithRowLimit(`SELECT w.work_item_id, `+completedExpression+`, ifNull(w.completed_at, toDateTime64(0, 6, 'UTC')), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}`+timeBound.ExistencePredicate("w.created_at"), DefaultRowLimit)

	var rows []WorkItemCompletionRow
	err := QueryOrgScoped(ctx, client, statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemCompletionRow
		if scanErr := row.Scan(&r.ID, &r.IsCompleted, &r.CompletedAt, &r.RepoID); scanErr != nil {
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
