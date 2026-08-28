package readers

import "context"

// RepositoryIdentityRow is one row of repos' identity columns.
type RepositoryIdentityRow struct {
	ID       string
	Slug     string
	Provider string
}

// ReadRepositoryIdentity reads repository identity from repos.repo/
// repos.provider, the same table and columns
// devhealthsource/tables.go's queryRepositories already reads.
func ReadRepositoryIdentity(ctx context.Context, client QueryClient, orgID string, ids []string) ([]RepositoryIdentityRow, error) {
	statement := WithRowLimit(`SELECT toString(r.id), ifNull(r.repo, ''), ifNull(r.provider, '')
FROM repos AS r FINAL
WHERE r.org_id = {org_id:String} AND toString(r.id) IN {ids:Array(String)}`, DefaultRowLimit)

	var rows []RepositoryIdentityRow
	err := QueryOrgScopedNamed(ctx, client, "ReadRepositoryIdentity", statement, orgID, ids, func(row RowScanner) error {
		var r RepositoryIdentityRow
		if scanErr := row.Scan(&r.ID, &r.Slug, &r.Provider); scanErr != nil {
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

// WorkItemIdentityRow is one row of work_items' identity columns.
type WorkItemIdentityRow struct {
	ID     string
	Title  string
	RepoID string
}

// ReadWorkItemIdentity reads work item identity from work_items.title, the
// same table and column devhealthsource/tables.go's queryWorkItems already
// reads.
func ReadWorkItemIdentity(ctx context.Context, client QueryClient, orgID string, ids []string) ([]WorkItemIdentityRow, error) {
	statement := WithRowLimit(`SELECT w.work_item_id, ifNull(w.title, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}`, DefaultRowLimit)

	var rows []WorkItemIdentityRow
	err := QueryOrgScopedNamed(ctx, client, "ReadWorkItemIdentity", statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemIdentityRow
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

// RepositoryIDRow is one row confirming a repository id exists (and is
// in-org, via the query's WHERE clause).
type RepositoryIDRow struct {
	ID string
}

// ReadRepositoryIDs confirms which of the requested repository ids exist
// in-org, for FactMembership's repository-containment answer: "which
// organization a repository belongs to" needs no column beyond the id,
// since membership itself is exactly the caller's own org.
func ReadRepositoryIDs(ctx context.Context, client QueryClient, orgID string, ids []string) ([]RepositoryIDRow, error) {
	statement := WithRowLimit(`SELECT toString(r.id)
FROM repos AS r FINAL
WHERE r.org_id = {org_id:String} AND toString(r.id) IN {ids:Array(String)}`, DefaultRowLimit)

	var rows []RepositoryIDRow
	err := QueryOrgScopedNamed(ctx, client, "ReadRepositoryIDs", statement, orgID, ids, func(row RowScanner) error {
		var r RepositoryIDRow
		if scanErr := row.Scan(&r.ID); scanErr != nil {
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

// WorkItemRepositoryRow is one row joining a work item to its owning
// repository.
type WorkItemRepositoryRow struct {
	ID       string
	RepoID   string
	RepoSlug string
}

// ReadWorkItemRepository reads which repository a work item belongs to
// (FactMembership's repository-containment answer), joining work_items to
// repos. There is no canonical team or project table in this schema (the
// same reason devhealthsource/teams_projects.go's TeamsProjectsSource is a
// documented no-op), so repository containment is the only membership
// relationship this table pair can honestly support.
func ReadWorkItemRepository(ctx context.Context, client QueryClient, orgID string, ids []string) ([]WorkItemRepositoryRow, error) {
	statement := WithRowLimit(`SELECT w.work_item_id, toString(w.repo_id), ifNull(r.repo, '')
FROM work_items AS w FINAL INNER JOIN repos AS r FINAL ON r.id = w.repo_id AND r.org_id = w.org_id
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}`, DefaultRowLimit)

	var rows []WorkItemRepositoryRow
	err := QueryOrgScopedNamed(ctx, client, "ReadWorkItemRepository", statement, orgID, ids, func(row RowScanner) error {
		var r WorkItemRepositoryRow
		if scanErr := row.Scan(&r.ID, &r.RepoID, &r.RepoSlug); scanErr != nil {
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
