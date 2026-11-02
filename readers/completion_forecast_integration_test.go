//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/clickhouse"
	"github.com/full-chaos/dev-health-go/readers"
	"github.com/full-chaos/dev-health-go/schema"
)

// CHAOS-12584: ReadTeamCompletionForecasts against a real ClickHouse, on a
// capacity_forecasts table rendered from the declared production columns.
// Each forecast is its own INSERT, with merges stopped, so the latest-row
// rule runs over several parts as it does in production.

const (
	cfOrg      = "cf-org"
	cfOtherOrg = "cf-other-org"
	cfWideBins = 400
)

// cfInsert renders one forecast row. arrays is "" for a row written before
// ops migration 101: the two array columns are left to their default, [].
func cfInsert(org, forecastID, computedAt, teamID, workScopeID, percentiles, arrays string) string {
	columns := "org_id, forecast_id, computed_at, team_id, work_scope_id, backlog_size, simulation_count, p50_days, p85_days, p95_days, p50_date, p85_date, p95_date, throughput_mean, throughput_stddev, insufficient_history, high_variance"
	values := fmt.Sprintf("'%s', '%s', '%s', %s, %s, 40, 10000, %s, 2.5, 0.5, 0, 0", org, forecastID, computedAt, teamID, workScopeID, percentiles)
	if arrays != "" {
		columns += ", completion_days_values, completion_days_counts"
		values += ", " + arrays
	}
	return "INSERT INTO capacity_forecasts (" + columns + ") VALUES (" + values + ")"
}

func cfWideArrays() string {
	values := make([]string, 0, cfWideBins)
	counts := make([]string, 0, cfWideBins)
	for day := 0; day < cfWideBins; day++ {
		values = append(values, fmt.Sprint(day))
		counts = append(counts, "25")
	}
	return "[" + strings.Join(values, ",") + "], [" + strings.Join(counts, ",") + "]"
}

func cfSeed() []string {
	const full = "12, 30, 40, '2026-11-01', '2026-11-19', '2026-11-29'"
	return []string{
		// S1: an older and a newer forecast; the newer is served.
		cfInsert(cfOrg, "f-s1-old", "2026-10-01 00:00:00", "'T1'", "'S1'", "10, 25, 35, '2026-10-11', '2026-10-26', '2026-11-05'", "[10, 25], [6000, 4000]"),
		cfInsert(cfOrg, "f-s1-new", "2026-10-20 00:00:00", "'T1'", "'S1'", full, "[12, 30, 40], [5000, 3500, 1500]"),
		// A NULL scope and an empty scope are two forecasts.
		cfInsert(cfOrg, "f-null", "2026-10-15 00:00:00", "'T1'", "NULL", full, "[7], [10000]"),
		cfInsert(cfOrg, "f-empty", "2026-10-15 00:00:00", "'T1'", "''", full, "[8], [10000]"),
		// S2: written before migration 101, no p85/p95: empty arrays, NULLs kept.
		cfInsert(cfOrg, "f-s2", "2026-10-10 00:00:00", "'T1'", "'S2'", "9, NULL, NULL, NULL, NULL, NULL", ""),
		// S3: two forecasts at the same computed_at; forecast_id breaks the tie.
		cfInsert(cfOrg, "f-s3-a", "2026-10-12 00:00:00", "'T1'", "'S3'", full, "[5], [10000]"),
		cfInsert(cfOrg, "f-s3-b", "2026-10-12 00:00:00", "'T1'", "'S3'", full, "[6], [10000]"),
		// WIDE: more bins than CompletionDayBinLimit.
		cfInsert(cfOrg, "f-wide", "2026-10-16 00:00:00", "'T1'", "'WIDE'", full, cfWideArrays()),
		// Not requested, and another organization's newer forecast for T1/S1.
		cfInsert(cfOrg, "f-t2", "2026-10-20 00:00:00", "'T2'", "'S1'", full, "[3], [10000]"),
		cfInsert(cfOtherOrg, "f-other", "2026-10-25 00:00:00", "'T1'", "'S1'", full, "[99], [10000]"),
	}
}

