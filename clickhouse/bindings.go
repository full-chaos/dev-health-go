package clickhouse

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var bindingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func translateBindings(bindings []Binding) (map[string]string, error) {
	parameters := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		if !bindingName.MatchString(binding.Name) {
			return nil, fmt.Errorf("binding name: %w", ErrInvalidBinding)
		}
		if _, exists := parameters[binding.Name]; exists {
			return nil, fmt.Errorf("duplicate binding: %w", ErrInvalidBinding)
		}
		value, err := clickHouseParameter(binding.Value)
		if err != nil {
			return nil, err
		}
		parameters[binding.Name] = value
	}
	return parameters, nil
}

func clickHouseParameter(value any) (string, error) {
	if value == nil {
		return `\\N`, nil
	}
	switch value := value.(type) {
	case time.Time:
		return value.UTC().Format("2006-01-02 15:04:05.000"), nil
	case []string:
		return clickHouseStringArray(value), nil
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String:
		return reflected.String(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(reflected.Uint(), 10), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(reflected.Int(), 10), nil
	case reflect.Slice, reflect.Array:
		// Any slice/array shape not already special-cased above (e.g. a
		// []{2}string or a Pair struct slice a caller reaches for to bind
		// Array(Tuple(...)) -- CHAOS-4729) has no native ClickHouse literal
		// encoding in this package yet. Fail closed with a typed,
		// specific error instead of silently falling through to the
		// generic ErrInvalidBinding a caller can't distinguish from a
		// malformed binding name/duplicate.
		return "", fmt.Errorf("binding value: %w", ErrUnsupportedBinding)
	}
	return "", fmt.Errorf("binding value: %w", ErrInvalidBinding)
}

// clickHouseStringArray renders values as a ClickHouse Array(String)
// literal suitable for a native-protocol query-parameter value (the
// {name:Array(String)} mechanism clickhousedriver.WithParameters uses --
// see clickHouseQuotedString's doc comment for why this is not the same
// escaping ordinary ClickHouse SQL text accepts).
func clickHouseStringArray(values []string) string {
	encoded := make([]string, len(values))
	for i, value := range values {
		encoded[i] = "'" + clickHouseQuotedString(value) + "'"
	}
	return "[" + strings.Join(encoded, ",") + "]"
}

// clickHouseQuotedString escapes value for embedding inside a single-quoted
// element of a ClickHouse array literal sent as a native-protocol
// query-parameter value.
//
// This is NOT the same escaping ordinary ClickHouse SQL text accepts.
// ClickHouse's own syntax docs (https://clickhouse.com/docs/sql-reference/syntax)
// say an embedded single-quote character can be escaped EITHER by writing
// it twice in a row OR with a preceding backslash. But ClickHouse's
// native-protocol parameter parser -- the {name:Array(String)} mechanism
// clickhousedriver.WithParameters uses, NOT the clickhouse-client CLI
// (which decodes escapes client-side and is not a valid proxy for this
// parser) -- only accepts one of those two documented forms for a
// parameter value: it rejects the backslash-preceded form outright with
// "Cannot parse escape sequence" (CHAOS-4745, reproduced against
// ClickHouse 25.1 and 26.7), so this package writes the quote twice
// instead. A literal backslash is escaped by doubling it (which the same
// parser does accept); strings.NewReplacer applies both substitutions in
// a single left-to-right pass over the ORIGINAL text, so a backslash
// immediately adjacent to a quote is not double-processed -- each input
// byte is classified once, independent of what any other substitution
// emitted. See integration_test.go's
// TestIntegrationClient_binds_Array_String_values_byte_exact for the
// executed round-trip proof against a real server.
func clickHouseQuotedString(value string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(value)
}
