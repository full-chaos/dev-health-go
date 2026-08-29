package readers_test

import (
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// CHAOS-4542. devhealthsource's queryProjectTeams walks the WHOLE project
// catalog, paginating by cursor; it has no requested-subject list. Reusing
// ProjectIdentityJoinSQL there failed on a real ClickHouse with
//
//	Code: 456. DB::Exception: Substitution `ids` is not set.
//
// because that form's row source ends `WHERE concat(provider, ':', id) IN
// {ids:Array(String)}` and the producer binds only org_id/since/after/
// row_limit. A design mismatch -- subject-filtered helper in a catalog
// walker -- not a syntax error, and not the JOIN-ON portability problem it
// was first mistaken for.
//
// The catalog form is the same expansion without that predicate. These
// pin the two properties that make it safe to have both.
func TestChaos4542_CatalogExpansionBindsNoSubjectFilter(t *testing.T) {
	t.Parallel()
	catalog := readers.ProjectIdentityCatalogSQL()

	// 1. It binds no `ids` parameter -- the actual failure, pinned. A
	//    caller with no subject list must be able to run it.
	if strings.Contains(catalog, "{ids:") {
		t.Errorf("the catalog expansion still references an ids parameter; a catalog walker binds none and would fail with Code: 456\n%s", catalog)
	}
	// 2. It still reads org-scoped. Dropping the subject filter must not
	//    drop the org boundary with it.
	if !strings.Contains(catalog, "org_id = {org_id:String}") {
		t.Errorf("the catalog expansion lost its org scoping\n%s", catalog)
	}
}

// The two forms must differ ONLY by the subject predicate. If they drift,
// "identity" comes to mean two different things depending on which caller
// asked -- and the guards (non-empty key, unambiguous key, the org-wide
// ambiguity window) are exactly what must not vary.
func TestChaos4542_TheFilteredFormIsTheCatalogFormPlusTheSubjectPredicate(t *testing.T) {
	t.Parallel()
	filtered := readers.ProjectIdentityJoinSQL()
	catalog := readers.ProjectIdentityCatalogSQL()

	const subjectPredicate = "\n\t\tWHERE concat(provider, ':', id) IN {ids:Array(String)}"
	if reduced := strings.ReplaceAll(filtered, subjectPredicate, ""); reduced != catalog {
		t.Errorf("the filtered form is not the catalog form plus the subject predicate; the two expansions have drifted\n--- filtered minus predicate ---\n%s\n--- catalog ---\n%s", reduced, catalog)
	}

	// And the guards survive in the catalog form specifically, since that
	// is the one a new caller will reach for.
	for _, guard := range []string{
		"project_key != '' AND key_resolution_count = 1",    // the key scope row
		"count() OVER (PARTITION BY provider, project_key)", // org-wide ambiguity window
		"id AS scope",
		"project_key AS scope",
	} {
		if !strings.Contains(catalog, guard) {
			t.Errorf("the catalog expansion lost %q\n%s", guard, catalog)
		}
	}
}
