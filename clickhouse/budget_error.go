package clickhouse

import (
	"errors"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
)

// ClickHouse server exception codes for a query that exceeded its own
// configured read budget. TOO_MANY_BYTES is max_bytes_to_read (CHAOS-3848);
// TOO_MANY_ROWS is its row-count analogue, co-located because both name the
// same condition -- a query shape that will fail again on identical retry,
// as opposed to a transient dependency outage.
const (
	exceptionCodeTooManyBytes = 307
	exceptionCodeTooManyRows  = 158
)

// QueryBudgetExceededCode reports the ClickHouse exception code when err is,
// or wraps, a server exception signaling that a query exceeded its
// max_bytes_to_read/max_result_rows budget. ok is false for every other
// error, including nil.
//
// This stays a code, never the exception's Message: the message text is
// unbounded driver/query output, and callers that log this classification
// must not be handed something that could recreate the exact leak this
// helper exists to let them avoid.
func QueryBudgetExceededCode(err error) (code int32, ok bool) {
	var exception *clickhousedriver.Exception
	if !errors.As(err, &exception) {
		return 0, false
	}
	switch exception.Code {
	case exceptionCodeTooManyBytes, exceptionCodeTooManyRows:
		return exception.Code, true
	default:
		return 0, false
	}
}

// IsQueryBudgetExceeded reports whether err is, or wraps, a ClickHouse
// query-budget exception. See QueryBudgetExceededCode.
func IsQueryBudgetExceeded(err error) bool {
	_, ok := QueryBudgetExceededCode(err)
	return ok
}
