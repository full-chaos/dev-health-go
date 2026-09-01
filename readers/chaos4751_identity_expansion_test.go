package readers_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// CHAOS-4751, the second half of CHAOS-4552.
//
// CHAOS-4552 halved the rendered ownership statement from four physical
// `projects FINAL` scans to two by unioning the OWNERSHIP side once. The two
// that remained were inside the identity expansion itself: its id-row and
// key-row branches were a UNION ALL over the row source spelled TWICE, so
// the row source -- and the `projects FINAL` read at the bottom of it -- was
// rendered as two textually independent subqueries. ClickHouse's planner
// does not share scans across those, so two texts meant two reads.
//
// This collapses them to one read fanned out by ARRAY JOIN: the row source
// is read once, and each project emits its one or two scope rows from that
// single read.
//
// These pins are STATIC and need no server, the same deliberate choice
// TestChaos4552_OwnershipJoinScansProjectsOnce documents: the textual count
// is a 1:1 proxy for the physical one here, verified against the real
// EXPLAIN PLAN when this change was made (real ClickHouse 26.7, seeded with
// one project and one ownership row so the optimizer could not fold an
// empty table into ReadNothing) and captured in the PR rather than re-run
// by this test.

// expansionFanOut is the ARRAY JOIN that replaced the two-branch UNION ALL.
// Each element pairs the id scope row's value with the key scope row's, in
// that order, under ONE shared condition.
const (
	fanOutScope     = "if(key_scope_emitted, [id, project_key], [id]) AS scope"
	fanOutScopeKind = "if(key_scope_emitted, ['id', 'key'], ['id']) AS scope_kind"
	fanOutCount     = "if(key_scope_emitted, [toUInt64(1), project_key_resolution_count], [toUInt64(1)]) AS key_resolution_count"

	// The key scope row's guard, unchanged in meaning and now the ONE place
	// the ambiguity filter lives. Parenthesised because ClickHouse binds AS
	// to the last operand of an unparenthesised AND chain.
	keyScopeGuard = "(project_key != '' AND key_resolution_count = 1) AS key_scope_emitted"
)

// identityExpansions is every form of the shared fragment. Both must hold
// every property below -- the catalog form is the one a new caller reaches
// for, and the filtered form is what the project rollups embed.
func identityExpansions() map[string]string {
	return map[string]string{
		"filtered": readers.ProjectIdentityJoinSQL(),
		"catalog":  readers.ProjectIdentityCatalogSQL(),
	}
}

// The ticket, pinned. TWO on the parent commit, ONE here.
func TestChaos4751_IdentityExpansionScansProjectsOnce(t *testing.T) {
	t.Parallel()
	for name, expansion := range identityExpansions() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if want, got := 1, strings.Count(expansion, "FROM projects FINAL"); got != want {
				t.Fatalf("FROM projects FINAL count = %d, want %d -- the id-row and key-row branches must be fanned out from ONE read of the row source, not a UNION ALL over two copies of its text\n%s", got, want, expansion)
			}
			// The row source is spelled once, so its org-wide ambiguity
			// window is too. Two copies would also mean two window
			// computations over the whole org, not just two scans.
			if want, got := 1, strings.Count(expansion, "countIf(ifNull(project_key, '') != '') OVER"); got != want {
				t.Errorf("ambiguity window count = %d, want %d -- a second copy means the org-wide window is computed twice as well", got, want)
			}
		})
	}
}

// And the whole assembled ownership statement therefore reaches one too --
// this is the number CHAOS-4552 could only take to two, and the reason that
// ticket left a follow-up rather than claiming the reduction.
func TestChaos4751_OwnershipJoinReachesASingleProjectsScan(t *testing.T) {
	t.Parallel()
	stmt := readers.ProjectOwnershipJoinSQL(readers.OwnershipValidityPredicate(readers.TimeBound{}))
	if want, got := 1, strings.Count(stmt, "FROM projects FINAL"); got != want {
		t.Fatalf("FROM projects FINAL count = %d, want %d (four before the ownership union, two after it, one now that the identity expansion fans out from a single read)\n%s", got, want, stmt)
	}
}

