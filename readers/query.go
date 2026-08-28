// Package readers holds neutral, column-level ClickHouse row readers over
// the table contracts declared in package schema. A reader takes a
// QueryClient, an org id, and a set of natural row keys ("ids"), and
// returns plain Go structs scanned directly off the declared columns --
// never a GraphQL response shape, a resolver type, or any consumer-specific
// fact/adapter wrapper. Consumers (acr's devhealthfacts adapter, a future
// query-api GraphQL resolver layer) each wrap these plain rows into their
// own domain shape on top.
//
// Extracted from acr's internal/contextfabric/devhealthfacts package
// (CHAOS-4377): that package's readXxx methods interleaved a neutral
// SQL-scan with building an ACR-specific CanonicalFact per row. This
// package carries only the SQL-scan half; the CanonicalFact-building half
// stays in acr as its own adapter, calling into this package's readers.
package readers

import (
	"context"
	"errors"
	"strconv"

	"github.com/full-chaos/dev-health-go/clickhouse"
)

// Binding re-exports clickhouse.Binding so callers of this package do not
// need a second import just to pass an extra bound parameter (e.g. a
// TimeBound's Bindings()).
type Binding = clickhouse.Binding

// RowScanner re-exports clickhouse.RowScanner for the same reason.
type RowScanner = clickhouse.RowScanner

// QueryClient is the read-only ClickHouse query boundary every reader in
// this package uses. *clickhouse.Client satisfies it directly.
type QueryClient interface {
	Query(ctx context.Context, statement string, bindings []Binding) (RowScanner, error)
}

// ErrQueryClientRequired is returned by QueryOrgScoped when client is nil.
var ErrQueryClientRequired = errors.New("readers: clickhouse query client is required")

// QueryOrgScoped runs statement scoped to orgID and ids (bound as the
// {org_id:String} and {ids:Array(String)} parameters every reader in this
// package expects its statement to reference), invoking scan once per
// returned row. extra carries any additional bound parameters a specific
// reader's statement needs (e.g. a TimeBound's Bindings()). It never adds
// its own timeout; ctx is propagated straight through to client.
//
// Mirrors acr devhealthfacts's clickhouseFacts.query exactly, minus the
// Fact-specific pieces (readFailure classification stays with the caller).
func QueryOrgScoped(ctx context.Context, client QueryClient, statement, orgID string, ids []string, scan func(RowScanner) error, extra ...Binding) error {
	if client == nil {
		return ErrQueryClientRequired
	}
	bindings := make([]Binding, 0, 2+len(extra))
	bindings = append(bindings, Binding{Name: "org_id", Value: orgID}, Binding{Name: "ids", Value: ids})
	bindings = append(bindings, extra...)
	rows, err := client.Query(ctx, statement, bindings)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DefaultRowLimit is the anti-fanout row bound most readers in this
// package apply via WithRowLimit: a single query is generously bounded so
// one pathological subject (e.g. a work item with thousands of dependency
// rows) cannot return unbounded rows before a caller-side truncation check
// runs. Mirrors acr devhealthfacts's maxFactRowsPerQuery. A specific reader
// may pass a different limit to WithRowLimit if its own shape warrants it.
const DefaultRowLimit = 200

// WithRowLimit appends a LIMIT clause bounding statement to limit rows.
// limit must be an internal caller-controlled constant, never a value
// derived from a request -- mirroring acr devhealthfacts's withRowLimit,
// which inlines its own fixed maxFactRowsPerQuery the same way.
func WithRowLimit(statement string, limit int) string {
	return statement + "\nLIMIT " + strconv.Itoa(limit)
}
