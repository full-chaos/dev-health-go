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
	return ReadWorkItemStatusWithRowLimit(ctx, client, orgID, ids, DefaultRowLimit)
}

// ReadWorkItemStatusWithRowLimit is ReadWorkItemStatus with a caller-chosen
// row bound, for a caller that must distinguish a FULL page from a TRUNCATED
// one. Pass ProbeRowLimit to read one row more than you will serve; the extra
// row is the only truncation evidence there is. See ProbeRowLimit's own doc
// comment. limit must be a caller-controlled constant, never a request value
// -- WithRowLimit's own contract.
//
// Delegates to ReadWorkItemStatusWithScopeAndRowLimit with an allow-all
// AuthorizationScope and no Settings, so its statement stays byte-identical
// to what this function has always sent.
func ReadWorkItemStatusWithRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, limit int) ([]WorkItemStatusRow, error) {
	return ReadWorkItemStatusWithScopeAndRowLimit(ctx, client, orgID, ids, AuthorizationScope{}, Settings{}, limit)
}

// ReadWorkItemStatusWithScope is ReadWorkItemStatus narrowed to scope and
// bounded by settings. See AuthorizationScope and Settings for the
// allow-all/no-ceiling zero values.
func ReadWorkItemStatusWithScope(ctx context.Context, client QueryClient, orgID string, ids []string, scope AuthorizationScope, settings Settings) ([]WorkItemStatusRow, error) {
	return ReadWorkItemStatusWithScopeAndRowLimit(ctx, client, orgID, ids, scope, settings, DefaultRowLimit)
}

// ReadWorkItemStatusWithScopeAndRowLimit is ReadWorkItemStatus with every
// axis this package exposes: an authorization scope, per-statement
// SETTINGS, and a caller-chosen row bound. See ReadWorkItemStatusWithRowLimit
// for the limit+1 discipline, AuthorizationScope.predicate for the
// work_items<->repos relation the scope predicate reuses, and Settings.Render
// for the SETTINGS clause.
func ReadWorkItemStatusWithScopeAndRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, scope AuthorizationScope, settings Settings, limit int) ([]WorkItemStatusRow, error) {
	statement, scopeBindings := workItemReadStatement(`w.work_item_id, ifNull(w.status, ''), toString(w.repo_id)`, "", scope, settings, limit)

	var rows []WorkItemStatusRow
	err := QueryOrgScopedNamed(ctx, client, "ReadWorkItemStatus", statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemStatusRow
		if scanErr := row.Scan(&r.ID, &r.Status, &r.RepoID); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, scopeBindings...)
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
	return ReadWorkItemTitleWithRowLimit(ctx, client, orgID, ids, DefaultRowLimit)
}

// ReadWorkItemTitleWithRowLimit is ReadWorkItemTitle with a caller-chosen row
// bound. See ReadWorkItemStatusWithRowLimit and ProbeRowLimit.
//
// Delegates to ReadWorkItemTitleWithScopeAndRowLimit with an allow-all
// AuthorizationScope and no Settings, so its statement stays byte-identical
// to what this function has always sent.
func ReadWorkItemTitleWithRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, limit int) ([]WorkItemTitleRow, error) {
	return ReadWorkItemTitleWithScopeAndRowLimit(ctx, client, orgID, ids, AuthorizationScope{}, Settings{}, limit)
}

// ReadWorkItemTitleWithScope is ReadWorkItemTitle narrowed to scope and
// bounded by settings. See AuthorizationScope and Settings for the
// allow-all/no-ceiling zero values.
func ReadWorkItemTitleWithScope(ctx context.Context, client QueryClient, orgID string, ids []string, scope AuthorizationScope, settings Settings) ([]WorkItemTitleRow, error) {
	return ReadWorkItemTitleWithScopeAndRowLimit(ctx, client, orgID, ids, scope, settings, DefaultRowLimit)
}

