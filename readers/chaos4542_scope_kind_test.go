package readers

import (
	"regexp"
	"strings"
	"testing"
)

// CHAOS-4542 defect 6, and the re-plan that ends the class.
//
// v0.5.4 made key_resolution_count correct PER SCOPE ROW -- an id match is
// unambiguous by construction, so its count is 1. That was right, and it
// silently broke the only other reader of the column. The ownership key arm
// guarded itself with `p.key_resolution_count = 1` while joining on
// `p.project_key`, a column present on EVERY scope row: so an ID row, which
// carries the project's key and now carries a count of 1, satisfied a guard
// written to mean "this key names exactly one project". Two projects sharing
// a key both matched an ownership row that named neither, and the owning
// team's metrics were attributed to both.
//
// Under v0.5.3 the id row happened to carry the project-level count and
// blocked it -- accidentally correct, for a reason nobody had written down.
//
// The column was being asked two different questions by two different arms.
// v0.5.5 stops asking: the ambiguity filter moves INSIDE the expansion, so an
// ambiguous key has no scope row at all and no consumer can join one, and
// scope_kind says which question a row answers. key_resolution_count survives
// only as telemetry.
//
// These are structural assertions on purpose. dev-health-go has no ClickHouse
// harness, so the behavioural proof -- two projects, one key, an ownership row
// naming neither, ZERO attribution -- lives in acr's
// TestCHAOS4347WideningAgainstRealClickHouse, which is red at v0.5.4 and must
// go green on this pin. A statement test cannot prove attribution; it can
// prove that the shape which produced the misattribution is gone.

// keyResolutionCountPredicate finds the column used as a guard rather than
// reported as telemetry: any comparison or equality against it.
var keyResolutionCountPredicate = regexp.MustCompile(`key_resolution_count\s*(=|!=|<|>|<=|>=)`)

func TestChaos4542_NoConsumerGuardsOnKeyResolutionCount(t *testing.T) {
	t.Parallel()
	// The expansion itself is where the filter belongs -- that is the fix,
	// not a violation -- so it is checked separately below. Every OTHER
	// statement must be free of it.
	for name, statement := range map[string]string{
		"ProjectOwnershipJoinSQL":   ProjectOwnershipJoinSQL(""),
		"ProjectIdentityJoinSQL":    ProjectIdentityJoinSQL(),
		"ProjectIdentityCatalogSQL": ProjectIdentityCatalogSQL(),
		"ProjectIdentityMatchSQL":   ProjectIdentityMatchSQL("o", "project_id"),
	} {
		outside := statementOutsideExpansion(statement)
		if keyResolutionCountPredicate.MatchString(outside) {
			t.Errorf("%s guards on key_resolution_count outside the expansion: it is TELEMETRY, and a consumer that reads it as a guard reads a per-scope-row number as if it described a key -- the defect 6 shape", name)
		}
	}
}

// statementOutsideExpansion strips the one place the ambiguity filter is
// allowed to live, so the assertion above cannot be satisfied by accident.
func statementOutsideExpansion(statement string) string {
	const marker = "WHERE project_key != '' AND key_resolution_count = 1"
	return strings.ReplaceAll(statement, marker, "")
}

func TestChaos4542_AmbiguousKeyHasNoScopeRow(t *testing.T) {
	t.Parallel()
	expansion := ProjectIdentityCatalogSQL()
	// The filter must be INSIDE the expansion, on the key row's own SELECT.
	// That is what makes an ambiguous key unjoinable by anyone: it is not a
	// guard a consumer may forget, it is a row that does not exist.
	if !strings.Contains(expansion, "WHERE project_key != '' AND key_resolution_count = 1") {
		t.Fatal("the key scope row must be emitted only when its key names exactly one project -- otherwise every consumer has to remember a guard, and the last three did not")
	}
	if !strings.Contains(expansion, "'id' AS scope_kind") || !strings.Contains(expansion, "'key' AS scope_kind") {
		t.Fatal("the expansion must label each scope row with the question it answers ('id' or 'key')")
	}
}

func TestChaos4542_KeyArmSelectsTheKeyScopeRow(t *testing.T) {
	t.Parallel()
	ownership := ProjectOwnershipJoinSQL("")
	// The key arm must join the KEY scope row -- p.scope, restricted by
	// kind -- never p.project_key, which every scope row carries and which
	// is exactly how an ID row satisfied a key-shaped guard.
	if strings.Contains(ownership, "o.project_key = p.project_key") {
		t.Error("the key arm joins p.project_key, a column present on EVERY scope row: an id row carrying the same key matches, which is defect 6")
	}
	if !strings.Contains(ownership, "o.project_key = p.scope") {
		t.Error("the key arm must match the key SCOPE row (o.project_key = p.scope)")
	}
	if !strings.Contains(ownership, "p.scope_kind = 'key'") {
		t.Error("the key arm must name scope_kind = 'key' -- without it the arm matches id rows too")
	}
}

// The SCOPE arm must carry NO kind restriction, and this is a real guard
// rather than a symmetry: the column it compares (project_id,
// work_scope_id) is not an id column, it is whichever id space that row
// happens to use, and today's GitLab rows carry a project KEY there.
// Restricting it to scope_kind = 'id' reads like tightening and is the
// key-to-key arm being dropped a fourth time.
//
// It is safe unrestricted only because an ambiguous key has no scope row at
// all, which the two tests above pin.
func TestChaos4542_ScopeArmCarriesNoKindRestriction(t *testing.T) {
	t.Parallel()
	if match := ProjectIdentityMatchSQL("o", "project_id"); strings.Contains(match, "scope_kind") {
		t.Errorf("the scope arm names scope_kind (%q): project_id/work_scope_id may hold EITHER id space, so a kind restriction drops the GitLab key-shaped rows", match)
	}
}

// scope_kind must NOT join the identity grouping (codex R1 on the scope_kind
// change, confirmed).
//
// The two UNION branches produce the SAME scope row for a project whose id
// equals its project_key, and the GROUP BY exists to collapse exactly that.
// Grouping by scope_kind makes those rows differ, so both survive and the
// scope arm matches both. ReadProjectWorkload and ReadProjectReadiness have
// no outer GROUP BY, so each matching source row returns twice and burns
// DefaultRowLimit at double rate -- silently truncating other projects out of
// the answer.
//
// A duplicate that costs the caller OTHER rows is the same failure mode the
// resolved-grain collapse in ProjectOwnershipJoinSQL was added to prevent, so
// it gets its own guard rather than a comment.
func TestChaos4542_ScopeKindDoesNotSplitTheIdentityGrouping(t *testing.T) {
	t.Parallel()
	expansion := ProjectIdentityCatalogSQL()
	if strings.Contains(expansion, "GROUP BY provider, id, project_key, key_resolution_count, scope, scope_kind") {
		t.Error("scope_kind is in the GROUP BY: a project whose id equals its project_key now yields TWO scope rows, and the scope arm matches both")
	}
	if !strings.Contains(expansion, "max(scope_kind) AS scope_kind") {
		t.Error("scope_kind must be aggregated so the discriminator survives without splitting the identity")
	}
}
