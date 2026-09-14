package readers_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestWorkItemScopeSQLSelectorRendering(t *testing.T) {
	t.Parallel()
	requested := readers.RepositorySelectorSet{All: true}
	scope := readers.AuthorizationScope{
		GrantedRepositoryIDs:   []string{"repo-a"},
		RequestedRepositoryIDs: []string{"repo-a", "repo-b"},
		RepositorySelectors: &readers.RepositorySelectorScope{
			Granted: readers.RepositorySelectorSet{
				ExactSlugs: []string{" ACME/Widgets ", "malformed/too/many"},
				Owners:     []string{" Acme ", "bad/owner"},
			},
			Requested: &requested,
		},
	}

	rendered := readers.WorkItemScopeSQL(scope)
	if got, want := rendered.JoinSQL, "LEFT JOIN repos AS r FINAL ON r.id = w.repo_id AND r.org_id = w.org_id"; got != want {
		t.Fatalf("JoinSQL = %q, want %q", got, want)
	}
	for _, want := range []string{
		"toString(w.repo_id) IN {authorized_repo_ids:Array(String)}",
		"toString(w.repo_id) IN {requested_repo_ids:Array(String)}",
		"{authorized_repo_all:UInt8} = 1",
		"has({authorized_repo_slugs:Array(String)}, replaceAll(replaceAll(lowerUTF8(replaceRegexpAll(trimBoth(ifNull(r.repo, ''))",
		"has({authorized_repo_owners:Array(String)}, arrayElement(splitByChar('/', replaceAll(replaceAll(lowerUTF8(replaceRegexpAll(trimBoth(ifNull(r.repo, ''))",
		"{requested_repo_all:UInt8} = 1",
		"toString(w.repo_id) != '00000000-0000-0000-0000-000000000000'",
		"match(replaceAll(replaceAll(lowerUTF8(replaceRegexpAll(trimBoth(ifNull(r.repo, ''))",
	} {
		if !strings.Contains(rendered.AuthorizationExpr, want) {
			t.Fatalf("AuthorizationExpr = %q, want substring %q", rendered.AuthorizationExpr, want)
		}
	}
	if strings.Contains(rendered.AuthorizationExpr, "malformed") || strings.Contains(rendered.AuthorizationExpr, "bad/owner") {
		t.Fatalf("AuthorizationExpr = %q, must not interpolate selector values", rendered.AuthorizationExpr)
	}

	wantBindings := []readers.Binding{
		{Name: "authorized_repo_ids", Value: []string{"repo-a"}},
		{Name: "requested_repo_ids", Value: []string{"repo-a", "repo-b"}},
		{Name: "authorized_repo_all", Value: uint8(0)},
		{Name: "authorized_repo_slugs", Value: []string{"acme/widgets"}},
		{Name: "authorized_repo_owners", Value: []string{"acme"}},
		{Name: "requested_repo_all", Value: uint8(1)},
		{Name: "requested_repo_slugs", Value: []string{}},
		{Name: "requested_repo_owners", Value: []string{}},
	}
	if !reflect.DeepEqual(rendered.Bindings, wantBindings) {
		t.Fatalf("Bindings = %#v, want %#v", rendered.Bindings, wantBindings)
	}
}

func TestWorkItemScopeSQLSelectorModeHasOneClosedRenderingForAllContentReaders(t *testing.T) {
	t.Parallel()
	requested := readers.RepositorySelectorSet{ExactSlugs: []string{"acme/widgets"}}
	scope := readers.AuthorizationScope{
		RepositorySelectors: &readers.RepositorySelectorScope{
			Granted:   readers.RepositorySelectorSet{Owners: []string{"acme"}},
			Requested: &requested,
		},
	}
	checks := []struct {
		name string
		read func(*fakeClient) error
	}{
		{
			name: "status",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemStatusWithScope(context.Background(), client, "org-1", []string{"repo-a:WI-1"}, scope, readers.Settings{})
				return err
			},
		},
		{
			name: "title",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemTitleWithScope(context.Background(), client, "org-1", []string{"repo-a:WI-1"}, scope, readers.Settings{})
				return err
			},
		},
		{
			name: "completion",
			read: func(client *fakeClient) error {
				_, err := readers.ReadWorkItemCompletionWithScope(context.Background(), client, "org-1", []string{"repo-a:WI-1"}, readers.TimeBound{}, scope, readers.Settings{})
				return err
			},
		},
	}

	want := readers.WorkItemScopeSQL(scope)
	for _, check := range checks {
		check := check
		t.Run(check.name, func(t *testing.T) {
			client := &fakeClient{}
			if err := check.read(client); err != nil {
				t.Fatalf("reader error = %v", err)
			}
			if len(client.queries) != 1 {
				t.Fatalf("queries = %d, want one query", len(client.queries))
			}
			query := client.queries[0]
			if !strings.Contains(query.statement, want.JoinSQL) {
				t.Fatalf("statement = %q, want fixed repository join %q", query.statement, want.JoinSQL)
			}
			if !strings.Contains(query.statement, "AND ("+want.AuthorizationExpr+")") {
				t.Fatalf("statement = %q, want the closed authorization expression in WHERE", query.statement)
			}
			if !reflect.DeepEqual(query.bindings[2:], want.Bindings) {
				t.Fatalf("scope bindings = %#v, want %#v", query.bindings[2:], want.Bindings)
			}
		})
	}
}

