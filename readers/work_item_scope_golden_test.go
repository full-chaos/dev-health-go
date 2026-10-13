package readers

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateWorkItemScopeGolden = flag.Bool("update-work-item-scope-golden", false, "rewrite testdata/work_item_scope_sql.golden.json")

func workItemScopeGoldenScopes() map[string]AuthorizationScope {
	requested := RepositorySelectorSet{ExactSlugs: []string{"acme/one"}}
	return map[string]AuthorizationScope{
		"zero":             {},
		"id_only":          {GrantedRepositoryIDs: []string{"r1"}, RequestedRepositoryIDs: []string{"r2"}},
		"selector_all":     {RepositorySelectors: &RepositorySelectorScope{Granted: RepositorySelectorSet{All: true}}},
		"selector_exact":   {RepositorySelectors: &RepositorySelectorScope{Granted: RepositorySelectorSet{ExactSlugs: []string{"Acme/One"}}}},
		"selector_owner":   {RepositorySelectors: &RepositorySelectorScope{Granted: RepositorySelectorSet{Owners: []string{"acme/*"}}}},
		"selector_empty":   {RepositorySelectors: &RepositorySelectorScope{}},
		"selector_request": {GrantedRepositoryIDs: []string{"r1"}, RepositorySelectors: &RepositorySelectorScope{Granted: RepositorySelectorSet{Owners: []string{"acme"}}, Requested: &requested}},
	}
}

// TestWorkItemScopeSQLGolden pins every statement a caller without
// candidates receives to the bytes it received before candidates existed,
// and pins the zero WorkItemCandidates to the same bytes.
func TestWorkItemScopeSQLGolden(t *testing.T) {
	got := map[string]WorkItemScopeSQLResult{}
	for name, scope := range workItemScopeGoldenScopes() {
		got[name] = WorkItemScopeSQL(scope)
		among, err := json.Marshal(WorkItemScopeSQLAmong(scope, WorkItemCandidates{}))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := json.Marshal(got[name])
		if err != nil {
			t.Fatal(err)
		}
		if string(among) != string(plain) {
			t.Errorf("%s: WorkItemScopeSQLAmong with zero candidates differs from WorkItemScopeSQL", name)
		}
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/work_item_scope_sql.golden.json"
	if *updateWorkItemScopeGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(encoded)+"\n" {
		t.Fatalf("WorkItemScopeSQL output differs from %s", path)
	}
}