func cfFull(hasScope uint8, scope, forecastAt string, values []uint16, counts []uint32) readers.CompletionForecastRow {
	return readers.CompletionForecastRow{
		TeamID: "T1", HasWorkScope: hasScope, WorkScopeID: scope, ComputedAt: forecastAt,
		SimulationCount: 10000, BacklogSize: 40,
		HasP50Days: 1, P50Days: 12, HasP85Days: 1, P85Days: 30, HasP95Days: 1, P95Days: 40,
		P50Date: "2026-11-01", P85Date: "2026-11-19", P95Date: "2026-11-29",
		CompletionDayBins: uint64(len(values)), CompletionCountBins: uint64(len(counts)),
		CompletionDaysValues: values, CompletionDaysCounts: counts,
	}
}

func cfWant() []readers.CompletionForecastRow {
	wide := cfFull(1, "WIDE", "2026-10-16 00:00:00.000", nil, nil)
	wide.CompletionDayBins, wide.CompletionCountBins = cfWideBins, cfWideBins
	for day := 0; day <= readers.CompletionDayBinLimit; day++ {
		wide.CompletionDaysValues = append(wide.CompletionDaysValues, uint16(day))
		wide.CompletionDaysCounts = append(wide.CompletionDaysCounts, 25)
	}
	s2 := readers.CompletionForecastRow{
		TeamID: "T1", HasWorkScope: 1, WorkScopeID: "S2", ComputedAt: "2026-10-10 00:00:00.000",
		SimulationCount: 10000, BacklogSize: 40, HasP50Days: 1, P50Days: 9,
	}
	return []readers.CompletionForecastRow{
		cfFull(0, "", "2026-10-15 00:00:00.000", []uint16{7}, []uint32{10000}),
		cfFull(1, "", "2026-10-15 00:00:00.000", []uint16{8}, []uint32{10000}),
		cfFull(1, "S1", "2026-10-20 00:00:00.000", []uint16{12, 30, 40}, []uint32{5000, 3500, 1500}),
		s2,
		cfFull(1, "S3", "2026-10-12 00:00:00.000", []uint16{6}, []uint32{10000}),
		wide,
	}
}

// cfSame compares rows with an empty array and a nil one taken as the same:
// both are "no distribution", and the driver's choice between them is not
// part of the contract.
func cfSame(got, want []readers.CompletionForecastRow) bool {
	normalize := func(rows []readers.CompletionForecastRow) []readers.CompletionForecastRow {
		out := make([]readers.CompletionForecastRow, len(rows))
		for i, row := range rows {
			if len(row.CompletionDaysValues) == 0 {
				row.CompletionDaysValues = nil
			}
			if len(row.CompletionDaysCounts) == 0 {
				row.CompletionDaysCounts = nil
			}
			out[i] = row
		}
		return out
	}
	return reflect.DeepEqual(normalize(got), normalize(want))
}

