package latestrow_test

import (
	"testing"

	"github.com/full-chaos/dev-health-go/latestrow"
)

// opsOrderKeyAtMove is dev-health-ops internal/storage/clickhouse/latestrow
// OrderKey at 9ebae224d56b (latestrow.go:23), the text this package took over.
// A change of OrderKey must change this pin in the same PR, and it ships as a
// minor tag that ops and acr both bump to.
const opsOrderKeyAtMove = "(work_unit_investments.computed_at, toUInt64OrZero(splitByChar('_', work_unit_investments._part)[3]), work_unit_investments._part_offset)"

func TestOrderKeyIsPinned(t *testing.T) {
	if latestrow.OrderKey != opsOrderKeyAtMove {
		t.Fatalf("OrderKey = %q, want the pinned key %q", latestrow.OrderKey, opsOrderKeyAtMove)
	}
}

func TestOrderKeyForWorkUnitInvestmentsIsOrderKey(t *testing.T) {
	if got := latestrow.OrderKeyFor("work_unit_investments"); got != latestrow.OrderKey {
		t.Errorf("OrderKeyFor(work_unit_investments) = %q, want OrderKey %q", got, latestrow.OrderKey)
	}
	if got, want := latestrow.ArgMaxFor("work_unit_investments", "from_ts"), latestrow.ArgMax("from_ts"); got != want {
		t.Errorf("ArgMaxFor(work_unit_investments, from_ts) = %q, want ArgMax %q", got, want)
	}
}

func TestOrderKeyForIssueSizeIsPinned(t *testing.T) {
	const want = "(work_unit_issue_size.computed_at, toUInt64OrZero(splitByChar('_', work_unit_issue_size._part)[3]), work_unit_issue_size._part_offset)"
	if got := latestrow.OrderKeyFor("work_unit_issue_size"); got != want {
		t.Errorf("OrderKeyFor(work_unit_issue_size) = %q, want %q", got, want)
	}
	if got := latestrow.ArgMaxFor("work_unit_issue_size", "issue_size"); got != "argMax(issue_size, "+want+")" {
		t.Errorf("ArgMaxFor = %q", got)
	}
}

func TestRenderers(t *testing.T) {
	cases := []struct {
		name, got, want string
	}{
		{"ArgMax", latestrow.ArgMax("from_ts"), "argMax(from_ts, " + opsOrderKeyAtMove + ")"},
		{"ArgMaxKeepNull", latestrow.ArgMaxKeepNull("repo_id"), "(argMax(tuple(repo_id), " + opsOrderKeyAtMove + ")).1"},
		{"ArgMaxKeepNullBy", latestrow.ArgMaxKeepNullBy("team_id", "computed_at"), "(argMax(tuple(team_id), tuple(computed_at, ifNull(toString(team_id), ''))).1)"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
