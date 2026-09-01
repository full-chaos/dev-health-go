package readers_test

import (
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

// CHAOS-4552. Before this change, ProjectOwnershipJoinSQL's rendered SQL
// embedded a full copy of ProjectIdentityJoinSQL's text in EACH of its two
// arms, and that text itself scans `projects FINAL` twice (id-row, key-row
// branches) -- four physical scans per read, measured (EXPLAIN PLAN, real
// ClickHouse 26.7.5.10, seeded data so the optimizer could not fold an
// empty table into ReadNothing): four `ReadFromMergeTree(default.projects)`
// nodes.
//
// This pins the textual count as a cheap proxy for the physical one -- the
// two are 1:1 here because ClickHouse's planner does not share scans
// across textually-independent occurrences of the same subquery (verified
// against the real EXPLAIN PLAN when this change was made; not re-verified
// by this test, which is deliberately static and needs no server).
//
// It reached 2, not 1, when this test was written: the id-row/key-row
// doubling inside ProjectIdentityJoinSQL's own row expansion was untouched
// then, and was filed as the separate, riskier restructure it turned out to
// be. That restructure has since landed (CHAOS-4751 -- the expansion reads
// its row source once and fans the scope rows out with ARRAY JOIN), so the
// expected count here is now 1. The assertion this test exists to make --
// that the ownership side is unioned ONCE and joined against a SINGLE copy
// of the identity expansion -- is unchanged; only the number the identity
// expansion contributes moved, and TestChaos4751_IdentityExpansionScansProjectsOnce
// is what pins that half.
func TestChaos4552_OwnershipJoinScansProjectsOnce(t *testing.T) {
	t.Parallel()
	stmt := readers.ProjectOwnershipJoinSQL(readers.OwnershipValidityPredicate(readers.TimeBound{}))
	if want, got := 1, strings.Count(stmt, "FROM projects FINAL"); got != want {
		t.Fatalf("FROM projects FINAL count = %d, want %d (four before the ownership union; two after it while the identity expansion still spelled its row source twice; one now that the expansion fans out from a single read)\n%s", got, want, stmt)
	}
	// The identity expansion's own JOIN ON predicate (o.scope_value =
	// p.scope) must appear exactly once too -- the whole point of the
	// change. A second occurrence would mean the union-once restructure
	// regressed back toward two separate arms each joining their own copy
	// of the identity expansion.
	if want, got := 1, strings.Count(stmt, "o.scope_value = p.scope"); got != want {
		t.Errorf("o.scope_value = p.scope JOIN count = %d, want %d -- the ownership union must be joined against the identity expansion once, not once per arm", got, want)
	}
}

// The scope-kind restriction's OR (chaos4542_scope_kind_test.go's
// TestChaos4542_KeyArmSelectsTheKeyScopeRow pins that the restriction
// itself exists and is enforced) must live in a WHERE, never a JOIN ON --
// 24.8 rejects an ON that is not a plain column equality. This is the one
// property that test, being `package readers` and free to assert on
// fragments, does not itself check against the assembled ON clauses.
func TestChaos4552_ScopeKindGuardNeverEntersAnONClause(t *testing.T) {
	t.Parallel()
	stmt := readers.ProjectOwnershipJoinSQL(readers.OwnershipValidityPredicate(readers.TimeBound{}))
	for _, on := range strings.Split(stmt, " ON ")[1:] {
		clause := strings.SplitN(on, "\n", 2)[0]
		if strings.Contains(clause, "required_scope_kind") || strings.Contains(clause, " OR ") {
			t.Errorf("a JOIN ON condition is not a plain equality (%q) -- 24.8 rejects this with Code: 403", clause)
		}
	}
}
