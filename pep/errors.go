package pep

import (
	"fmt"

	"github.com/full-chaos/dev-health-go/pep/internal/decision"
)

// Error is the typed cause of a local deny. It carries the closed local reason
// and never the transport error: a URL, a host or a credential in a transport
// error must not reach a caller's log.
type Error struct {
	reason Reason
	status int
}

// Reason is the closed local reason of the deny.
func (e *Error) Reason() Reason {
	if e == nil {
		return ReasonNoDecision
	}
	return e.reason
}

func (e *Error) Error() string {
	if e == nil {
		return "pep: no decision"
	}
	if e.status != 0 {
		return fmt.Sprintf("pep: denied: %s (http %d)", e.reason, e.status)
	}
	return "pep: denied: " + string(e.reason)
}

func fail(r Reason) *Error { return &Error{reason: r} }

func failStatus(r Reason, status int) *Error { return &Error{reason: r, status: status} }

func deny(e *Error) Decision { return decision.Local(e.reason, e) }
