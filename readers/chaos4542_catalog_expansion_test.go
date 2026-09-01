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
		"project_key != '' AND key_resolution_count = 1",                                               // the key scope row
		"countIf(ifNull(project_key, '') != '') OVER (PARTITION BY provider, ifNull(project_key, ''))", // org-wide ambiguity window, empty keys excluded
		// RESPELLED by CHAOS-4751, not relaxed: the two identity values
		// used to be two UNION branches ("id AS scope", "project_key AS
		// scope") and are now the two positions of one fan-out array. One
		// literal pins both, AND pins that the id value is unconditional
		// while the key value appears only in the guarded arm -- which the
		// two separate substrings could not distinguish.
		"if(key_scope_emitted, [id, project_key], [id]) AS scope",
	} {
		if !strings.Contains(catalog, guard) {
			t.Errorf("the catalog expansion lost %q\n%s", guard, catalog)
		}
	}
}

// CHAOS-4542, codex R1 P1. key_resolution_count is a property of the SCOPE
// ROW, not of the project, and conflating the two shipped a fix that fixed
// nothing.
//
// Every real Linear project carries project_key NULL. They therefore all
// share the empty-key partition, so a project-level count came back as
// "however many NULL-key projects this org has" -- 17 on the org this was
// measured against -- and rode along on the project's ID row too. A
// consumer that gates on `key_resolution_count > 1`, as
// devhealthsource's queryProjectTeams does, then discards a perfectly
// unambiguous `project_id = projects.id` match. That producer still emitted
// zero Linear edges after being "fixed", and 13 green integration subtests
// missed it, because every fixture seeds projects WITH keys.
//
// Two properties make the trap unavailable rather than merely documented.
func TestChaos4542_KeyResolutionCountIsPerScopeRow(t *testing.T) {
	t.Parallel()
	for name, expansion := range map[string]string{
		"catalog":  readers.ProjectIdentityCatalogSQL(),
		"filtered": readers.ProjectIdentityJoinSQL(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// 1. The ID row's count is the literal 1. projects.id is unique,
			//    so an id match is unambiguous BY CONSTRUCTION and no
			//    partition count can say otherwise.
			// RESPELLED by CHAOS-4751, and stronger for it. The id row's
			// count and the key row's used to live in two UNION branches, so
			// this could only pin the id one; they are now the two positions
			// of a single array, and this pins BOTH at once -- the id
			// position is the literal 1, the key position the project's own
			// partition count. The names differ on purpose: the scope row's
			// number is key_resolution_count, the project's is
			// project_key_resolution_count.
			if !strings.Contains(expansion, "if(key_scope_emitted, [toUInt64(1), project_key_resolution_count], [toUInt64(1)]) AS key_resolution_count") {
				t.Errorf("the id scope row does not carry a literal count of 1; a project-level count would gate an unambiguous id match\n%s", expansion)
			}
			// 2. The ambiguity window EXCLUDES empty keys. An empty key is
			//    never a match candidate -- the key row is guarded on
			//    `project_key != ''` -- so counting empty keys can only
			//    produce a number that describes nothing.
			if !strings.Contains(expansion, "countIf(ifNull(project_key, '') != '') OVER") {
				t.Errorf("the ambiguity window still counts empty keys; NULL-key projects would inflate each other's counts\n%s", expansion)
			}
			if strings.Contains(expansion, "count() OVER (PARTITION BY provider, project_key)") {
				t.Errorf("the unqualified count() window is back; it counts empty keys\n%s", expansion)
			}
		})
	}
}
