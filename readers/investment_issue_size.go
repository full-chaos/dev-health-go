package readers

import (
	"fmt"
	"strings"
)

// UnitSizeWeightColumn is the column IssueSizedSource adds: what a work unit
// weighs in the ISSUE_SIZE measure (D6027: its issue size, 1 when it has none).
const UnitSizeWeightColumn = "unit_size_weight"

// UnitIssueSizeColumn is the other column IssueSizedSource adds: the unit's
// issue size, 0 when it has none, so a read can tell a unit sized 1 from an
// unsized one.
const UnitIssueSizeColumn = "unit_issue_size"

// UnitSizeWeightExpr is the weight of a work unit in ISSUE_SIZE over a row of
// LatestWorkUnitIssueSizeSource joined as alias: its issue size, or 1.0 when
// it has none or no size row. IssueSizedSource and every reader that joins the
// size source itself use it, so the rule is written once.
func UnitSizeWeightExpr(alias string) string {
	return fmt.Sprintf("if(%[1]s.issue_size > 0, %[1]s.issue_size, 1.0)", alias)
}

// UnitIssueSizeExpr is the unit's issue size over a row of
// LatestWorkUnitIssueSizeSource joined as alias: the size, or 0.0 when it has
// none or no size row (0 means unsized, D6027).
func UnitIssueSizeExpr(alias string) string {
	return fmt.Sprintf("if(%[1]s.issue_size > 0, %[1]s.issue_size, 0.0)", alias)
}

// LatestWorkUnitIssueSizeSource is the newest issue size of each work unit of
// the organization (work_unit_issue_size, ops migration 130): argMax over
// computed_at per (org_id, work_unit_id). It binds {org_id:String}.
//
// The size is materialised per unit by the investment materializer as the sum
// of the unit's issues' positive story points (D6027); this package never
// computes it, it only reads the stored value.
func LatestWorkUnitIssueSizeSource() string {
	return `(
                SELECT org_id, work_unit_id, argMax(issue_size, computed_at) AS issue_size
                FROM work_unit_issue_size
                WHERE org_id = {org_id:String}
                GROUP BY org_id, work_unit_id
            )`
}

// IssueSizedSource adds UnitSizeWeightColumn and UnitIssueSizeColumn to a
// work-unit source written as "(...) AS <alias>": the unit's issue size, or 1
// when it has none or no size row (D5999: an unsized unit counts one work
// unit; 0 points means unsized, D6027). Every other column, and the rows, are
// the source's own, so a reader that weighs by the column reads the same rows
// as a count.
func IssueSizedSource(aliasedSource, alias string) (string, error) {
	suffix := " AS " + alias
	if !strings.HasSuffix(aliasedSource, suffix) {
		return "", fmt.Errorf("readers: IssueSizedSource: the source does not end in %q", suffix)
	}
	inner := strings.TrimSuffix(aliasedSource, suffix)
	return fmt.Sprintf(`(
            SELECT sized_unit.*, %s AS %s,
                %s AS %s
            FROM %s AS sized_unit
            LEFT JOIN %s AS unit_size
                ON unit_size.org_id = sized_unit.org_id
                AND unit_size.work_unit_id = sized_unit.work_unit_id
        )%s`, UnitSizeWeightExpr("unit_size"), UnitSizeWeightColumn, UnitIssueSizeExpr("unit_size"), UnitIssueSizeColumn, inner, LatestWorkUnitIssueSizeSource(), suffix), nil
}

// SizingCoverageSelectList is the select list of a sizing-coverage read over a
// source IssueSizedSource built: the distinct work units that carry an issue
// size, then the distinct work units. workUnitIDExpr is the column holding the
// work unit id in the read's FROM. It scans into two *uint64
// (ScanSizingCoverage), the form a scan-type census pins.
func SizingCoverageSelectList(workUnitIDExpr string) string {
	return fmt.Sprintf(`
    toUInt64(uniqExactIf(%[1]s, %[2]s > 0)) AS sized_units,
    toUInt64(uniqExact(%[1]s)) AS units`, workUnitIDExpr, UnitIssueSizeColumn)
}

// SizingCoverage is how many distinct work units of a read carry an issue
// size, and how many units it holds (D6027): a share in ISSUE_SIZE is read
// together with it, because with few sized units the mix is mostly unsized
// units, each counted as 1 work unit.
type SizingCoverage struct {
	SizedUnits int
	Units      int
}

// ScanSizingCoverage scans the two columns of SizingCoverageSelectList off the
// current row. The destinations are *uint64, *uint64.
func ScanSizingCoverage(row interface{ Scan(...any) error }) (SizingCoverage, error) {
	var sized, units uint64
	if err := row.Scan(&sized, &units); err != nil {
		return SizingCoverage{}, fmt.Errorf("scan investment sizing coverage: %w", err)
	}
	return SizingCoverage{SizedUnits: int(sized), Units: int(units)}, nil
}

// Share is the sized share of the units, nil when the read holds no unit (not
// measured, never 0).
func (c SizingCoverage) Share() *float64 {
	if c.Units <= 0 {
		return nil
	}
	share := float64(c.SizedUnits) / float64(c.Units)
	return &share
}

// Unsized is the number of units without a size: 0 for a read with no unit, so
// an ISSUE_SIZE answer always carries its coverage (D6027) while Share stays
// nil (no unit to take a share of).
func (c SizingCoverage) Unsized() *int {
	unsized := 0
	if c.Units > 0 {
		unsized = c.Units - c.SizedUnits
	}
	return &unsized
}

// SizingNotMeasured is how a null coverage reads (D6031): the read held no
// unit, so there is no share to state.
const SizingNotMeasured = "not measured"

// Measured reports whether the read held a unit, so Share is not null.
func (c SizingCoverage) Measured() bool { return c.Units > 0 }

// Note is the "mostly unsized" sentence (D6031): when fewer than half of the
// units carry a size, the mix in ISSUE_SIZE is mostly unsized units counted 1
// each, and the answer says so. Empty otherwise, and for a read with no unit.
// This is the one definition of the sentence.
func (c SizingCoverage) Note() string {
	if 2*c.SizedUnits >= c.Units {
		return ""
	}
	return fmt.Sprintf("Mostly unsized: %d of %d work units have an issue size. The other %d have no size, so each counts as 1 work unit.",
		c.SizedUnits, c.Units, c.Units-c.SizedUnits)
}