// ReadWorkItemTitleWithScopeAndRowLimit is ReadWorkItemTitle with every axis
// this package exposes: an authorization scope, per-statement SETTINGS, and
// a caller-chosen row bound. See ReadWorkItemStatusWithScopeAndRowLimit.
func ReadWorkItemTitleWithScopeAndRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, scope AuthorizationScope, settings Settings, limit int) ([]WorkItemTitleRow, error) {
	statement, scopeBindings := workItemReadStatement(`w.work_item_id, ifNull(w.title, ''), toString(w.repo_id)`, "", scope, settings, limit)

	var rows []WorkItemTitleRow
	err := QueryOrgScopedNamed(ctx, client, "ReadWorkItemTitle", statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemTitleRow
		if scanErr := row.Scan(&r.ID, &r.Title, &r.RepoID); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, scopeBindings...)
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
	return ReadWorkItemCompletionWithRowLimit(ctx, client, orgID, ids, timeBound, DefaultRowLimit)
}

// ReadWorkItemCompletionWithRowLimit is ReadWorkItemCompletion with a
// caller-chosen row bound. See ReadWorkItemStatusWithRowLimit and
// ProbeRowLimit.
//
// Delegates to ReadWorkItemCompletionWithScopeAndRowLimit with an allow-all
// AuthorizationScope and no Settings, so its statement stays byte-identical
// to what this function has always sent.
func ReadWorkItemCompletionWithRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound, limit int) ([]WorkItemCompletionRow, error) {
	return ReadWorkItemCompletionWithScopeAndRowLimit(ctx, client, orgID, ids, timeBound, AuthorizationScope{}, Settings{}, limit)
}

// ReadWorkItemCompletionWithScope is ReadWorkItemCompletion narrowed to
// scope and bounded by settings. See AuthorizationScope and Settings for
// the allow-all/no-ceiling zero values.
func ReadWorkItemCompletionWithScope(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound, scope AuthorizationScope, settings Settings) ([]WorkItemCompletionRow, error) {
	return ReadWorkItemCompletionWithScopeAndRowLimit(ctx, client, orgID, ids, timeBound, scope, settings, DefaultRowLimit)
}

// ReadWorkItemCompletionWithScopeAndRowLimit is ReadWorkItemCompletion with
// every axis this package exposes: an authorization scope, per-statement
// SETTINGS, and a caller-chosen row bound. See
// ReadWorkItemStatusWithScopeAndRowLimit.
func ReadWorkItemCompletionWithScopeAndRowLimit(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound, scope AuthorizationScope, settings Settings, limit int) ([]WorkItemCompletionRow, error) {
	completedExpression := "isNotNull(w.completed_at)"
	if timeBound.Active {
		completedExpression = "toUInt8(w.completed_at IS NOT NULL AND w.completed_at <= " + timeBound.AsOfExpression() + ")"
	}
	statement, scopeBindings := workItemReadStatement(`w.work_item_id, `+completedExpression+`, ifNull(w.completed_at, toDateTime64(0, 6, 'UTC')), toString(w.repo_id)`, timeBound.ExistencePredicate("w.created_at"), scope, settings, limit)

	var rows []WorkItemCompletionRow
	err := QueryOrgScopedNamed(ctx, client, "ReadWorkItemCompletion", statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemCompletionRow
		if scanErr := row.Scan(&r.ID, &r.IsCompleted, &r.CompletedAt, &r.RepoID); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, append(timeBound.Bindings(), scopeBindings...)...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// workItemReadStatement renders the three content readers from one closed
// statement shape. With no typed selector it appends the existing ID
// predicate exactly as before. Selector mode adds the package-owned JOIN and
// uses the same WorkItemScopeSQL expression that a census mask can consume.
func workItemReadStatement(selectSQL, extraWhere string, scope AuthorizationScope, settings Settings, limit int) (string, []Binding) {
	from := `FROM work_items AS w FINAL`
	where := `w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}` + extraWhere
	var scopeBindings []Binding
	if scope.RepositorySelectors == nil {
		scopePredicate, bindings := scope.predicate("w.repo_id")
		where += scopePredicate
		scopeBindings = bindings
	} else {
		rendered := WorkItemScopeSQL(scope)
		from += " " + rendered.JoinSQL
		where += " AND (" + rendered.AuthorizationExpr + ")"
		scopeBindings = rendered.Bindings
	}
	statement := "SELECT " + selectSQL + "\n" + from + "\nWHERE " + where
	return WithSettings(WithRowLimit(statement, limit), settings), scopeBindings
}
