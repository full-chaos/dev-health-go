package readers_test

// CHAOS-5438 -- ProbeRowLimit, and the five work-item readers that can now
// opt into it.
//
// THE DEFECT THIS ENABLES A FIX FOR lives in the CALLER (acr's
// devhealthfacts), which computes `Truncated: len(rows) >= 200` against a
// statement this package bounds at `LIMIT 200`. Reading N rows under `LIMIT N`
// cannot distinguish "there were exactly N" from "there were more and we
// stopped", so a complete population of exactly 200 is served as a degraded,
// truncated answer. The caller cannot fix that alone: the LIMIT is built here.
//
// THE SHAPE OF THE FIX, and why it is additive. Ops pins v0.6.2 and must not
// need a bump, so every existing exported signature keeps its exact behaviour:
// the four-argument readers still bound at DefaultRowLimit, byte-identical
// statements. A caller that wants the probe opts in explicitly through the
// `...WithRowLimit` variant beside each one. That is what the delegation
// tests below pin -- an "additive" change that silently altered the default
// would be the regression, not the fix.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

// TestProbeRowLimitIsOneMoreThanTheDefault pins the constant's whole reason
// for existing. A probe equal to the default proves nothing; a probe two or
// more above it reads rows no caller will ever serve.
func TestProbeRowLimitIsOneMoreThanTheDefault(t *testing.T) {
	t.Parallel()
	if readers.ProbeRowLimit != readers.DefaultRowLimit+1 {
		t.Fatalf("ProbeRowLimit = %d, want DefaultRowLimit+1 = %d -- one row more than is served, and no more", readers.ProbeRowLimit, readers.DefaultRowLimit+1)
	}
}

// workItemReaderArm is one of the five work-item readers under the probe
// contract: its default form, its limit-taking form, and the table its
// statement selects from.
type workItemReaderArm struct {
	name  string
	match string
	// readDefault runs the pre-existing four-argument reader.
	readDefault func(ctx context.Context, client readers.QueryClient, ids []string) (int, error)
	// readWithLimit runs the new limit-taking variant.
	readWithLimit func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error)
}

func workItemReaderArms() []workItemReaderArm {
	return []workItemReaderArm{
		{
			name: "status", match: "FROM work_items",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadWorkItemStatus(ctx, client, "org-1", ids)
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadWorkItemStatusWithRowLimit(ctx, client, "org-1", ids, limit)
				return len(rows), err
			},
		},
		{
			name: "title", match: "FROM work_items",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadWorkItemTitle(ctx, client, "org-1", ids)
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadWorkItemTitleWithRowLimit(ctx, client, "org-1", ids, limit)
				return len(rows), err
			},
		},
		{
			name: "completion", match: "FROM work_items",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadWorkItemCompletion(ctx, client, "org-1", ids, readers.TimeBound{})
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadWorkItemCompletionWithRowLimit(ctx, client, "org-1", ids, readers.TimeBound{}, limit)
				return len(rows), err
			},
		},
		{
			name: "identity", match: "FROM work_items",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadWorkItemIdentity(ctx, client, "org-1", ids)
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadWorkItemIdentityWithRowLimit(ctx, client, "org-1", ids, limit)
				return len(rows), err
			},
		},
		{
			// CHAOS-5474: the two REPOSITORY-subject readers. They join the
			// work-item ones here rather than in a table of their own
			// because the property is the same one, and because a consumer
			// can hold both behind ONE truncation flag -- which is exactly
			// how fixing only the work-item half left the observable
			// dishonest.
			name: "repository_identity", match: "FROM repos",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadRepositoryIdentity(ctx, client, "org-1", ids)
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadRepositoryIdentityWithRowLimit(ctx, client, "org-1", ids, limit)
				return len(rows), err
			},
		},
		{
			name: "repository_ids", match: "FROM repos",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadRepositoryIDs(ctx, client, "org-1", ids)
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadRepositoryIDsWithRowLimit(ctx, client, "org-1", ids, limit)
				return len(rows), err
			},
		},
		{
			name: "work_item_repository", match: "INNER JOIN repos",
			readDefault: func(ctx context.Context, client readers.QueryClient, ids []string) (int, error) {
				rows, err := readers.ReadWorkItemRepository(ctx, client, "org-1", ids)
				return len(rows), err
			},
			readWithLimit: func(ctx context.Context, client readers.QueryClient, ids []string, limit int) (int, error) {
				rows, err := readers.ReadWorkItemRepositoryWithRowLimit(ctx, client, "org-1", ids, limit)
				return len(rows), err
			},
		},
	}
}

