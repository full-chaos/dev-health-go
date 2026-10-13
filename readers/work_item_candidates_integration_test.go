//go:build integration

package readers_test

import (
	"reflect"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

type authzCandidateScope struct {
	name  string
	scope readers.AuthorizationScope
}

func authzCandidateScopes() []authzCandidateScope {
	requestedGranted := readers.RepositorySelectorSet{ExactSlugs: []string{"acme/granted"}}
	var scopes []authzCandidateScope
	for _, grant := range authzGrants {
		scopes = append(scopes, authzCandidateScope{grant.name, readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: grant.set}}})
	}
	return append(scopes,
		authzCandidateScope{"matching-exact+requested-granted", readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: authzGrants[1].set, Requested: &requestedGranted}}},
		authzCandidateScope{"all+requested-granted", readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{Granted: authzGrants[0].set, Requested: &requestedGranted}}},
	)
}

// authzWithoutCandidatePaths is the row a work item outside the candidate
// set must read: no link aggregate joins it, so its link path is false and
// its excluded links are empty. The project aggregate is per project, so it
// still joins when a candidate item names the same provider and project
// (projectKept) and is otherwise gone too. Only the organization grant, the
// item's own repository or a kept project can admit it.
func authzWithoutCandidatePaths(row authzReadRow, projectKept bool) authzReadRow {
	out := authzReadRow{paths: map[string]bool{}, evidence: map[string][]string{}}
	for path, ok := range row.paths {
		out.paths[path] = ok
	}
	for path, repos := range row.evidence {
		out.evidence[path] = repos
	}
	dropped := []string{readers.WorkItemAuthorizationPullRequestLink}
	if !projectKept {
		dropped = append(dropped, readers.WorkItemAuthorizationProjectOwnership)
	}
	for _, path := range dropped {
		out.paths[path] = false
		delete(out.evidence, path)
	}
	out.authorized = false
	for _, ok := range out.paths {
		out.authorized = out.authorized || ok
	}
	return out
}

func authzSameRow(a, b authzReadRow) bool {
	if a.authorized != b.authorized || !reflect.DeepEqual(a.paths, b.paths) || !reflect.DeepEqual(a.evidence, b.evidence) {
		return false
	}
	return len(a.excluded) == 0 && len(b.excluded) == 0 || reflect.DeepEqual(a.excluded, b.excluded)
}

// TestIntegrationWorkItemCandidatesDifferential runs the unnarrowed
// relation and the candidate-narrowed one over the same seeded rows for
// every principal, and requires the same authorization, paths, evidence
// and excluded links for every row the candidate set holds; a candidate set
// with ids the statement never reads changes nothing, and an item left out
// of the candidate set loses only its repo-less paths.
func TestIntegrationWorkItemCandidatesDifferential(t *testing.T) {
	items := authzItems()
	client := authzDatabase(t, items)
	ids := authzIDs(items)

	var workItemIDs, kept []string
	dropped := map[string]bool{}
	for i, item := range items {
		workItemIDs = append(workItemIDs, item.id)
		if i%2 == 0 {
			kept = append(kept, item.id)
		} else {
			dropped[item.id] = true
		}
	}
	superset := append(append([]string(nil), workItemIDs...), "linear:never-seeded", "jira:never-seeded", "proj-owned", "KEYED")

	type variant struct {
		name       string
		candidates readers.WorkItemCandidates
		dropped    map[string]bool
	}
	variants := []variant{
		{"exact-ids", readers.WorkItemCandidateIDs(workItemIDs), nil},
		{"superset-ids", readers.WorkItemCandidateIDs(superset), nil},
		{"subquery", readers.WorkItemCandidateSubquery("SELECT work_item_id FROM work_items WHERE org_id = {org_id:String} AND provider IN {candidate_providers:Array(String)}",
			readers.Binding{Name: "candidate_providers", Value: []string{"linear", "jira"}}), nil},
		{"subquery-every-org", readers.WorkItemCandidateSubquery("SELECT work_item_id FROM work_graph_issue_pr UNION ALL SELECT work_item_id FROM work_items"), nil},
		{"half-ids", readers.WorkItemCandidateIDs(kept), dropped},
		{"no-ids", readers.WorkItemCandidateIDs(nil), map[string]bool{}},
	}

	compared, denied := 0, 0
	for _, sc := range authzCandidateScopes() {
		t.Run(sc.name, func(t *testing.T) {
			base := authzReadRendered(t, client, readers.WorkItemScopeSQL(sc.scope), ids)
			if len(base) != len(items) {
				t.Fatalf("unnarrowed relation returned %d rows, want %d", len(base), len(items))
			}
			for _, v := range variants {
				got := authzReadRendered(t, client, readers.WorkItemScopeSQLAmong(sc.scope, v.candidates), ids)
				if len(got) != len(items) {
					t.Fatalf("%s: narrowed relation returned %d rows, want %d", v.name, len(got), len(items))
				}
				candidateProjects := map[[2]string]bool{}
				for _, item := range items {
					if v.dropped != nil && (len(v.dropped) == 0 || v.dropped[item.id]) {
						continue
					}
					candidateProjects[[2]string{item.provider, item.projectID}] = true
				}
				for _, item := range items {
					want := base[item.id]
					outside := v.dropped != nil && (len(v.dropped) == 0 || v.dropped[item.id])
					if outside {
						want = authzWithoutCandidatePaths(want, candidateProjects[[2]string{item.provider, item.projectID}])
						if base[item.id].authorized && !want.authorized {
							denied++
						}
					}
					if !authzSameRow(got[item.id], want) {
						t.Errorf("%s: %q = %+v, want %+v", v.name, item.id, got[item.id], want)
					}
					compared++
				}
			}
		})
	}
	if want := len(authzCandidateScopes()) * len(variants) * len(items); compared != want {
		t.Fatalf("compared %d rows, want %d", compared, want)
	}
	if denied == 0 {
		t.Fatal("no row admitted only through a repo-less path was left out of a candidate set: the deny direction was never measured")
	}
	t.Logf("compared %d rows; %d admitted rows left out of a candidate set read denied", compared, denied)
}