func TestWorkItemScopeSQLSelectorAndIDDimensionsAreANDed(t *testing.T) {
	t.Parallel()
	scope := readers.AuthorizationScope{
		GrantedRepositoryIDs: []string{"repo-a", "repo-b"},
		RepositorySelectors: &readers.RepositorySelectorScope{
			Granted: readers.RepositorySelectorSet{ExactSlugs: []string{"acme/widgets"}},
		},
	}
	rendered := readers.WorkItemScopeSQL(scope)
	if !strings.Contains(rendered.AuthorizationExpr, "toString(w.repo_id) IN {authorized_repo_ids:Array(String)} AND ") {
		t.Fatalf("AuthorizationExpr = %q, want ID predicate before selector predicate", rendered.AuthorizationExpr)
	}
	selectorStart := strings.Index(rendered.AuthorizationExpr, " AND (")
	if selectorStart < 0 || strings.Contains(rendered.AuthorizationExpr[:selectorStart], " OR ") {
		t.Fatalf("AuthorizationExpr = %q, ID and selector dimensions must be ANDed", rendered.AuthorizationExpr)
	}
	if got := rendered.Bindings[0]; got.Name != "authorized_repo_ids" || !reflect.DeepEqual(got.Value, []string{"repo-a", "repo-b"}) {
		t.Fatalf("first binding = %#v, want authorized IDs", got)
	}
}

func TestWorkItemScopeSQLNormalizesAndRejectsSelectorValues(t *testing.T) {
	t.Parallel()
	scope := readers.AuthorizationScope{
		RepositorySelectors: &readers.RepositorySelectorScope{
			Granted: readers.RepositorySelectorSet{
				ExactSlugs: []string{" ACME/Repo ", "acme/repo", "acme/not/real", "bad"},
				Owners:     []string{" zeta ", " ACME/* ", "acme", "bad/owner", ""},
			},
		},
	}
	rendered := readers.WorkItemScopeSQL(scope)
	want := []readers.Binding{
		{Name: "authorized_repo_all", Value: uint8(0)},
		{Name: "authorized_repo_slugs", Value: []string{"acme/repo"}},
		{Name: "authorized_repo_owners", Value: []string{"acme", "zeta"}},
	}
	if got := rendered.Bindings; !reflect.DeepEqual(got, want) {
		t.Fatalf("Bindings = %#v, want normalized valid selector bindings %#v", got, want)
	}
}

func TestWorkItemScopeSQLRequestedSelectorPresenceAndSettings(t *testing.T) {
	t.Parallel()
	requested := readers.RepositorySelectorSet{}
	scope := readers.AuthorizationScope{
		RepositorySelectors: &readers.RepositorySelectorScope{
			Granted:   readers.RepositorySelectorSet{All: true},
			Requested: &requested,
		},
	}
	rendered := readers.WorkItemScopeSQL(scope)
	if !strings.Contains(rendered.AuthorizationExpr, "{requested_repo_all:UInt8} = 1") {
		t.Fatalf("AuthorizationExpr = %q, want explicit requested selector gate", rendered.AuthorizationExpr)
	}
	if got := rendered.Bindings[len(rendered.Bindings)-3:]; !reflect.DeepEqual(got, []readers.Binding{
		{Name: "requested_repo_all", Value: uint8(0)},
		{Name: "requested_repo_slugs", Value: []string{}},
		{Name: "requested_repo_owners", Value: []string{}},
	}) {
		t.Fatalf("requested bindings = %#v, want explicit zero-request bindings", got)
	}

	client := &fakeClient{}
	settings := readers.Settings{MaxRowsToRead: 1000, MaxResultRows: 5}
	if _, err := readers.ReadWorkItemStatusWithScope(context.Background(), client, "org-1", []string{"repo-a:WI-1"}, scope, settings); err != nil {
		t.Fatalf("selector reader error = %v", err)
	}
	if got := client.queries[0].statement; !strings.HasSuffix(got, settings.Render()) {
		t.Fatalf("statement = %q, want selector reader to retain SETTINGS %q", got, settings.Render())
	}
}

func TestWorkItemScopeSQLZeroAndRequestedWildcardBindings(t *testing.T) {
	t.Parallel()
	requested := readers.RepositorySelectorSet{All: true}
	rendered := readers.WorkItemScopeSQL(readers.AuthorizationScope{
		RepositorySelectors: &readers.RepositorySelectorScope{Requested: &requested},
	})
	if rendered.JoinSQL == "" {
		t.Fatal("selector mode must expose the fixed repos join")
	}
	for _, binding := range rendered.Bindings {
		switch binding.Name {
		case "authorized_repo_all":
			if binding.Value != uint8(0) {
				t.Fatalf("authorized_repo_all = %#v, want zero-grant flag", binding.Value)
			}
		case "requested_repo_all":
			if binding.Value != uint8(1) {
				t.Fatalf("requested_repo_all = %#v, want explicit wildcard flag", binding.Value)
			}
		}
	}
	if !strings.Contains(rendered.AuthorizationExpr, "toString(w.repo_id) != '00000000-0000-0000-0000-000000000000'") {
		t.Fatalf("AuthorizationExpr = %q, explicit requested wildcard must reject zero repo IDs", rendered.AuthorizationExpr)
	}
}
