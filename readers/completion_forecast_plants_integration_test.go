//go:build integration

package readers_test

// PROBE ONLY (CHAOS-12634): never merged. Each plant rewrites one clause of
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
		{"target NULL read as present", "toUInt8(isNotNull(target_items))", "toUInt8(1)"},
		{"target 0 read as absent", "toUInt8(isNotNull(target_items))", "toUInt8(ifNull(target_items, 0) != 0)"},
		{"target read as the backlog", "toInt64(ifNull(target_items, 0))", "toInt64(backlog_size)"},
		{"target NULL read as the backlog", "toInt64(ifNull(target_items, 0))", "toInt64(ifNull(target_items, backlog_size))"},
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
