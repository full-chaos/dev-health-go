//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
)

const (
	orderFixtureItems = 300
	orderFixtureRepos = 7
)

func orderFixtureRepo(n int) string { return fmt.Sprintf("repo-%d", n%orderFixtureRepos) }
func orderFixtureItem(n int) string { return fmt.Sprintf("WI-%03d", n) }

// seedOrderFixture inserts orderFixtureItems matching rows for org as several
// separate INSERTs, so the table holds several parts. insertSpans lists
// (offset, count, descending) in the order the parts are written; two orgs
// seeded with different span orders hold the same logical rows in different
// part orders.
func seedOrderFixture(t *testing.T, org string, insertSpans [][3]int) {
	t.Helper()
	for _, span := range insertSpans {
		offset, count, descending := span[0], span[1], span[2] == 1
		direction := "ASC"
		if descending {
			direction = "DESC"
		}
		integrationExec(t, fmt.Sprintf(`INSERT INTO work_items (org_id, work_item_id, status, title, repo_id, created_at)
SELECT '%s', concat('WI-', leftPad(toString(number), 3, '0')), 'open', concat('title ', toString(number)), concat('repo-', toString(number %% %d)), toDateTime64('2026-01-01 00:00:00', 6, 'UTC') + toIntervalDay(number)
FROM numbers(%d, %d)
ORDER BY number %s`, org, orderFixtureRepos, offset, count, direction))
	}
}

func orderFixtureIDs() []string {
	ids := make([]string, 0, orderFixtureItems)
	for n := 0; n < orderFixtureItems; n++ {
		ids = append(ids, orderFixtureRepo(n)+":"+orderFixtureItem(n))
	}
	return ids
}

// TestIntegrationLimitReadsServeTheSameRowsForAnyPartOrder reads more rows
// than the LIMIT keeps from two orgs that hold the same logical rows in
// different part orders, and requires the same rows in the documented order.
func TestIntegrationLimitReadsServeTheSameRowsForAnyPartOrder(t *testing.T) {
	client := integrationReadersClient(t)
	ctx := context.Background()
	const orgAscending, orgDescending = "order-fixture-ascending", "order-fixture-descending"
	seedOrderFixture(t, orgAscending, [][3]int{{0, 100, 0}, {100, 100, 0}, {200, 100, 0}})
	seedOrderFixture(t, orgDescending, [][3]int{{200, 100, 1}, {0, 100, 1}, {100, 100, 1}})
	ids := orderFixtureIDs()
	limit := readers.DefaultRowLimit

	byKey := make([]int, orderFixtureItems)
	for n := range byKey {
		byKey[n] = n
	}
	sort.Slice(byKey, func(i, j int) bool {
		a, b := byKey[i], byKey[j]
		if orderFixtureRepo(a) != orderFixtureRepo(b) {
			return orderFixtureRepo(a) < orderFixtureRepo(b)
		}
		return orderFixtureItem(a) < orderFixtureItem(b)
	})
	wantByKey := make([]string, 0, limit)
	for _, n := range byKey[:limit] {
		wantByKey = append(wantByKey, orderFixtureRepo(n)+":"+orderFixtureItem(n))
	}
	wantNewestFirst := make([]string, 0, limit)
	for n := orderFixtureItems - 1; len(wantNewestFirst) < limit; n-- {
		wantNewestFirst = append(wantNewestFirst, orderFixtureRepo(n)+":"+orderFixtureItem(n))
	}

	for _, org := range []string{orgAscending, orgDescending} {
		org := org
		t.Run("status "+org, func(t *testing.T) {
			rows, err := readers.ReadWorkItemStatusWithRowLimit(ctx, client, org, ids, limit)
			if err != nil {
				t.Fatalf("ReadWorkItemStatusWithRowLimit() error = %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.RepoID+":"+row.ID)
			}
			if !reflect.DeepEqual(got, wantByKey) {
				t.Fatalf("status rows = %v, want the first %d by (repo_id, work_item_id) = %v", got, limit, wantByKey)
			}
		})
		t.Run("identity "+org, func(t *testing.T) {
			rows, err := readers.ReadWorkItemIdentityWithRowLimit(ctx, client, org, ids, limit)
			if err != nil {
				t.Fatalf("ReadWorkItemIdentityWithRowLimit() error = %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.RepoID+":"+row.ID)
			}
			if !reflect.DeepEqual(got, wantByKey) {
				t.Fatalf("identity rows = %v, want the first %d by (repo_id, work_item_id) = %v", got, limit, wantByKey)
			}
		})
		t.Run("completion "+org, func(t *testing.T) {
			rows, err := readers.ReadWorkItemCompletionWithRowLimit(ctx, client, org, ids, readers.TimeBound{}, limit)
			if err != nil {
				t.Fatalf("ReadWorkItemCompletionWithRowLimit() error = %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.RepoID+":"+row.ID)
			}
			if !reflect.DeepEqual(got, wantNewestFirst) {
				t.Fatalf("completion rows = %v, want the %d newest by created_at = %v", got, limit, wantNewestFirst)
			}
		})
	}
}
