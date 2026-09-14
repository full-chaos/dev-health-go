//go:build integration

package readers_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/full-chaos/dev-health-go/clickhouse"
	"github.com/full-chaos/dev-health-go/readers"
)

const integrationWorkItemOrg = "reader-scope-fixture-org"
const integrationZeroRepositoryID = "00000000-0000-0000-0000-000000000000"

var integrationWorkItemIDs = []string{
	"repo-a:WI-A",
	"repo-b:WI-B-EARLY",
	"repo-b:WI-B-LATE",
	"repo-b:WI-B-NOT-YET",
	"repo-c:WI-C",
	"repo-missing:WI-MISSING",
	integrationZeroRepositoryID + ":WI-ZERO",
	"repo-cross-org:WI-CROSS-ORG",
	"repo-empty:WI-EMPTY-METADATA",
	"repo-malformed:WI-MALFORMED-METADATA",
}

func integrationReadersClient(t *testing.T) *clickhouse.Client {
	t.Helper()
	required := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_REQUIRED") == "1"
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	if dsn == "" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DSN is required when reader integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_DSN is required for the real reader integration test")
	}
	if os.Getenv("ACR_CLICKHOUSE_INTEGRATION_ISOLATED") != "1" {
		if required {
			t.Fatal("ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 is required when reader integration is mandatory")
		}
		t.Skip("ACR_CLICKHOUSE_INTEGRATION_ISOLATED=1 is required before the reader integration test can target seeded data")
	}
	expectedDatabase := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DATABASE")
	if required && expectedDatabase == "" {
		t.Fatal("ACR_CLICKHOUSE_INTEGRATION_DATABASE is required when reader integration is mandatory")
	}

	client, err := clickhouse.NewClickHouseQueryClientWithOptions(clickhouse.Options{
		DSN:          dsn,
		QueryTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
	})

	rows, err := client.Query(context.Background(), "SELECT currentDatabase(), version()", nil)
	if err != nil {
		t.Fatalf("identify integration ClickHouse server: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("identify integration ClickHouse server returned no row: %v", rows.Err())
	}
	var database, version string
	if err := rows.Scan(&database, &version); err != nil {
		t.Fatalf("scan integration ClickHouse identity: %v", err)
	}
	if version == "" {
		t.Fatal("integration ClickHouse version is empty")
	}
	if expectedDatabase != "" && database != expectedDatabase {
		t.Fatalf("integration ClickHouse database = %q, want isolated database %q (server version %s)", database, expectedDatabase, version)
	}
	return client
}

func integrationExec(t *testing.T, statement string) {
	t.Helper()
	dsn := os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN")
	options, err := clickhousedriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse integration mutation DSN: %v", err)
	}
	connection, err := clickhousedriver.Open(options)
	if err != nil {
		t.Fatalf("open integration mutation connection: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.Exec(context.Background(), statement); err != nil {
		t.Fatalf("integration statement %q: %v", statement, err)
	}
}

