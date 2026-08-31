package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
)

// CHAOS-4651 requirement 1: operationError.Error() must report the
// underlying cause. RED-FIRST: watched failing on unmodified origin/main
// (b024bc2) before the fix landed --
//
//	chaos4651_red_test.go:31: Error() = "ClickHouse query failed", want it
//	to contain the underlying cause text
//
// (Error() returned the fixed template string regardless of cause.) Kept
// here as the permanent regression pin.
func TestCHAOS4651_operationError_Error_reports_the_cause(t *testing.T) {
	// Given a query that fails with a specific, diagnosable driver cause.
	connection := &fakeConnection{queryErr: &clickhousedriver.Exception{
		Code:    62,
		Message: "Syntax error: failed at position 7",
	}}
	client := newClient(connection)

	// When
	_, err := client.Query(context.Background(), "SELECT 1", nil)

	// Then Error() must surface the cause, not a fixed template string.
	if err == nil {
		t.Fatal("Query() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "Syntax error: failed at position 7") {
		t.Fatalf("Error() = %q, want it to contain the underlying cause text", err.Error())
	}
}

// Unwrap() must keep working after the Error() fix -- a later "fix" that
// flattens the cause into a string instead of formatting it would break
// errors.Is/errors.As for every caller (e.g. QueryBudgetExceededCode).
func TestCHAOS4651_operationError_Unwrap_still_reaches_the_typed_cause(t *testing.T) {
	// Given
	cause := &clickhousedriver.Exception{Code: 307, Message: "too many bytes"}
	connection := &fakeConnection{queryErr: cause}
	client := newClient(connection)

	// When
	_, err := client.Query(context.Background(), "SELECT 1", nil)

	// Then the wrapped cause is still reachable by type via errors.As, and
	// QueryBudgetExceededCode (which depends on this) still classifies it.
	var exception *clickhousedriver.Exception
	if !errors.As(err, &exception) || exception != cause {
		t.Fatalf("errors.As() did not reach the original *Exception cause: %v", err)
	}
	if code, ok := QueryBudgetExceededCode(err); !ok || code != 307 {
		t.Fatalf("QueryBudgetExceededCode() = (%d, %t), want (307, true)", code, ok)
	}
}

// Error() must fail CLOSED on a cause type this package does not recognize
// as safe to quote verbatim -- no free-form text from it reaches the
// formatted string, regardless of what shape that text takes. This is the
// case both because a dial/DSN error can legitimately contain credentials,
// and because a denylist (matching known-dangerous patterns like
// "user:pass@") would miss shapes it wasn't written for -- concretely, a
// query-string credential (?password=...), which a userinfo-only pattern
// does not match. Two independent leak shapes are tested so neither is a
// coincidence of one pattern.
//
// This test is also this ticket's mutation proof: run against a naive
// `describeCause` that returns `cause.Error()` (or `fmt.Sprintf("%v",
// cause)`) unconditionally, the query-param case goes RED -- watched
// directly, see the PR body. The type-based allowlist implementation makes
// both cases pass.
func TestCHAOS4651_operationError_Error_fails_closed_on_unrecognized_cause_types(t *testing.T) {
	tests := []struct {
		name   string
		cause  error
		secret string
	}{
		{"userinfo DSN credential", errors.New("dial clickhouse://user:super-secret-password@host failed"), "super-secret-password"},
		{"query-string credential", errors.New("dial https://host:8443/?password=super-secret-password failed"), "super-secret-password"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given
			connection := &fakeConnection{queryErr: tc.cause}
			client := newClient(connection)

			// When
			_, err := client.Query(context.Background(), "SELECT 1", nil)

			// Then
			if err == nil {
				t.Fatal("Query() error = nil, want an error")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("Error() leaked the secret: %v", err)
			}
			if strings.Contains(err.Error(), tc.cause.Error()) {
				t.Fatalf("Error() = %q, want an unrecognized cause's free-form text to never reach it (fail-closed)", err.Error())
			}
		})
	}
}

// CHAOS-4651 requirement 2: Options must be able to express "unlimited"
// (max_bytes_to_read = 0) distinctly from "unset, use the default". RED-
// FIRST: watched failing on unmodified origin/main before the fix landed --
//
//	chaos4651_red_test.go:50: max_bytes_to_read = 67108864 (the default),
//	want the caller's explicit request for an unbounded read to reach the
//	driver
//
// (Options.MaxBytesToRead was a plain uint64, so the zero value and "unset"
// were the same bit pattern and always collapsed to the 64 MiB default.)
func TestCHAOS4651_MaxBytesToRead_explicit_zero_reaches_the_driver_unlimited(t *testing.T) {
	// Given a caller who explicitly wants no read-byte ceiling.
	configured := &clickhousedriver.Options{}
	unlimited := new(uint64) // pointer to 0

	// When
	applyOptions(configured, Options{MaxBytesToRead: unlimited})

	// Then ClickHouse's own "0 means unrestricted" semantics reach the wire
	// verbatim -- not the 64 MiB default.
	if got := configured.Settings["max_bytes_to_read"]; got != uint64(0) {
		t.Fatalf("max_bytes_to_read = %v, want 0 (unrestricted)", got)
	}
}

// The nil (unset) case must still fall back to the default -- this is the
// distinction requirement 2 exists to preserve, not remove.
func TestCHAOS4651_MaxBytesToRead_unset_still_gets_the_default(t *testing.T) {
	// Given
	configured := &clickhousedriver.Options{}

	// When
	applyOptions(configured, Options{})

	// Then
	if got := configured.Settings["max_bytes_to_read"]; got != DefaultMaxBytesToRead {
		t.Fatalf("max_bytes_to_read = %v, want the default %d", got, DefaultMaxBytesToRead)
	}
}

// Symmetry: MaxResultRows gets the same nil-vs-explicit-zero treatment,
// ahead of CHAOS-4654 retiring the provisional rows value.
func TestCHAOS4651_MaxResultRows_explicit_zero_reaches_the_driver_unlimited(t *testing.T) {
	// Given
	configured := &clickhousedriver.Options{}
	unlimited := new(uint)

	// When
	applyOptions(configured, Options{MaxResultRows: unlimited})

	// Then
	if got := configured.Settings["max_result_rows"]; got != uint(0) {
		t.Fatalf("max_result_rows = %v, want 0 (unrestricted)", got)
	}
}

func TestCHAOS4651_MaxResultRows_unset_still_gets_the_default(t *testing.T) {
	// Given
	configured := &clickhousedriver.Options{}

	// When
	applyOptions(configured, Options{})

	// Then
	if got := configured.Settings["max_result_rows"]; got != uint(1_000) {
		t.Fatalf("max_result_rows = %v, want the default 1000", got)
	}
}

// A caller requesting a specific non-default, non-zero ceiling still gets
// exactly that value -- the pointer indirection must not disturb the
// ordinary "set a real number" path.
func TestCHAOS4651_MaxBytesToRead_explicit_value_reaches_the_driver_unchanged(t *testing.T) {
	// Given
	configured := &clickhousedriver.Options{}
	limit := uint64(2 << 30) // 2 GiB, e.g. CHAOS-4647's provisional value
	// When
	applyOptions(configured, Options{MaxBytesToRead: &limit})

	// Then
	if got := configured.Settings["max_bytes_to_read"]; got != uint64(2<<30) {
		t.Fatalf("max_bytes_to_read = %v, want %d", got, uint64(2<<30))
	}
}
