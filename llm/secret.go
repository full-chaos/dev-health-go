package llm

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// redacted is what every printed form of a Secret shows.
const redacted = "[REDACTED]"

// Secret holds a credential. Its value is reachable only through Reveal: every
// fmt verb, slog, encoding/json and encoding.TextMarshaler print the redaction
// marker, so a Config or a provider printed whole by a caller never shows the
// key. The value is in an unexported field, so reflection-based printers that
// skip methods (fmt's %#v on a containing struct included) cannot read it
// either; Format covers that verb too.
type Secret struct {
	value string
}

// NewSecret wraps a credential value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the credential, for the one place that sends it.
func (s Secret) Reveal() string { return s.value }

// Configured reports whether a value is present.
func (s Secret) Configured() bool { return s.value != "" }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format prints the redaction marker for every verb.
func (s Secret) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(redacted)) }

// LogValue is the slog form.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON is the encoding/json form.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText covers encoders that use encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// providerView is what every printed form of a provider shows: its config
// fields, never the key, never the client. A provider keeps its config in an
// unexported field, which fmt prints by reflection without calling the
// Secret's own Format, so each provider prints through this view instead.
type providerView struct {
	Kind    Kind
	Model   string
	BaseURL string
}

func (v providerView) format(state fmt.State, verb rune) {
	if verb == 'v' && state.Flag('#') {
		_, _ = fmt.Fprintf(state, "llm.Provider{Kind:%q, Model:%q, BaseURL:%q}", v.Kind, v.Model, v.BaseURL)
		return
	}
	_, _ = fmt.Fprintf(state, "{Kind:%s Model:%s BaseURL:%s}", v.Kind, v.Model, v.BaseURL)
}

func (v providerView) logValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", string(v.Kind)), slog.String("model", v.Model), slog.String("base_url", v.BaseURL))
}

// MarshalJSON keeps encoding/json off the unexported config too.
func (v providerView) marshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind    Kind   `json:"kind"`
		Model   string `json:"model"`
		BaseURL string `json:"base_url"`
	}{v.Kind, v.Model, v.BaseURL})
}
