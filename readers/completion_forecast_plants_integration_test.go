//go:build integration

package readers_test

// PROBE ONLY (CHAOS-12584): never merged. Each plant rewrites one clause of
// the reader's statement on its way to the real ClickHouse and runs the same
// checks TestIntegrationReadTeamCompletionForecasts makes.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

type plantClient struct {
	inner    readers.QueryClient
	old, new string
	mu       sync.Mutex
	applied  int
	errs     []string
}

func (c *plantClient) Query(ctx context.Context, statement string, bindings []readers.Binding) (readers.RowScanner, error) {
	if c.old != "" {
		n := strings.Count(statement, c.old)
		if n > 1 {
			c.mu.Lock()
			c.errs = append(c.errs, "ambiguous plant: "+c.old)
			c.mu.Unlock()
		}
		if n == 1 {
			statement = strings.Replace(statement, c.old, c.new, 1)
			c.mu.Lock()
			c.applied++
			c.mu.Unlock()
		}
	}
	rows, err := c.inner.Query(ctx, statement, bindings)
	if err != nil {
		c.mu.Lock()
		c.errs = append(c.errs, err.Error())
		c.mu.Unlock()
	}
	return rows, err
}

func TestIntegrationCompletionForecastPlants(t *testing.T) {
	client := cfDatabase(t)
	plants := []struct{ name, old, new string }{
		{"control", "", ""},
		{"latest-row rule removed", "WHERE rn = 1", "WHERE rn >= 1"},
		{"oldest row served", "ORDER BY computed_at DESC, forecast_id DESC", "ORDER BY computed_at ASC, forecast_id DESC"},
		{"tiebreak reversed", "forecast_id DESC) AS rn", "forecast_id ASC) AS rn"},
		{"NULL and empty scope one partition", "PARTITION BY team_id, work_scope_id", "PARTITION BY team_id, ifNull(work_scope_id, '')"},
		{"has_work_scope always 1", "toUInt8(isNotNull(work_scope_id)) AS has_work_scope", "toUInt8(1) AS has_work_scope"},
		{"org filter dropped", "WHERE org_id = {org_id:String} AND team_id", "WHERE team_id"},
		{"team filter dropped", " AND team_id IN {ids:Array(String)}", ""},
		{"values not sliced", "arraySlice(completion_days_values, 1, 367)", "completion_days_values"},
		{"counts not sliced", "arraySlice(completion_days_counts, 1, 367)", "completion_days_counts"},
		{"values length after slice", "toUInt64(length(completion_days_values))", "toUInt64(length(arraySlice(completion_days_values, 1, 367)))"},
		{"counts length after slice", "toUInt64(length(completion_days_counts))", "toUInt64(length(arraySlice(completion_days_counts, 1, 367)))"},
		{"slice at the limit, no probe entry", "arraySlice(completion_days_values, 1, 367), arraySlice(completion_days_counts, 1, 367)", "arraySlice(completion_days_values, 1, 366), arraySlice(completion_days_counts, 1, 366)"},
		{"p85 NULL read as present", "toUInt8(isNotNull(p85_days))", "toUInt8(1)"},
		{"p95 NULL read as present", "toUInt8(isNotNull(p95_days))", "toUInt8(1)"},
		{"p85 date NULL read as epoch", "ifNull(toString(p85_date), '')", "toString(ifNull(p85_date, toDate(0)))"},
		{"time bound dropped", " AND computed_at <= {time_end:DateTime64(6,'UTC')}", ""},
		{"outer order without has_work_scope", "ORDER BY team_key, has_work_scope, work_scope_id", "ORDER BY team_key, work_scope_id"},
		{"counts length read from values", "toUInt64(length(completion_days_counts))", "toUInt64(length(completion_days_values)) AS counts_len_planted"},
		{"values length read from counts", "toUInt64(length(completion_days_values))", "toUInt64(length(completion_days_counts))"},
		{"values cut to the counts length", "arraySlice(completion_days_values, 1, 367)", "arraySlice(completion_days_values, 1, least(367, length(completion_days_counts)))"},
		{"outer order reversed", "ORDER BY team_key, has_work_scope, work_scope_id", "ORDER BY team_key, has_work_scope, work_scope_id DESC"},
	}
	survived := 0
	for _, plant := range plants {
		pc := &plantClient{inner: client, old: plant.old, new: plant.new}
		failures := cfCheck(pc)
		verdict := "KILLED"
		switch {
		case plant.old != "" && pc.applied == 0:
			verdict = "INVALID (no site)"
		case len(pc.errs) > 0:
			verdict = "INVALID (" + strings.Join(pc.errs, "; ") + ")"
		case plant.old == "" && len(failures) == 0:
			verdict = "CONTROL PASS"
		case plant.old == "":
			verdict = "CONTROL FAIL"
		case len(failures) == 0:
			verdict = "SURVIVED"
		}
		first := ""
		if len(failures) > 0 {
			first = strings.SplitN(failures[0], "\n", 2)[0]
		}
		t.Logf("PLANT %-40s %-14s applied=%d first=%s", plant.name, verdict, pc.applied, first)
		if verdict != "KILLED" && verdict != "CONTROL PASS" {
			survived++
		}
	}
	if survived > 0 {
		t.Errorf("%d plants not KILLED (or control failed)", survived)
	}
}