// The id scope row's count must stay the LITERAL 1, and the key scope row's
// must stay the real partition count. This is the invariant the fan-out is
// most able to destroy, because both values now live in one expression
// instead of two separate branches -- the id row's is the FIRST array
// element, the key row's the second.
//
// Blurring them is not hypothetical. projects.id is unique by construction,
// so an id match is unambiguous whatever any partition says; when the
// project-level number rode along on the id row instead, every real Linear
// project (project_key NULL, so all in one empty-key partition) carried
// "however many NULL-key projects this org has", and a consumer gating on
// that count discarded a perfectly unambiguous id match. That shipped.
//
// The two numbers now have two NAMES -- key_resolution_count is the scope
// row's, project_key_resolution_count is the project's -- so the confusion
// is visible at the point of use rather than described in a comment.
func TestChaos4751_IdRowCarriesTheLiteralCountNotThePartitionCount(t *testing.T) {
	t.Parallel()
	for name, expansion := range identityExpansions() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(expansion, fanOutCount) {
				t.Fatalf("the per-scope-row count is not %q -- the id position must be the literal toUInt64(1) and the key position the project's own partition count\n%s", fanOutCount, expansion)
			}
			// The project-level number must never be what a scope row
			// reports. It may only be READ (to build the guard and the key
			// row's value); it may not be the id row's value.
			if strings.Contains(expansion, "[project_key_resolution_count,") {
				t.Errorf("the project-level count is in the id position of the fan-out: an unambiguous id match would report the key partition's number, which is the defect that shipped\n%s", expansion)
			}
		})
	}
}

// The ambiguity filter still lives in exactly ONE place, and it is inside
// the expansion -- so an ambiguous key has no scope row at all rather than a
// row every consumer must remember to guard. Three consumers did not.
func TestChaos4751_TheKeyScopeGuardHasExactlyOneSite(t *testing.T) {
	t.Parallel()
	for name, expansion := range identityExpansions() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if want, got := 1, strings.Count(expansion, keyScopeGuard); got != want {
				t.Fatalf("key scope guard site count = %d, want %d (%q)\n%s", got, want, keyScopeGuard, expansion)
			}
		})
	}
}

// The three fan-out arrays are ARRAY JOIN-ed together, so ClickHouse zips
// them element-wise and REQUIRES equal lengths per row ("Sizes of
// ARRAY-JOIN-ed arrays do not match"). They are equal by construction --
// same condition, two elements when a key scope row is emitted and one when
// it is not -- and this pins that construction, because the failure mode if
// it ever drifts is a runtime exception on every read.
func TestChaos4751_ScopeFanOutArraysAreLengthMatched(t *testing.T) {
	t.Parallel()
	// Each element of these arrays is a literal or a bare column name, none
	// containing a top-level comma, so splitting on "," counts elements.
	fanOut := regexp.MustCompile(`if\(key_scope_emitted, \[([^\]]*)\], \[([^\]]*)\]\)`)
	for name, expansion := range identityExpansions() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			matches := fanOut.FindAllStringSubmatch(expansion, -1)
			if want, got := 3, len(matches); got != want {
				t.Fatalf("fan-out expressions on key_scope_emitted = %d, want %d (scope, scope_kind, key_resolution_count)\n%s", got, want, expansion)
			}
			for _, m := range matches {
				emitted, withheld := strings.Count(m[1], ",")+1, strings.Count(m[2], ",")+1
				if emitted != 2 || withheld != 1 {
					t.Errorf("fan-out %q has %d/%d elements, want 2/1 -- unequal ARRAY JOIN array lengths raise DB::Exception on every read", m[0], emitted, withheld)
				}
			}
		})
	}
}

// The expansion's OUTPUT PROJECTION is a cross-repo contract, not an
// internal detail. acr's project-teams edge producer embeds the catalog
// form as `FROM (SELECT * FROM <expansion>) AS pi`, so the column SET and
// their ORDER are load-bearing outside this repo: adding, removing or
// reordering a column silently changes that producer's row shape, and no
// test in this repo would see it.
//
// Pinned verbatim rather than as a set of Contains checks, because order is
// exactly what a Contains check cannot see.
func TestChaos4751_ExpansionOuterProjectionIsACrossRepoContract(t *testing.T) {
	t.Parallel()
	const projection = "SELECT provider, id, project_key, key_resolution_count, scope, max(scope_kind) AS scope_kind"
	for name, expansion := range identityExpansions() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(expansion, projection) {
				t.Fatalf("the expansion's outer projection changed; a consumer reading it through SELECT * gets a different row shape and nothing here would catch it\nwant: %s\ngot:\n%s", projection, expansion)
			}
			// And the fan-out must sit UNDER that projection, never beside
			// it -- the ARRAY JOIN's own aliases are the columns the
			// projection names, so they must not leak extra columns out.
			if strings.Contains(expansion, "project_key_resolution_count, scope, max") {
				t.Errorf("the project-level count leaked into the outer projection; consumers reading SELECT * would see a column that describes the project, not the scope row\n%s", expansion)
			}
		})
	}
}