func TestIntegrationWorkItemReadersRespectScopeAndLimits(t *testing.T) {
	client := integrationReadersClient(t)
	ctx := context.Background()
	intersection := readers.AuthorizationScope{
		GrantedRepositoryIDs:   []string{"repo-a", "repo-b"},
		RequestedRepositoryIDs: []string{"repo-b", "repo-c"},
	}

	t.Run("status applies grant and requested intersection", func(t *testing.T) {
		rows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, intersection, readers.Settings{})
		if err != nil {
			t.Fatalf("ReadWorkItemStatusWithScope() error = %v", err)
		}
		want := map[string]string{
			"WI-B-EARLY":   "in_progress",
			"WI-B-LATE":    "done",
			"WI-B-NOT-YET": "planned",
		}
		got := make(map[string]string, len(rows))
		for _, row := range rows {
			if row.RepoID != "repo-b" {
				t.Errorf("status row %q has RepoID %q, want repo-b", row.ID, row.RepoID)
			}
			got[row.ID] = row.Status
		}
		if len(got) != len(want) {
			t.Fatalf("status rows = %#v, want exactly %#v", got, want)
		}
		for id, status := range want {
			if got[id] != status {
				t.Errorf("status[%q] = %q, want %q", id, got[id], status)
			}
		}
	})

	t.Run("title applies grant and requested intersection", func(t *testing.T) {
		rows, err := readers.ReadWorkItemTitleWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, intersection, readers.Settings{})
		if err != nil {
			t.Fatalf("ReadWorkItemTitleWithScope() error = %v", err)
		}
		want := map[string]string{
			"WI-B-EARLY":   "B completed before the cutoff",
			"WI-B-LATE":    "B completed after the cutoff",
			"WI-B-NOT-YET": "B created after the cutoff",
		}
		got := make(map[string]string, len(rows))
		for _, row := range rows {
			if row.RepoID != "repo-b" {
				t.Errorf("title row %q has RepoID %q, want repo-b", row.ID, row.RepoID)
			}
			got[row.ID] = row.Title
		}
		if len(got) != len(want) {
			t.Fatalf("title rows = %#v, want exactly %#v", got, want)
		}
		for id, title := range want {
			if got[id] != title {
				t.Errorf("title[%q] = %q, want %q", id, got[id], title)
			}
		}
	})

	t.Run("completion applies intersection and active time bound", func(t *testing.T) {
		bound := readers.TimeBound{Active: true, End: time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC)}
		rows, err := readers.ReadWorkItemCompletionWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, bound, intersection, readers.Settings{})
		if err != nil {
			t.Fatalf("ReadWorkItemCompletionWithScope() error = %v", err)
		}
		want := map[string]uint8{
			"WI-B-EARLY": 1,
			"WI-B-LATE":  0,
		}
		got := make(map[string]uint8, len(rows))
		for _, row := range rows {
			if row.RepoID != "repo-b" {
				t.Errorf("completion row %q has RepoID %q, want repo-b", row.ID, row.RepoID)
			}
			got[row.ID] = row.IsCompleted
		}
		if len(got) != len(want) {
			t.Fatalf("completion rows = %#v, want exactly %#v; WI-B-NOT-YET must be excluded by created_at", got, want)
		}
		for id, completed := range want {
			if got[id] != completed {
				t.Errorf("completion[%q] = %d, want %d", id, got[id], completed)
			}
		}
	})

	t.Run("nil scope is unconstrained", func(t *testing.T) {
		rows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.AuthorizationScope{}, readers.Settings{})
		if err != nil {
			t.Fatalf("ReadWorkItemStatusWithScope() with nil scope error = %v", err)
		}
		if len(rows) != len(integrationWorkItemIDs) {
			t.Fatalf("nil-scope status rows = %d, want %d", len(rows), len(integrationWorkItemIDs))
		}
	})

	t.Run("empty grant and requested scopes deny", func(t *testing.T) {
		emptyGrant, err := readers.ReadWorkItemTitleWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.AuthorizationScope{GrantedRepositoryIDs: []string{}}, readers.Settings{})
		if err != nil {
			t.Fatalf("ReadWorkItemTitleWithScope() with empty grant error = %v", err)
		}
		if len(emptyGrant) != 0 {
			t.Fatalf("empty-grant title rows = %#v, want no rows", emptyGrant)
		}
		emptyRequested, err := readers.ReadWorkItemCompletionWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.TimeBound{}, readers.AuthorizationScope{RequestedRepositoryIDs: []string{}}, readers.Settings{})
		if err != nil {
			t.Fatalf("ReadWorkItemCompletionWithScope() with empty requested scope error = %v", err)
		}
		if len(emptyRequested) != 0 {
			t.Fatalf("empty-requested completion rows = %#v, want no rows", emptyRequested)
		}
	})

	t.Run("selector grant and requested scope intersect by live repository metadata", func(t *testing.T) {
		requested := readers.RepositorySelectorSet{ExactSlugs: []string{" ACME/TOOLS "}}
		scope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted:   readers.RepositorySelectorSet{Owners: []string{" ACME "}},
				Requested: &requested,
			},
		}
		statusRows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("selector status read error = %v", err)
		}
		if got := len(statusRows); got != 3 {
			t.Fatalf("selector status rows = %d, want the three repo-b rows", got)
		}
		for _, row := range statusRows {
			if row.RepoID != "repo-b" {
				t.Errorf("selector status row = %#v, want repo-b only", row)
			}
		}

		titleRows, err := readers.ReadWorkItemTitleWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("selector title read error = %v", err)
		}
		if got := len(titleRows); got != 3 {
			t.Fatalf("selector title rows = %d, want the three repo-b rows", got)
		}

		completionRows, err := readers.ReadWorkItemCompletionWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.TimeBound{}, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("selector completion read error = %v", err)
		}
		if got := len(completionRows); got != 3 {
			t.Fatalf("selector completion rows = %d, want the three repo-b rows", got)
		}
	})

	t.Run("asymmetric exact selector grants and request retain only their intersection", func(t *testing.T) {
		requested := readers.RepositorySelectorSet{ExactSlugs: []string{" ACME/TOOLS ", " OTHER/THREE "}}
		scope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted:   readers.RepositorySelectorSet{ExactSlugs: []string{" ACME/ONE ", " ACME/TOOLS "}},
				Requested: &requested,
			},
		}
		rows, err := readers.ReadWorkItemTitleWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("asymmetric selector title read error = %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("asymmetric selector rows = %#v, want the three repo-b rows", rows)
		}
		for _, row := range rows {
			if row.RepoID != "repo-b" {
				t.Errorf("asymmetric selector row = %#v, want repo-b only", row)
			}
		}
	})

	t.Run("organization-wide grant without requested selector retains sentinel and orphan rows", func(t *testing.T) {
		scope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted: readers.RepositorySelectorSet{All: true},
			},
		}
		rows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("organization-wide selector status read error = %v", err)
		}
		if got := len(rows); got != len(integrationWorkItemIDs) {
			t.Fatalf("organization-wide selector rows = %d, want %d including sentinel/orphan rows", got, len(integrationWorkItemIDs))
		}
	})

	t.Run("explicit requested wildcard requires usable same-org repository metadata", func(t *testing.T) {
		requested := readers.RepositorySelectorSet{All: true}
		scope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted:   readers.RepositorySelectorSet{All: true},
				Requested: &requested,
			},
		}
		rows, err := readers.ReadWorkItemTitleWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("requested wildcard title read error = %v", err)
		}
		// A requested global wildcard follows production ScopeMatch's
		// short-circuit: a nonempty same-org repository row is usable for '*'
		// even when its slug is malformed. Exact/owner selectors below still
		// validate the repository-name grammar before comparing.
		want := map[string]bool{"WI-A": true, "WI-B-EARLY": true, "WI-B-LATE": true, "WI-B-NOT-YET": true, "WI-C": true, "WI-MALFORMED-METADATA": true}
		got := make(map[string]bool, len(rows))
		for _, row := range rows {
			got[row.ID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("requested wildcard rows = %#v, want only rows with real metadata %#v", got, want)
		}
		for id := range want {
			if !got[id] {
				t.Errorf("requested wildcard missing real repository row %q", id)
			}
		}
		for _, id := range []string{"WI-MISSING", "WI-ZERO", "WI-CROSS-ORG", "WI-EMPTY-METADATA"} {
			if got[id] {
				t.Errorf("requested wildcard returned unusable repository row %q", id)
			}
		}
	})

	t.Run("owner selector rejects malformed repository names", func(t *testing.T) {
		scope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted: readers.RepositorySelectorSet{Owners: []string{" ACME "}},
			},
		}
		rows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("owner selector status read error = %v", err)
		}
		for _, row := range rows {
			if row.ID == "WI-MALFORMED-METADATA" {
				t.Fatalf("owner selector returned malformed repository row %#v", row)
			}
		}
	})

	t.Run("metadata update changes the same one content statement's result", func(t *testing.T) {
		scope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted: readers.RepositorySelectorSet{ExactSlugs: []string{"acme/one"}},
			},
		}
		before, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("metadata-before status read error = %v", err)
		}
		if len(before) != 1 || before[0].ID != "WI-A" {
			t.Fatalf("metadata-before rows = %#v, want WI-A", before)
		}
		integrationExec(t, "INSERT INTO repos (id, org_id, repo, version) VALUES ('repo-a', 'reader-scope-fixture-org', 'blocked/one', 2)")
		afterOldSlug, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, scope, readers.Settings{})
		if err != nil {
			t.Fatalf("metadata-after old-slug status read error = %v", err)
		}
		if len(afterOldSlug) != 0 {
			t.Fatalf("metadata-after old-slug rows = %#v, want no rows", afterOldSlug)
		}
		newScope := readers.AuthorizationScope{
			RepositorySelectors: &readers.RepositorySelectorScope{
				Granted: readers.RepositorySelectorSet{ExactSlugs: []string{"blocked/one"}},
			},
		}
		afterNewSlug, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, newScope, readers.Settings{})
		if err != nil {
			t.Fatalf("metadata-after new-slug status read error = %v", err)
		}
		if len(afterNewSlug) != 1 || afterNewSlug[0].ID != "WI-A" {
			t.Fatalf("metadata-after new-slug rows = %#v, want WI-A", afterNewSlug)
		}
	})

	t.Run("positive result limit setting allows bounded result", func(t *testing.T) {
		settings := readers.Settings{MaxResultRows: uint64(len(integrationWorkItemIDs))}
		rows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.AuthorizationScope{}, settings)
		if err != nil {
			t.Fatalf("bounded status read error = %v", err)
		}
		if len(rows) != len(integrationWorkItemIDs) {
			t.Fatalf("bounded status rows = %d, want %d", len(rows), len(integrationWorkItemIDs))
		}
	})

	t.Run("result limit throws for every reader", func(t *testing.T) {
		settings := readers.Settings{MaxResultRows: 1}
		for _, read := range []struct {
			name string
			call func() (int, error)
		}{
			{
				name: "status",
				call: func() (int, error) {
					rows, err := readers.ReadWorkItemStatusWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.AuthorizationScope{}, settings)
					return len(rows), err
				},
			},
			{
				name: "title",
				call: func() (int, error) {
					rows, err := readers.ReadWorkItemTitleWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.AuthorizationScope{}, settings)
					return len(rows), err
				},
			},
			{
				name: "completion",
				call: func() (int, error) {
					rows, err := readers.ReadWorkItemCompletionWithScope(ctx, client, integrationWorkItemOrg, integrationWorkItemIDs, readers.TimeBound{}, readers.AuthorizationScope{}, settings)
					return len(rows), err
				},
			},
		} {
			read := read
			t.Run(read.name, func(t *testing.T) {
				rows, err := read.call()
				if err == nil {
					t.Fatalf("%s returned %d rows without an error; max_result_rows=1 must throw", read.name, rows)
				}
				if rows != 0 {
					t.Fatalf("%s returned %d rows with result-limit error, want no rows", read.name, rows)
				}
				var serverError *clickhousedriver.Exception
				if !errors.As(err, &serverError) {
					t.Fatalf("%s error = %v, want a native ClickHouse exception", read.name, err)
				}
				if serverError.Code != 396 {
					t.Fatalf("%s native error code = %d, want 396 (TOO_MANY_ROWS_OR_BYTES)", read.name, serverError.Code)
				}
			})
		}
	})
}
