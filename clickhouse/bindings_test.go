package clickhouse

import (
	"errors"
	"testing"
)

// TestClickHouseStringArray_escapes_quotes_as_doubled_not_backslash locks in
// the CHAOS-4745 fix at the pure-encoding level: ClickHouse's native-protocol
// parameter parser rejects \' outright (reproduced against ClickHouse 25.1
// and 26.7 -- see clickhouse/integration_test.go's
// TestIntegrationClient_binds_Array_String_values_byte_exact for the
// server-round-trip proof) but accepts SQL-standard doubled-quote escaping.
// This test only pins the literal text this package emits; it cannot by
// itself prove ClickHouse accepts it -- that is the integration test's job.
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
			name:   "backslash is doubled",
			values: []string{`back\slash`},
			want:   `['back\\slash']`,
		},
		{
			name:   "backslash directly adjacent to a quote",
			values: []string{`a\'b`},
			want:   `['a\\''b']`,
		},
		{
			name:   "quote directly followed by backslash",
			values: []string{`a'\b`},
			want:   `['a''\\b']`,
		},
		{
			name:   "run of only quotes",
			values: []string{"''''"},
			want:   "['''''''''']",
		},
		{
			name:   "run of only backslashes",
			values: []string{`\\\\`},
			want:   `['\\\\\\\\']`,
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
			if got := clickHouseStringArray(tt.values); got != tt.want {
				t.Errorf("clickHouseStringArray(%q) = %q, want %q", tt.values, got, tt.want)
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
