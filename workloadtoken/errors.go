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
	// ErrRequestRefused is a request the token transport will not send: not
	// for its target origin, a redirect hop, or without a URL.
	ErrRequestRefused = errors.New("workloadtoken: the request is not for the token's target")
	// ErrResourceTransport wraps a failure of the transport behind the token
	// transport. Its text never carries the failure's own text, which may
	// quote the request's headers; context cancellation and deadline stay
	// visible to errors.Is.
	ErrResourceTransport = errors.New("workloadtoken: the resource request failed")
	// ErrSubjectUnreadable is a subject token file that is missing, not a
	// regular file, empty, too large or not a compact JWS.
	ErrSubjectUnreadable = errors.New("workloadtoken: the subject token could not be read")
)

// ConfigField names a field of Config.
type ConfigField string

// The Config fields New can refuse.
const (
	FieldSubject         ConfigField = "Subject"
	FieldTransport       ConfigField = "Transport"
	FieldIssuer          ConfigField = "Issuer"
	FieldExchangeURL     ConfigField = "ExchangeURL"
	FieldTimeout         ConfigField = "Timeout"
	FieldRefreshFraction ConfigField = "RefreshFraction"
	FieldSafetyMargin    ConfigField = "SafetyMargin"
	FieldBackoffFloor    ConfigField = "BackoffFloor"
	FieldBackoffCeiling  ConfigField = "BackoffCeiling"
)

// ConfigFieldError is the error New returns for a Config it refuses. Field
// names the setting that is wrong, so a caller can point at its own variable
// or flag without parsing text; Err is the cause. Both are fixed: neither
// carries a value from the Config, so an issuer or URL with credentials in it
// never reaches a log through this error.
type ConfigFieldError struct {
	Field ConfigField
	Err   error
}

func (e *ConfigFieldError) Error() string { return e.Err.Error() }

func (e *ConfigFieldError) Unwrap() error { return e.Err }
