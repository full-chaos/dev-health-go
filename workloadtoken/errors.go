package workloadtoken

import "errors"

// The error classes a caller can branch on. No error this package returns
// carries a token, a subject token, a response body or a transport error's
// text: only one of these sentinels and a fixed class word.
var (
	// ErrNoUsableToken is returned when no token may be sent: the exchange
	// failed and no cached token is still inside its usable window. The
	// caller must not send the request.
	ErrNoUsableToken = errors.New("workloadtoken: no usable workload token")
	// ErrExchangeRefused is the exchange's 400 invalid_grant: the subject
	// was refused (unknown, disabled, wrong audience or scope).
	ErrExchangeRefused = errors.New("workloadtoken: the exchange refused the subject")
	// ErrExchangeInvalidRequest is the exchange's 400 invalid_request: the
	// request shape was refused.
	ErrExchangeInvalidRequest = errors.New("workloadtoken: the exchange refused the request shape")
	// ErrExchangeUnavailable is every other failed exchange: a transport
	// failure or timeout, a 503 or any other status, or an answer that does
	// not have the token-exchange shape.
	ErrExchangeUnavailable = errors.New("workloadtoken: the exchange is unavailable")
	// ErrSubjectUnreadable is a subject token file that is missing, not a
	// regular file, empty, too large or not a compact JWS.
	ErrSubjectUnreadable = errors.New("workloadtoken: the subject token could not be read")
)
