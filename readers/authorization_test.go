package readers_test

import (
	"context"
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
				RequestedRepositoryIDs: []string{"repo-2"},
			},
			wantAuthorized:    true,
			wantAuthorizedIDs: []string{"repo-1", "repo-2"},
			wantRequested:     true,
			wantRequestedIDs:  []string{"repo-2"},
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

// TestDelegateStatementsStayByteIdentical is the compatibility pin: the
// pre-existing reader, its WithRowLimit twin, and the new WithScope/
// WithScopeAndRowLimit forms called with an allow-all AuthorizationScope and
// no Settings must all render the SAME statement, character for character.
// ops pins this module by go.mod and must see no behaviour change from the
// new, additive surface.
func TestDelegateStatementsStayByteIdentical(t *testing.T) {
	t.Parallel()

	t.Run("status", func(t *testing.T) {
		t.Parallel()
		plain := &fakeClient{}
		if _, err := readers.ReadWorkItemStatus(context.Background(), plain, "org-1", []string{"repo-1:WIDGET-101"}); err != nil {
			t.Fatalf("ReadWorkItemStatus() error = %v", err)
		}
		withScope := &fakeClient{}
		if _, err := readers.ReadWorkItemStatusWithScope(context.Background(), withScope, "org-1", []string{"repo-1:WIDGET-101"}, readers.AuthorizationScope{}, readers.Settings{}); err != nil {
			t.Fatalf("ReadWorkItemStatusWithScope() error = %v", err)
		}
		withScopeAndLimit := &fakeClient{}
		if _, err := readers.ReadWorkItemStatusWithScopeAndRowLimit(context.Background(), withScopeAndLimit, "org-1", []string{"repo-1:WIDGET-101"}, readers.AuthorizationScope{}, readers.Settings{}, readers.DefaultRowLimit); err != nil {
			t.Fatalf("ReadWorkItemStatusWithScopeAndRowLimit() error = %v", err)
		}
		want := plain.queries[0].statement
		if got := withScope.queries[0].statement; got != want {
			t.Fatalf("WithScope statement = %q, want %q", got, want)
		}
		if got := withScopeAndLimit.queries[0].statement; got != want {
			t.Fatalf("WithScopeAndRowLimit statement = %q, want %q", got, want)
		}
	})

	t.Run("title", func(t *testing.T) {
		t.Parallel()
		plain := &fakeClient{}
		if _, err := readers.ReadWorkItemTitle(context.Background(), plain, "org-1", []string{"repo-1:WIDGET-101"}); err != nil {
			t.Fatalf("ReadWorkItemTitle() error = %v", err)
		}
		withScope := &fakeClient{}
		if _, err := readers.ReadWorkItemTitleWithScope(context.Background(), withScope, "org-1", []string{"repo-1:WIDGET-101"}, readers.AuthorizationScope{}, readers.Settings{}); err != nil {
			t.Fatalf("ReadWorkItemTitleWithScope() error = %v", err)
		}
		if got, want := withScope.queries[0].statement, plain.queries[0].statement; got != want {
			t.Fatalf("WithScope statement = %q, want %q", got, want)
		}
	})

	t.Run("completion, inactive TimeBound", func(t *testing.T) {
		t.Parallel()
		plain := &fakeClient{}
		if _, err := readers.ReadWorkItemCompletion(context.Background(), plain, "org-1", []string{"repo-1:WIDGET-101"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadWorkItemCompletion() error = %v", err)
		}
		withScope := &fakeClient{}
		if _, err := readers.ReadWorkItemCompletionWithScope(context.Background(), withScope, "org-1", []string{"repo-1:WIDGET-101"}, readers.TimeBound{}, readers.AuthorizationScope{}, readers.Settings{}); err != nil {
			t.Fatalf("ReadWorkItemCompletionWithScope() error = %v", err)
		}
		if got, want := withScope.queries[0].statement, plain.queries[0].statement; got != want {
			t.Fatalf("WithScope statement = %q, want %q", got, want)
		}
	})

	t.Run("completion, active TimeBound", func(t *testing.T) {
		t.Parallel()
		bound := readers.TimeBound{Active: true, End: mustTime(t, "2026-01-01T00:00:00Z")}
		plain := &fakeClient{}
		if _, err := readers.ReadWorkItemCompletion(context.Background(), plain, "org-1", []string{"repo-1:WIDGET-101"}, bound); err != nil {
			t.Fatalf("ReadWorkItemCompletion() error = %v", err)
		}
		withScope := &fakeClient{}
		if _, err := readers.ReadWorkItemCompletionWithScope(context.Background(), withScope, "org-1", []string{"repo-1:WIDGET-101"}, bound, readers.AuthorizationScope{}, readers.Settings{}); err != nil {
			t.Fatalf("ReadWorkItemCompletionWithScope() error = %v", err)
		}
		if got, want := withScope.queries[0].statement, plain.queries[0].statement; got != want {
			t.Fatalf("WithScope statement = %q, want %q", got, want)
		}
		if len(plain.queries[0].bindings) != len(withScope.queries[0].bindings) {
			t.Fatalf("bindings: plain=%d withScope=%d, want equal counts for an allow-all scope", len(plain.queries[0].bindings), len(withScope.queries[0].bindings))
		}
	})
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