// TestWorkItemReadersAcceptAProbeRowLimit is the fix: the statement reads one
// row more than the caller will serve.
func TestWorkItemReadersAcceptAProbeRowLimit(t *testing.T) {
	t.Parallel()
	for _, arm := range workItemReaderArms() {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{}
			if _, err := arm.readWithLimit(context.Background(), client, []string{"repo-1:WIDGET-101"}, readers.ProbeRowLimit); err != nil {
				t.Fatalf("read error = %v", err)
			}
			if len(client.queries) != 1 {
				t.Fatalf("executed %d statements, want 1", len(client.queries))
			}
			want := "LIMIT " + strconv.Itoa(readers.ProbeRowLimit)
			if !strings.Contains(client.queries[0].statement, want) {
				t.Fatalf("statement = %q, want a %q clause", client.queries[0].statement, want)
			}
		})
	}
}

// TestTheDefaultReadersStayByteIdenticalAtTheDefaultLimit is the
// COMPATIBILITY pin, and the one that keeps ops on v0.6.2 without a bump: the
// four-argument reader and its limit-taking twin at DefaultRowLimit must emit
// the SAME statement, character for character. An additive change that
// silently altered the default would be the regression this whole PR exists
// to avoid.
func TestTheDefaultReadersStayByteIdenticalAtTheDefaultLimit(t *testing.T) {
	t.Parallel()
	for _, arm := range workItemReaderArms() {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			viaDefault := &fakeClient{}
			if _, err := arm.readDefault(context.Background(), viaDefault, []string{"repo-1:WIDGET-101"}); err != nil {
				t.Fatalf("default read error = %v", err)
			}
			viaLimit := &fakeClient{}
			if _, err := arm.readWithLimit(context.Background(), viaLimit, []string{"repo-1:WIDGET-101"}, readers.DefaultRowLimit); err != nil {
				t.Fatalf("limit read error = %v", err)
			}
			if len(viaDefault.queries) != 1 || len(viaLimit.queries) != 1 {
				t.Fatalf("statements: default=%d limit=%d, want 1 each", len(viaDefault.queries), len(viaLimit.queries))
			}
			if viaDefault.queries[0].statement != viaLimit.queries[0].statement {
				t.Fatalf("the four-argument reader's statement drifted from its own default:\n default: %q\n  limit: %q", viaDefault.queries[0].statement, viaLimit.queries[0].statement)
			}
			if !strings.Contains(viaDefault.queries[0].statement, "LIMIT "+strconv.Itoa(readers.DefaultRowLimit)) {
				t.Fatalf("statement = %q, want the unchanged LIMIT %d", viaDefault.queries[0].statement, readers.DefaultRowLimit)
			}
			if strings.Contains(viaDefault.queries[0].statement, "LIMIT "+strconv.Itoa(readers.ProbeRowLimit)) {
				t.Fatalf("the DEFAULT reader started probing -- ops pins v0.6.2 and must not be forced onto a new row bound")
			}
		})
	}
}

// TestTheProbeLimitReachesTheRowsNotJustTheStatement closes the gap between
// "the SQL says 201" and "201 rows actually come back": a variant that built
// the right statement but dropped rows on the floor would satisfy the
// statement pin above and still leave the caller unable to see its overflow
// row.
func TestTheProbeLimitReachesTheRowsNotJustTheStatement(t *testing.T) {
	t.Parallel()
	for _, arm := range workItemReaderArms() {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{tables: []fakeTable{{match: arm.match, rows: probeRows(arm.name, readers.ProbeRowLimit)}}}
			count, err := arm.readWithLimit(context.Background(), client, []string{"repo-1:WIDGET-101"}, readers.ProbeRowLimit)
			if err != nil {
				t.Fatalf("read error = %v", err)
			}
			if count != readers.ProbeRowLimit {
				t.Fatalf("rows returned = %d, want %d -- the overflow row is the caller's only truncation evidence", count, readers.ProbeRowLimit)
			}
		})
	}
}

// probeRows builds n rows in the column shape the named reader scans.
func probeRows(arm string, n int) [][]any {
	rows := make([][]any, n)
	for i := 0; i < n; i++ {
		id := "WIDGET-" + strconv.Itoa(i)
		switch arm {
		case "completion":
			rows[i] = []any{id, uint8(0), mustZeroTime(), "repo-1"}
		case "work_item_repository":
			rows[i] = []any{id, "repo-1", "acme/widget-service"}
		case "repository_identity":
			rows[i] = []any{"repo-" + strconv.Itoa(i), "acme/r" + strconv.Itoa(i), "github"}
		case "repository_ids":
			rows[i] = []any{"repo-" + strconv.Itoa(i)}
		default:
			// status, title and identity all scan (id, string, repo_id).
			rows[i] = []any{id, "value", "repo-1"}
		}
	}
	return rows
}

// mustZeroTime is the coalesced zero completed_at ReadWorkItemCompletion
// scans when the column is null -- the same `toDateTime64(0, 6, 'UTC')` its
// own statement produces.
func mustZeroTime() time.Time {
	return time.Unix(0, 0).UTC()
}
