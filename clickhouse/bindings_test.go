package clickhouse

import (
	"errors"
	"testing"
)

// TestClickHouseStringArray_escapes_quotes_as_doubled_not_backslash locks in
// the CHAOS-4745 fix at the pure-encoding level: ClickHouse's native-protocol
// parameter parser rejects \' outright (reproduced against ClickHouse 25.1
// -- see clickhouse/integration_test.go's
// TestIntegrationClient_binds_Array_String_values_byte_exact for the
// server-round-trip proof) but accepts SQL-standard doubled-quote escaping.
// This test only pins the literal text this package emits for the values it
// accepts; it cannot by itself prove ClickHouse accepts it -- that is the
// integration test's job.
func TestClickHouseStringArray_escapes_quotes_as_doubled_not_backslash(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{
			name:   "empty slice",
			values: []string{},
			want:   "[]",
		},
		{
			name:   "plain value needs no escaping",
			values: []string{"repo-1"},
			want:   "['repo-1']",
		},
		{
			name:   "embedded quote is doubled, not backslash-escaped",
			values: []string{"O'Brien's project"},
			want:   "['O''Brien''s project']",
		},
		{
			name:   "run of only quotes",
			values: []string{"''''"},
			want:   "['''''''''']",
		},
		{
			name:   "multiple elements joined with a comma",
			values: []string{"a", "b,c"},
			want:   "['a','b,c']",
		},
		{
			name:   "brackets in value content are not special",
			values: []string{"[a,b]"},
			want:   "['[a,b]']",
		},
		{
			name:   "unicode content passes through unescaped",
			values: []string{"héllo wörld 日本語 🚀"},
			want:   "['héllo wörld 日本語 🚀']",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := clickHouseStringArray(tt.values)
			if err != nil {
				t.Fatalf("clickHouseStringArray(%q) error = %v", tt.values, err)
			}
			if got != tt.want {
				t.Errorf("clickHouseStringArray(%q) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}

// TestClickHouseStringArray_fails_closed_on_any_backslash covers the
// CHAOS-4745 finding that a doubled-backslash escape (\\), though it
// decodes correctly against ClickHouse's real parameter parser when
// isolated, decodes INCONSISTENTLY -- sometimes silently dropping bytes,
// sometimes hard-erroring -- when adjacent to another escape (a second
// backslash, a doubled quote, or a letter ClickHouse's own escape table
// also recognizes, e.g. \b, \n). Executed proof: clickhouse/bindings.go's
// clickHouseQuotedString doc comment. Rather than characterize which
// placements are safe, this package rejects any backslash outright --
// never silently truncates a value.
func TestClickHouseStringArray_fails_closed_on_any_backslash(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "single backslash", value: `back\slash`},
		{name: "backslash directly followed by a quote", value: `a\'b`},
		{name: "quote directly followed by a backslash", value: `a'\b`},
		{name: "run of only backslashes", value: `\\\\`},
		{name: "backslash followed by a named-escape letter", value: `a\b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clickHouseStringArray([]string{tt.value})
			if !errors.Is(err, ErrUnsafeBindingValue) {
				t.Fatalf("clickHouseStringArray([%q]) error = %v, want ErrUnsafeBindingValue", tt.value, err)
			}
		})
	}
}

// TestClickHouseParameter_fails_closed_for_unsupported_slice_shapes covers
// CHAOS-4729: a slice/array shape this package has no native ClickHouse
// literal encoding for (e.g. what a caller would reach for to bind
// Array(Tuple(...))) must be rejected with the specific, typed
// ErrUnsupportedBinding -- never silently encoded, and never folded into
// the generic ErrInvalidBinding a caller can't distinguish from a malformed
// binding name/duplicate.
func TestClickHouseParameter_fails_closed_for_unsupported_slice_shapes(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "slice of string pairs", value: [][2]string{{"a", "b"}}},
		{name: "slice of ints", value: []int{1, 2, 3}},
		{name: "array of strings", value: [2]string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clickHouseParameter(tt.value)
			if !errors.Is(err, ErrUnsupportedBinding) {
				t.Fatalf("clickHouseParameter(%#v) error = %v, want ErrUnsupportedBinding", tt.value, err)
			}
			if errors.Is(err, ErrInvalidBinding) {
				t.Fatalf("clickHouseParameter(%#v) error also matches ErrInvalidBinding -- want it distinguishable", tt.value)
			}
		})
	}
}

func TestTranslateBindings_ArrayString_uses_doubled_quote_escaping(t *testing.T) {
	bindings := []Binding{{Name: "ids", Value: []string{"O'Brien"}}}

	parameters, err := translateBindings(bindings)

	if err != nil {
		t.Fatalf("translateBindings() error = %v", err)
	}
	if got, want := parameters["ids"], "['O''Brien']"; got != want {
		t.Fatalf("ids parameter = %q, want %q", got, want)
	}
}

func TestTranslateBindings_ArrayString_rejects_backslash(t *testing.T) {
	bindings := []Binding{{Name: "ids", Value: []string{`back\slash`}}}

	_, err := translateBindings(bindings)

	if !errors.Is(err, ErrUnsafeBindingValue) {
		t.Fatalf("translateBindings() error = %v, want ErrUnsafeBindingValue", err)
	}
}
