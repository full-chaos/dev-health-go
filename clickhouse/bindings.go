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
	}
	return "", fmt.Errorf("binding value: %w", ErrInvalidBinding)
}

func clickHouseStringArray(values []string) string {
	encoded := make([]string, len(values))
	for i, value := range values {
		encoded[i] = "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
	}
	return "[" + strings.Join(encoded, ",") + "]"
}
