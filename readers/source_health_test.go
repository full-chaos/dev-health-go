package readers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestReadSourceHealth(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM backfill_log", rows: [][]any{{"github", "success", int64(412), uint64(9800), "", "2026-08-12 03:00:00"}}},
		}}
		rows, err := readers.ReadSourceHealth(context.Background(), client, "org-1", []string{"org-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadSourceHealth() error = %v", err)
		}
		want := readers.SourceHealthRow{Provider: "github", Status: "success", ItemsSynced: 412, DurationMS: 9800, ErrorMessage: "", CreatedAt: "2026-08-12 03:00:00"}
		if len(rows) != 1 || rows[0] != want {
			t.Fatalf("rows = %#v, want %#v", rows, want)
		}
		if !strings.Contains(client.queries[0].statement, "PARTITION BY provider") {
			t.Fatalf("statement = %q, want the per-provider row_number window", client.queries[0].statement)
		}
	})

	t.Run("duration_ms above MaxInt64 is scanned raw, not wrapped", func(t *testing.T) {
		t.Parallel()
		const aboveMaxInt64 = uint64(1) << 63
		client := &fakeClient{tables: []fakeTable{
			{match: "FROM backfill_log", rows: [][]any{{"github", "success", int64(1), aboveMaxInt64, "", "2026-08-12 03:00:00"}}},
		}}
		rows, err := readers.ReadSourceHealth(context.Background(), client, "org-1", []string{"org-1"}, readers.TimeBound{})
		if err != nil {
			t.Fatalf("ReadSourceHealth() error = %v", err)
		}
		if len(rows) != 1 || rows[0].DurationMS != aboveMaxInt64 {
			t.Fatalf("rows = %#v, want the raw uint64 preserved for the caller to range-check", rows)
		}
	})

	t.Run("query error propagates", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM backfill_log", err: errors.New("boom")}}}
		rows, err := readers.ReadSourceHealth(context.Background(), client, "org-1", []string{"org-1"}, readers.TimeBound{})
		if err == nil || rows != nil {
			t.Fatalf("rows = %#v, err = %v, want (nil, err)", rows, err)
		}
	})

	t.Run("active time bound reaches the statement", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM backfill_log", rows: nil}}}
		end := mustTime(t, "2026-01-01T00:00:00Z")
		_, err := readers.ReadSourceHealth(context.Background(), client, "org-1", []string{"org-1"}, readers.TimeBound{Active: true, End: end})
		if err != nil {
			t.Fatalf("ReadSourceHealth() error = %v", err)
		}
		if !strings.Contains(client.queries[0].statement, "created_at <= {time_end:DateTime64(6,'UTC')}") {
			t.Fatalf("statement = %q, want the timestamp predicate", client.queries[0].statement)
		}
	})
}
