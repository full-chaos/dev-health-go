package readers_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestSizingCoverageNoteIsOnlyBelowHalf(t *testing.T) {
	for _, tc := range []struct {
		sized, units int
		want         string
	}{
		{1, 4, "Mostly unsized: 1 of 4 work units have an issue size. The other 3 have no size, so each counts as 1 work unit."},
		{0, 3, "Mostly unsized: 0 of 3 work units have an issue size. The other 3 have no size, so each counts as 1 work unit."},
		{2, 5, "Mostly unsized: 2 of 5 work units have an issue size. The other 3 have no size, so each counts as 1 work unit."},
		{2, 4, ""},
		{3, 5, ""},
		{0, 0, ""},
	} {
		if got := (readers.SizingCoverage{SizedUnits: tc.sized, Units: tc.units}).Note(); got != tc.want {
			t.Errorf("Note(%d of %d) = %q, want %q", tc.sized, tc.units, got, tc.want)
		}
	}
}

func TestSizingCoverageNullAndZeroAreDistinct(t *testing.T) {
	none := readers.SizingCoverage{}
	if none.Share() != nil || none.Measured() {
		t.Errorf("a read with no unit must be not measured, got share %v", none.Share())
	}
	if u := none.Unsized(); u == nil || *u != 0 {
		t.Errorf("Unsized of an empty read = %v, want 0", u)
	}
	zero := readers.SizingCoverage{SizedUnits: 0, Units: 3}
	if s := zero.Share(); s == nil || *s != 0 || !zero.Measured() {
		t.Errorf("0 of 3 sized must be a measured 0, got %v", s)
	}
	if u := zero.Unsized(); *u != 3 {
		t.Errorf("Unsized(0 of 3) = %d, want 3", *u)
	}
	if none.Note() != "" {
		t.Errorf("a read with no unit serves no note, got %q", none.Note())
	}
}

func TestIssueSizedSourceKeepsTheSourceAndAddsTheTwoColumns(t *testing.T) {
	got, err := readers.IssueSizedSource("(SELECT 1) AS wui", "wui")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if(unit_size.issue_size > 0, unit_size.issue_size, 1.0) AS " + readers.UnitSizeWeightColumn,
		"if(unit_size.issue_size > 0, unit_size.issue_size, 0.0) AS " + readers.UnitIssueSizeColumn,
		"FROM (SELECT 1) AS sized_unit",
		"LEFT JOIN " + readers.LatestWorkUnitIssueSizeSource() + " AS unit_size",
		"unit_size.org_id = sized_unit.org_id",
		"unit_size.work_unit_id = sized_unit.work_unit_id",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sized source lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, ") AS wui") {
		t.Errorf("sized source must keep the caller's alias, got suffix %q", got[len(got)-12:])
	}
	if _, err := readers.IssueSizedSource("(SELECT 1)", "wui"); err == nil {
		t.Error("a source that does not end in its alias must be refused")
	}
}

func TestSizeExpressionsAreTheOnesTheSizedSourceUses(t *testing.T) {
	if got, want := readers.UnitSizeWeightExpr("u"), "if(u.issue_size > 0, u.issue_size, 1.0)"; got != want {
		t.Errorf("UnitSizeWeightExpr = %q, want %q", got, want)
	}
	if got, want := readers.UnitIssueSizeExpr("u"), "if(u.issue_size > 0, u.issue_size, 0.0)"; got != want {
		t.Errorf("UnitIssueSizeExpr = %q, want %q", got, want)
	}
}

func TestSizingCoverageSelectListAndScan(t *testing.T) {
	list := readers.SizingCoverageSelectList("work_unit_investments.work_unit_id")
	for _, want := range []string{
		"toUInt64(uniqExactIf(work_unit_investments.work_unit_id, unit_issue_size > 0)) AS sized_units",
		"toUInt64(uniqExact(work_unit_investments.work_unit_id)) AS units",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("select list lacks %q:\n%s", want, list)
		}
	}
	c, err := readers.ScanSizingCoverage(scanner{values: []any{uint64(2), uint64(5)}})
	if err != nil || c.SizedUnits != 2 || c.Units != 5 {
		t.Errorf("ScanSizingCoverage = %+v, %v", c, err)
	}
}

type scanner struct{ values []any }

func (s scanner) Scan(dest ...any) error {
	for i, d := range dest {
		*(d.(*uint64)) = s.values[i].(uint64)
	}
	return nil
}

// The statements are byte-equal to ops' analytics.IssueSizedSource and
// LatestWorkUnitIssueSizeSource at the time they moved here (ops main
// 1fb3e6c83c); a change to either must change these pins on purpose.
func TestIssueSizeSourceIsByteEqualToTheOpsSourceItMovedFrom(t *testing.T) {
	sized, err := readers.IssueSizedSource("(SELECT 1) AS wui", "wui")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ got, want string }{
		"IssueSizedSource":              {sized, "e5f4ba96ea9971b173642d5045b12f5aa86bd52413f424872612d7b64e6da9a2"},
		"LatestWorkUnitIssueSizeSource": {readers.LatestWorkUnitIssueSizeSource(), "d566882b1b38d74b0cec0efec07c7a1fd5389448c4704858c4277751f2535cc1"},
	} {
		sum := sha256.Sum256([]byte(c.got))
		if hex.EncodeToString(sum[:]) != c.want {
			t.Errorf("%s changed: sha256 %x, pinned %s", name, sum, c.want)
		}
	}
}