// cfCheck runs every assertion over client and returns the failures, so a
// planted defect can be shown to fail the same checks the test makes.
func cfCheck(client readers.QueryClient) []string {
	ctx := context.Background()
	var failures []string
	describe := func(rows []readers.CompletionForecastRow) string {
		parts := make([]string, 0, len(rows))
		for _, row := range rows {
			parts = append(parts, fmt.Sprintf("{team=%s has_scope=%d scope=%q at=%s p50=%d/%d p85=%d/%d p95=%d/%d dates=%q,%q,%q bins=%d/%d values=%d counts=%d}",
				row.TeamID, row.HasWorkScope, row.WorkScopeID, row.ComputedAt, row.HasP50Days, row.P50Days, row.HasP85Days, row.P85Days, row.HasP95Days, row.P95Days,
				row.P50Date, row.P85Date, row.P95Date, row.CompletionDayBins, row.CompletionCountBins, len(row.CompletionDaysValues), len(row.CompletionDaysCounts)))
		}
		return strings.Join(parts, "\n  ")
	}

	rows, err := readers.ReadTeamCompletionForecasts(ctx, client, cfOrg, []string{"T1"}, readers.TimeBound{})
	if err != nil {
		return append(failures, fmt.Sprintf("unbounded read: %v", err))
	}
	if want := cfWant(); !cfSame(rows, want) {
		failures = append(failures, fmt.Sprintf("unbounded read:\n got\n  %s\n want\n  %s", describe(rows), describe(want)))
	}

	// As of 2026-10-05 only S1's older forecast existed.
	bound := readers.TimeBound{Active: true, End: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	rows, err = readers.ReadTeamCompletionForecasts(ctx, client, cfOrg, []string{"T1"}, bound)
	if err != nil {
		return append(failures, fmt.Sprintf("bounded read: %v", err))
	}
	older := readers.CompletionForecastRow{
		TeamID: "T1", HasWorkScope: 1, WorkScopeID: "S1", ComputedAt: "2026-10-01 00:00:00.000",
		SimulationCount: 10000, BacklogSize: 40,
		HasP50Days: 1, P50Days: 10, HasP85Days: 1, P85Days: 25, HasP95Days: 1, P95Days: 35,
		P50Date: "2026-10-11", P85Date: "2026-10-26", P95Date: "2026-11-05",
		CompletionDayBins: 2, CompletionCountBins: 2,
		CompletionDaysValues: []uint16{10, 25}, CompletionDaysCounts: []uint32{6000, 4000},
	}
	if want := []readers.CompletionForecastRow{older}; !cfSame(rows, want) {
		failures = append(failures, fmt.Sprintf("read as of 2026-10-05:\n got\n  %s\n want\n  %s", describe(rows), describe(want)))
	}

	// The row bound keeps the first rows of the documented order.
	rows, err = readers.ReadTeamCompletionForecastsWithRowLimit(ctx, client, cfOrg, []string{"T1"}, readers.TimeBound{}, 2)
	if err != nil {
		return append(failures, fmt.Sprintf("limited read: %v", err))
	}
	if want := cfWant()[:2]; !cfSame(rows, want) {
		failures = append(failures, fmt.Sprintf("read limited to 2:\n got\n  %s\n want\n  %s", describe(rows), describe(want)))
	}
	return failures
}

func cfDatabase(t *testing.T) *clickhouse.Client {
	t.Helper()
	required := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1"
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" || os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required when reader integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_DSN and ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 are required for the real completion forecast test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN: %v", err)
	}
	database := fmt.Sprintf("cf_forecast_%d", rand.Uint64())
	options, err := clickhousedriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse integration DSN options: %v", err)
	}
	admin, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open integration admin connection: %v", err)
	}
	ctx := context.Background()
	if err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatalf("create %s: %v", database, err)
	}
	t.Cleanup(func() {
		_ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database)
		_ = admin.Close()
	})
	options.Auth.Database = database
	seed, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open seed connection: %v", err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	for _, stmt := range schema.DDL("capacity_forecasts") {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("create statement %q: %v", stmt, err)
		}
	}
	// Keep every forecast in its own part. Best effort: a role without the
	// privilege still runs every assertion, only over merged parts.
	_ = seed.Exec(ctx, "SYSTEM STOP MERGES capacity_forecasts")
	for _, stmt := range cfSeed() {
		if err := seed.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed statement %q: %v", stmt, err)
		}
	}
	parsed.Path = "/" + database
	client, err := clickhouse.NewClickHouseQueryClientWithOptions(clickhouse.Options{DSN: parsed.String(), QueryTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestIntegrationReadTeamCompletionForecasts reads the latest forecast per
// (team, work scope): the newest row wins, forecast_id breaks a computed_at
// tie, a NULL and an empty scope stay two rows, a row from before migration
// 101 reads empty arrays with its NULL percentiles kept, a distribution past
// CompletionDayBinLimit is read to one entry past the limit with its stored
// length, other teams and other organizations are not read, and the time
// bound and the row bound apply.
func TestIntegrationReadTeamCompletionForecasts(t *testing.T) {
	client := cfDatabase(t)
	for _, failure := range cfCheck(client) {
		t.Error(failure)
	}
}
