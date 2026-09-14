package readers_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// bindingArray returns the []string value of the named binding on the last
// query the client captured, and whether that binding was present at all
// (as opposed to present with a nil value, which never happens for a
// []string binding -- a caller either sends the binding or doesn't).
func bindingArray(t *testing.T, client *fakeClient, name string) ([]string, bool) {
	t.Helper()
	if len(client.queries) == 0 {
		t.Fatal("no query was executed")
	}
	for _, binding := range client.queries[len(client.queries)-1].bindings {
		if binding.Name == name {
			values, ok := binding.Value.([]string)
			if !ok {
				t.Fatalf("binding %q value = %#v, want []string", name, binding.Value)
			}
			return values, true
		}
	}
	return nil, false
}

// TestAuthorizationScopePredicateMatrix covers {nil, empty, one, many} x
// {grants, requested scope}, exercised through ReadWorkItemStatusWithScope
// -- every predicate-taking reader in this package shares
// AuthorizationScope.predicate, so pinning its rendering once here is
// pinning it everywhere it is used; ReadWorkItemTitle/Completion below only
// need a smoke test confirming each one actually wires the scope through.
func TestAuthorizationScopePredicateMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		scope             readers.AuthorizationScope
		wantAuthorized    bool
		wantAuthorizedIDs []string
		wantRequested     bool
		wantRequestedIDs  []string
	}{
		{
			name:  "nil grants, nil requested scope -- allow-all, no predicate at all",
			scope: readers.AuthorizationScope{},
		},
		{
			name:              "empty grants -- deny-all on that dimension",
			scope:             readers.AuthorizationScope{GrantedRepositoryIDs: []string{}},
			wantAuthorized:    true,
			wantAuthorizedIDs: []string{},
		},
		{
			name:              "one grant",
			scope:             readers.AuthorizationScope{GrantedRepositoryIDs: []string{"repo-1"}},
			wantAuthorized:    true,
			wantAuthorizedIDs: []string{"repo-1"},
		},
		{
			name:              "many grants",
			scope:             readers.AuthorizationScope{GrantedRepositoryIDs: []string{"repo-1", "repo-2", "repo-3"}},
			wantAuthorized:    true,
			wantAuthorizedIDs: []string{"repo-1", "repo-2", "repo-3"},
		},
		{
			name:             "empty requested scope -- deny-all on that dimension",
			scope:            readers.AuthorizationScope{RequestedRepositoryIDs: []string{}},
			wantRequested:    true,
			wantRequestedIDs: []string{},
		},
		{
			name:             "one requested id",
			scope:            readers.AuthorizationScope{RequestedRepositoryIDs: []string{"repo-9"}},
			wantRequested:    true,
			wantRequestedIDs: []string{"repo-9"},
		},
		{
			name:             "many requested ids",
			scope:            readers.AuthorizationScope{RequestedRepositoryIDs: []string{"repo-9", "repo-8"}},
			wantRequested:    true,
			wantRequestedIDs: []string{"repo-9", "repo-8"},
		},
		{
			name: "both set -- intersection, not either alone",
			scope: readers.AuthorizationScope{
				GrantedRepositoryIDs:   []string{"repo-1", "repo-2"},
				RequestedRepositoryIDs: []string{"repo-2", "repo-3"},
			},
			wantAuthorized:    true,
			wantAuthorizedIDs: []string{"repo-1", "repo-2"},
			wantRequested:     true,
			wantRequestedIDs:  []string{"repo-2", "repo-3"},
		},
		{
			name: "both empty -- deny-all on both dimensions at once",
			scope: readers.AuthorizationScope{
				GrantedRepositoryIDs:   []string{},
				RequestedRepositoryIDs: []string{},
			},
			wantAuthorized:    true,
			wantAuthorizedIDs: []string{},
			wantRequested:     true,
			wantRequestedIDs:  []string{},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{}
			if _, err := readers.ReadWorkItemStatusWithScope(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"}, tt.scope, readers.Settings{}); err != nil {
				t.Fatalf("ReadWorkItemStatusWithScope() error = %v", err)
			}
			statement := client.queries[0].statement

			authorizedIDs, sawAuthorized := bindingArray(t, client, "authorized_repo_ids")
			if sawAuthorized != tt.wantAuthorized {
				t.Fatalf("authorized_repo_ids present = %v, want %v", sawAuthorized, tt.wantAuthorized)
			}
			if tt.wantAuthorized {
				if !strings.Contains(statement, "AND toString(w.repo_id) IN {authorized_repo_ids:Array(String)}") {
					t.Fatalf("statement = %q, want the authorized_repo_ids predicate", statement)
				}
				if !equalStringSlices(authorizedIDs, tt.wantAuthorizedIDs) {
					t.Fatalf("authorized_repo_ids = %#v, want %#v", authorizedIDs, tt.wantAuthorizedIDs)
				}
			} else if strings.Contains(statement, "authorized_repo_ids") {
				t.Fatalf("statement = %q, must not mention authorized_repo_ids for a nil grants list", statement)
			}

			requestedIDs, sawRequested := bindingArray(t, client, "requested_repo_ids")
			if sawRequested != tt.wantRequested {
				t.Fatalf("requested_repo_ids present = %v, want %v", sawRequested, tt.wantRequested)
			}
			if tt.wantRequested {
				if !strings.Contains(statement, "AND toString(w.repo_id) IN {requested_repo_ids:Array(String)}") {
					t.Fatalf("statement = %q, want the requested_repo_ids predicate", statement)
				}
				if !equalStringSlices(requestedIDs, tt.wantRequestedIDs) {
					t.Fatalf("requested_repo_ids = %#v, want %#v", requestedIDs, tt.wantRequestedIDs)
				}
			} else if strings.Contains(statement, "requested_repo_ids") {
				t.Fatalf("statement = %q, must not mention requested_repo_ids for a nil requested-scope list", statement)
			}

			if tt.wantAuthorized && tt.wantRequested {
				// Intersection, not either alone: both IN clauses must be
				// ANDed together in the rendered WHERE, never OR'd.
				authorizedAt := strings.Index(statement, "authorized_repo_ids")
				requestedAt := strings.Index(statement, "requested_repo_ids")
				if authorizedAt < 0 || requestedAt < 0 || authorizedAt > requestedAt {
					t.Fatalf("statement = %q, want authorized_repo_ids before requested_repo_ids", statement)
				}
				between := statement[authorizedAt:requestedAt]
				if strings.Contains(between, " OR ") {
					t.Fatalf("statement = %q, the two scope predicates must be ANDed, never OR'd", statement)
				}
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReadWorkItemTitleWithScopeAppliesThePredicate is the smoke test for
// the title reader: the full matrix is pinned once, above, against status.
func TestReadWorkItemTitleWithScopeAppliesThePredicate(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	scope := readers.AuthorizationScope{GrantedRepositoryIDs: []string{"repo-1"}}
	if _, err := readers.ReadWorkItemTitleWithScope(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"}, scope, readers.Settings{}); err != nil {
		t.Fatalf("ReadWorkItemTitleWithScope() error = %v", err)
	}
	if !strings.Contains(client.queries[0].statement, "authorized_repo_ids") {
		t.Fatalf("statement = %q, want the scope predicate wired through", client.queries[0].statement)
	}
}

// TestReadWorkItemCompletionWithScopeAppliesThePredicate is the smoke test
// for the completion reader, including alongside an active TimeBound (the
// two predicates -- existence and scope -- must coexist).
func TestReadWorkItemCompletionWithScopeAppliesThePredicate(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	scope := readers.AuthorizationScope{RequestedRepositoryIDs: []string{"repo-9"}}
	bound := readers.TimeBound{Active: true, End: mustTime(t, "2026-01-01T00:00:00Z")}
	if _, err := readers.ReadWorkItemCompletionWithScope(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"}, bound, scope, readers.Settings{}); err != nil {
		t.Fatalf("ReadWorkItemCompletionWithScope() error = %v", err)
	}
	statement := client.queries[0].statement
	if !strings.Contains(statement, "requested_repo_ids") {
		t.Fatalf("statement = %q, want the scope predicate wired through", statement)
	}
	if !strings.Contains(statement, "w.created_at <=") {
		t.Fatalf("statement = %q, want the TimeBound existence predicate to survive alongside the scope predicate", statement)
	}
}

// TestLegacyReaderStatementsMatchBaseline compares every existing work-item
// reader API with statements and bindings captured independently from the
// exact old implementation at 27dfd43f9965c053ad80a9796dffa5a4873325d9.
// The expected SQL is intentionally literal: comparing a legacy call to a
// new builder only proves that both calls share the same builder.
func TestLegacyReaderStatementsMatchBaseline(t *testing.T) {
	t.Parallel()
	const orgID = "org-1"
	ids := []string{"repo-1:WIDGET-101"}
	activeEnd := mustTime(t, "2026-01-01T00:00:00Z")
	activeBound := readers.TimeBound{Active: true, End: activeEnd}

	tests := []struct {
		name          string
		read          func(*fakeClient) error
		wantStatement string
		wantBindings  []readers.Binding
	}{
		{
			name: "status default row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemStatus(context.Background(), client, orgID, ids)
				return err
			},
			wantStatement: `SELECT w.work_item_id, ifNull(w.status, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}
LIMIT 200`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
			},
		},
		{
			name: "status custom row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemStatusWithRowLimit(context.Background(), client, orgID, ids, 7)
				return err
			},
			wantStatement: `SELECT w.work_item_id, ifNull(w.status, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}
LIMIT 7`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
			},
		},
		{
			name: "title default row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemTitle(context.Background(), client, orgID, ids)
				return err
			},
			wantStatement: `SELECT w.work_item_id, ifNull(w.title, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}
LIMIT 200`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
			},
		},
		{
			name: "title custom row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemTitleWithRowLimit(context.Background(), client, orgID, ids, 7)
				return err
			},
			wantStatement: `SELECT w.work_item_id, ifNull(w.title, ''), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}
LIMIT 7`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
			},
		},
		{
			name: "completion inactive default row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemCompletion(context.Background(), client, orgID, ids, readers.TimeBound{})
				return err
			},
			wantStatement: `SELECT w.work_item_id, isNotNull(w.completed_at), ifNull(w.completed_at, toDateTime64(0, 6, 'UTC')), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}
LIMIT 200`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
			},
		},
		{
			name: "completion inactive custom row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemCompletionWithRowLimit(context.Background(), client, orgID, ids, readers.TimeBound{}, 7)
				return err
			},
			wantStatement: `SELECT w.work_item_id, isNotNull(w.completed_at), ifNull(w.completed_at, toDateTime64(0, 6, 'UTC')), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)}
LIMIT 7`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
			},
		},
		{
			name: "completion active default row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemCompletion(context.Background(), client, orgID, ids, activeBound)
				return err
			},
			wantStatement: `SELECT w.work_item_id, toUInt8(w.completed_at IS NOT NULL AND w.completed_at <= {time_end:DateTime64(6,'UTC')}), ifNull(w.completed_at, toDateTime64(0, 6, 'UTC')), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)} AND w.created_at <= {time_end:DateTime64(6,'UTC')}
LIMIT 200`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
				{Name: "time_end", Value: activeEnd},
			},
		},
		{
			name: "completion active custom row limit",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemCompletionWithRowLimit(context.Background(), client, orgID, ids, activeBound, 7)
				return err
			},
			wantStatement: `SELECT w.work_item_id, toUInt8(w.completed_at IS NOT NULL AND w.completed_at <= {time_end:DateTime64(6,'UTC')}), ifNull(w.completed_at, toDateTime64(0, 6, 'UTC')), toString(w.repo_id)
FROM work_items AS w FINAL
WHERE w.org_id = {org_id:String} AND concat(toString(w.repo_id), ':', w.work_item_id) IN {ids:Array(String)} AND w.created_at <= {time_end:DateTime64(6,'UTC')}
LIMIT 7`,
			wantBindings: []readers.Binding{
				{Name: "org_id", Value: orgID},
				{Name: "ids", Value: ids},
				{Name: "time_end", Value: activeEnd},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{}
			if err := tt.read(client); err != nil {
				t.Fatalf("legacy reader error = %v", err)
			}
			if len(client.queries) != 1 {
				t.Fatalf("captured queries = %d, want 1", len(client.queries))
			}
			got := client.queries[0]
			if got.statement != tt.wantStatement {
				t.Fatalf("statement = %q, want baseline statement %q", got.statement, tt.wantStatement)
			}
			if !reflect.DeepEqual(got.bindings, tt.wantBindings) {
				t.Fatalf("bindings = %#v, want baseline bindings %#v", got.bindings, tt.wantBindings)
			}
		})
	}
}

// TestSettingsReachTheRenderedStatement confirms a non-zero Settings value
// actually reaches the statement text through a real reader call, not just
// through Settings.Render in isolation.
func TestSettingsReachTheRenderedStatement(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	settings := readers.Settings{MaxExecutionTimeSeconds: 5, MaxResultRows: 201}
	if _, err := readers.ReadWorkItemStatusWithScope(context.Background(), client, "org-1", []string{"repo-1:WIDGET-101"}, readers.AuthorizationScope{}, settings); err != nil {
		t.Fatalf("ReadWorkItemStatusWithScope() error = %v", err)
	}
	statement := client.queries[0].statement
	if !strings.HasSuffix(statement, settings.Render()) {
		t.Fatalf("statement = %q, want it to end with the rendered SETTINGS clause %q", statement, settings.Render())
	}
	if !strings.Contains(statement, "LIMIT "+"200") {
		t.Fatalf("statement = %q, want LIMIT to still precede SETTINGS", statement)
	}
}
