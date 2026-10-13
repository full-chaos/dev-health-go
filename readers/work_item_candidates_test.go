package readers

import (
	"reflect"
	"strings"
	"testing"
)

func TestWorkItemCandidateSubqueryRejectsReservedBindingNames(t *testing.T) {
	for _, name := range []string{"org_id", "ids", "authorized_repo_all", "requested_repo_slugs", "wia_candidate_work_item_ids"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("binding %q: want a panic", name)
				}
			}()
			WorkItemCandidateSubquery("SELECT work_item_id FROM work_items", Binding{Name: name, Value: "x"})
		}()
	}
	WorkItemCandidateSubquery("SELECT work_item_id FROM work_items WHERE team_id IN {team_ids:Array(String)}", Binding{Name: "team_ids", Value: []string{"t"}})
}

func TestWorkItemScopeSQLAmongBindsCandidatesOnlyInSelectorMode(t *testing.T) {
	candidates := WorkItemCandidateIDs([]string{"linear:1"})

	idOnly := AuthorizationScope{GrantedRepositoryIDs: []string{"r1"}}
	if got, want := WorkItemScopeSQLAmong(idOnly, candidates), WorkItemScopeSQL(idOnly); !reflect.DeepEqual(got, want) {
		t.Errorf("ID-only scope: candidates changed the result: %+v, want %+v", got, want)
	}

	selector := AuthorizationScope{RepositorySelectors: &RepositorySelectorScope{Granted: RepositorySelectorSet{Owners: []string{"acme"}}}}
	got := WorkItemScopeSQLAmong(selector, candidates)
	bound := false
	for _, b := range got.Bindings {
		if b.Name == workItemCandidateIDsBinding && reflect.DeepEqual(b.Value, []string{"linear:1"}) {
			bound = true
		}
	}
	if !bound {
		t.Errorf("selector scope: bindings %+v do not carry the candidate ids", got.Bindings)
	}
	relation := "{" + workItemCandidateIDsBinding + ":Array(String)}"
	if n := strings.Count(got.JoinSQL, relation); n != 3 {
		t.Errorf("JoinSQL names the candidate set %d times, want 3 (link rows, project scopes, owned projects)", n)
	}
	plain := WorkItemScopeSQL(selector)
	if got.AuthorizationExpr != plain.AuthorizationExpr || !reflect.DeepEqual(got.Provenance, plain.Provenance) || got.ExcludedLinkProvenancesExpr != plain.ExcludedLinkProvenancesExpr {
		t.Error("candidates changed an authorization expression; only the aggregates may change")
	}
}

func TestWorkItemCandidateIDsNilIsAnEmptySet(t *testing.T) {
	got := WorkItemCandidateIDs(nil)
	if len(got.bindings) != 1 || got.bindings[0].Value == nil || len(got.bindings[0].Value.([]string)) != 0 {
		t.Fatalf("WorkItemCandidateIDs(nil) bindings = %+v, want one empty array", got.bindings)
	}
}
