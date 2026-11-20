//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/clickhouse"
	"github.com/full-chaos/dev-health-go/readers"
)

// Two sizes of one work unit at ONE computed_at millisecond: the issue-size
// source must serve the later insert, the row a ReplacingMergeTree merge
// keeps, before and after the merge. The want is the same reader on a database
// that holds only the later rows, so it does not depend on the key.

const isCreateTable = `CREATE TABLE work_unit_issue_size (
    org_id String, work_unit_id String, issue_size Float64, sized_issue_count UInt32,
    issue_count UInt32, categorization_run_id String, computed_at DateTime64(3, 'UTC')
) ENGINE = ReplacingMergeTree(computed_at) PARTITION BY toYYYYMM(computed_at)
ORDER BY (org_id, work_unit_id) SETTINGS index_granularity = 8192`

type isRow struct {
	unit, computedAt string
	size             float64
}

type isPair struct{ first, second, winner isRow }

func isPairs() []isPair {
	var out []isPair
	for _, order := range []string{"xy", "yx"} {
		for rep := 0; rep < 4; rep++ {
			unit := fmt.Sprintf("u-tie-%s-%d", order, rep)
			x := isRow{unit: unit, computedAt: vtTieAt, size: 3}
			y := isRow{unit: unit, computedAt: vtTieAt, size: 8}
			if order == "xy" {
				out = append(out, isPair{x, y, y})
			} else {
				out = append(out, isPair{y, x, x})
			}
		}
	}
	for rep := 0; rep < 2; rep++ {
		unit := fmt.Sprintf("u-ver-%d", rep)
		newer := isRow{unit: unit, computedAt: vtNewerAt, size: 5}
		older := isRow{unit: unit, computedAt: vtOlderAt, size: 13}
		out = append(out, isPair{newer, older, newer})
	}
	return out
}

func isFillers() []isRow {
	var out []isRow
	for i := 0; i < 16; i++ {
		out = append(out, isRow{unit: fmt.Sprintf("a-fill-%02d", i), computedAt: vtTieAt, size: 1})
	}
	return out
}

func isInsert(settings string, rows []isRow) string {
	values := make([]string, len(rows))
	for i, r := range rows {
		values[i] = fmt.Sprintf("('%s', '%s', %g, 1, 1, 'run', '%s')", ptmOrg, r.unit, r.size, r.computedAt)
	}
	return "INSERT INTO work_unit_issue_size (org_id, work_unit_id, issue_size, sized_issue_count, issue_count, categorization_run_id, computed_at) SETTINGS " + settings + " VALUES " + strings.Join(values, ", ")
}

func isDatabase(t *testing.T, prefix string) (clickhousedriver.Conn, *clickhouse.Client) {
	t.Helper()
	seed, client := vtDatabase(t, prefix)
	vtExec(t, seed, isCreateTable, "SYSTEM STOP MERGES work_unit_issue_size")
	return seed, client
}

func isRead(t *testing.T, client *clickhouse.Client) []string {
	t.Helper()
	source, err := readers.IssueSizedSource("(SELECT org_id, work_unit_id FROM work_unit_issue_size GROUP BY org_id, work_unit_id) AS wui", "wui")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := client.Query(context.Background(),
		"SELECT work_unit_id, "+readers.UnitIssueSizeColumn+" FROM "+source+" WHERE startsWith(work_unit_id, 'u-') ORDER BY work_unit_id",
		[]clickhouse.Binding{{Name: "org_id", Value: ptmOrg}})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var unit string
		var size float64
		if err := rows.Scan(&unit, &size); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%g", unit, size))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func isRequireStored(t *testing.T, seed clickhousedriver.Conn, units, rows, parts int) {
	t.Helper()
	got, err := seed.Query(context.Background(), "SELECT work_unit_id, count(), uniqExact(_part) FROM work_unit_issue_size WHERE startsWith(work_unit_id, 'u-') GROUP BY work_unit_id")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	seen := 0
	for got.Next() {
		var unit string
		var count, distinct uint64
		if err := got.Scan(&unit, &count, &distinct); err != nil {
			t.Fatal(err)
		}
		seen++
		if int(count) != rows || int(distinct) != parts {
			t.Fatalf("fixture: unit %s holds %d rows in %d parts, want %d in %d", unit, count, distinct, rows, parts)
		}
	}
	if seen != units {
		t.Fatalf("fixture: %d units stored, want %d", seen, units)
	}
}

func isCompare(t *testing.T, arm string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got  %v\n want %v", arm, got, want)
	}
}

func isMerge(t *testing.T, seed clickhousedriver.Conn, units int) {
	t.Helper()
	vtExec(t, seed, "SYSTEM START MERGES work_unit_issue_size", "OPTIMIZE TABLE work_unit_issue_size FINAL")
	isRequireStored(t, seed, units, 1, 1)
}

func TestIntegrationIssueSizeSameMillisecondTiePicksTheLaterInsert(t *testing.T) {
	pairs := isPairs()
	fillers := isFillers()
	var first, second, winners, onePart []isRow
	for _, p := range pairs {
		first = append(first, p.first)
		second = append(second, p.second)
		winners = append(winners, p.winner)
		onePart = append(onePart, p.first, p.second)
	}
	const sync = "async_insert=0"

	oracleSeed, oracle := isDatabase(t, "is_oracle")
	vtExec(t, oracleSeed, isInsert(sync, append(append([]isRow{}, fillers...), winners...)))
	want := isRead(t, oracle)
	if len(want) != len(pairs) {
		t.Fatalf("oracle answer holds %d units, want %d: %v", len(want), len(pairs), want)
	}

	twoSeed, two := isDatabase(t, "is_two_parts")
	vtExec(t, twoSeed, isInsert(sync, append(append([]isRow{}, fillers...), first...)), isInsert(sync, second))
	isRequireStored(t, twoSeed, len(pairs), 2, 2)
	isCompare(t, "two_parts", isRead(t, two), want)
	isMerge(t, twoSeed, len(pairs))
	isCompare(t, "two_parts_after_merge", isRead(t, two), want)

	oneSeed, one := isDatabase(t, "is_one_part")
	vtExec(t, oneSeed, isInsert(sync+", optimize_on_insert=0", append(append([]isRow{}, fillers...), onePart...)))
	isRequireStored(t, oneSeed, len(pairs), 2, 1)
	isCompare(t, "one_part", isRead(t, one), want)
	isMerge(t, oneSeed, len(pairs))
	isCompare(t, "one_part_after_merge", isRead(t, one), want)
}
