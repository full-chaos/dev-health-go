package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"math/rand"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const bigInteger = int64(9007199254740993)

func bigEnumFormat(extra map[string]any) *JSONFormat {
	properties := map[string]any{"n": map[string]any{"type": "integer", "enum": []any{bigInteger, int64(2)}}}
	for key, value := range extra {
		properties[key] = value
	}
	return &JSONFormat{Name: "categorization", Schema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"n"}, "properties": properties,
	}}
}

func TestAnthropicSchemaKeepsIntegerTextInTheFormatPath(t *testing.T) {
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicText(`{"n":2}`, "end_turn")})
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: bigEnumFormat(nil)}); err != nil {
		t.Fatal(err)
	}
	if _, present := c.request(0)["output_config"]; !present {
		t.Fatal("the request is not on the output_config path")
	}
	if !bytes.Contains(c.bodies[0], []byte("9007199254740993")) {
		t.Errorf("the request body lost the integer text: %s", c.bodies[0])
	}
}

func TestAnthropicSchemaKeepsIntegerTextInTheToolPath(t *testing.T) {
	format := bigEnumFormat(map[string]any{"labels": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}})
	provider, c := newTestAnthropic(t, step{status: 200, body: anthropicToolUse("categorization", `{"n":2}`, "tool_use")})
	result, err := provider.Complete(context.Background(), Request{Prompt: "p", Format: format})
	if err != nil {
		t.Fatal(err)
	}
	if result.FormatPath != FormatPathTool {
		t.Fatalf("FormatPath = %q, want the tool path", result.FormatPath)
	}
	if !bytes.Contains(c.bodies[0], []byte("9007199254740993")) {
		t.Errorf("the request body lost the integer text: %s", c.bodies[0])
	}
}

func TestAnthropicSchemaKeepsMinItemsOfOne(t *testing.T) {
	schema := map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}
	out := anthropicSchema(map[string]any{"type": "object", "properties": map[string]any{"a": schema}})
	a := out["properties"].(map[string]any)["a"].(map[string]any)
	got, kept := a["minItems"]
	if raw, _ := json.Marshal(got); !kept || string(raw) != "1" {
		t.Errorf("minItems = %s (kept %v), want 1 kept", raw, kept)
	}
	zero := anthropicSchema(map[string]any{"type": "array", "minItems": 5})
	if _, kept := zero["minItems"]; kept {
		t.Error("minItems 5 survived: only 0 and 1 are accepted")
	}
}

func TestEnumUnderATypeListKeepsNumericValues(t *testing.T) {
	out := anthropicSchema(map[string]any{"type": []any{"integer", "null"}, "enum": []any{bigInteger, nil}})
	raw, _ := json.Marshal(out["enum"])
	if string(raw) != `[9007199254740993,null]` {
		t.Errorf("enum = %s, want [9007199254740993,null]", raw)
	}
	out = anthropicSchema(map[string]any{"type": []any{"number", "null"}, "enum": []any{json.Number("1.50"), nil, "x"}})
	raw, _ = json.Marshal(out["enum"])
	if string(raw) != `[1.50,null]` {
		t.Errorf("enum = %s, want [1.50,null]", raw)
	}
}

func TestSchemaNumberOfAFloat64(t *testing.T) {
	if n, ok := schemaNumber(float64(1.5)); !ok || n != 1.5 {
		t.Errorf("schemaNumber(1.5) = %v, %v", n, ok)
	}
	if _, ok := schemaNumber("1"); ok {
		t.Error("schemaNumber accepted a string")
	}
}

func TestSchemaNumberOfAJSONNumber(t *testing.T) {
	if n, ok := schemaNumber(json.Number("1")); !ok || n != 1 {
		t.Errorf("schemaNumber(json.Number 1) = %v, %v", n, ok)
	}
	if n, ok := schemaNumber(json.Number("1.50")); !ok || n != 1.5 {
		t.Errorf("schemaNumber(json.Number 1.50) = %v, %v", n, ok)
	}
}

func TestSchemaIntegerAbove2To63(t *testing.T) {
	for text, want := range map[string]bool{
		"9223372036854775809":  true,
		"9007199254740993":     true,
		"2.0":                  true,
		"1E2":                  true,
		"1.5":                  false,
		"-0":                   true,
		"18446744073709551617": true,
	} {
		if got := schemaInteger(json.Number(text)); got != want {
			t.Errorf("schemaInteger(%s) = %v, want %v", text, got, want)
		}
	}
	if !schemaInteger(float64(3)) || schemaInteger(float64(1.5)) {
		t.Error("float64 integer check is wrong")
	}
}

func TestSchemaNumbersOutOfFloatRangeAreKeptExactly(t *testing.T) {
	if !schemaInteger(json.Number("1e400")) {
		t.Error("schemaInteger(1e400) = false: it is an integer")
	}
	for _, kind := range []string{"integer", "number"} {
		out := anthropicSchema(map[string]any{"enum": []any{json.Number("1e400"), nil}, "type": []any{kind, "null"}})
		raw, _ := json.Marshal(out["enum"])
		if string(raw) != "[1e400,null]" {
			t.Errorf("%s: enum = %s, want [1e400,null]", kind, raw)
		}
	}
}

func TestEnumUnderATypeListKeepsALongIntegerWithoutADrop(t *testing.T) {
	long := strings.Repeat("9", 129)
	for _, kind := range []string{"integer", "number"} {
		out := anthropicSchema(map[string]any{"enum": []any{json.Number(long), nil}, "type": []any{kind, "null"}})
		raw, _ := json.Marshal(out["enum"])
		if string(raw) != "["+long+",null]" {
			t.Errorf("%s: enum = %s, want the 129-digit value kept", kind, raw)
		}
	}
	if !schemaInteger(json.Number(long)) || schemaInteger(json.Number("1e-999999999")) || schemaInteger(json.Number("1."+long)) {
		t.Error("schemaInteger is wrong for a long integer, a tiny exponent or a long fraction")
	}
}

func TestSchemaIntegerOfHugeTextAllocatesLittle(t *testing.T) {
	digits := strings.Repeat("9", 100000)
	cases := map[string]bool{"1e999999999": true, "1e-999999999": false, digits: true, "1." + digits: false, digits + "e5": true, "0e-999999999": true}
	for text, want := range cases {
		value := json.Number(text)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		got := schemaInteger(value)
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		if got != want {
			t.Errorf("%.12s...: schemaInteger = %v, want %v", text, got, want)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<10 {
			t.Errorf("%.12s...: allocated %d bytes, want under 64 KiB", text, allocated)
		}
		if elapsed > 250*time.Millisecond {
			t.Errorf("%.12s...: took %v, want under 250ms", text, elapsed)
		}
	}
}

func TestSchemaIntegerExponentsAtTheEdgesOfIntRange(t *testing.T) {
	for text, want := range map[string]bool{
		"1e-9223372036854775808":    false,
		"1.5e-9223372036854775808":  false,
		"10.0e-9223372036854775808": false,
		"0e-9223372036854775808":    true,
		"0.0e-9223372036854775808":  true,
		"1e-9223372036854775807":    false,
		"1.5e-9223372036854775807":  false,
		"0.00e-9223372036854775807": true,
		"1e9223372036854775807":     true,
		"1.5e9223372036854775807":   true,
		"0.5e9223372036854775807":   true,
		"1.5e9223372036854775806":   true,
		"1.5e92233720368547758080":  true,
		"1.5e+92233720368547758080": true,
		"1.5e-92233720368547758090": false,
		"1.0e-92233720368547758090": false,
		"0.0e-92233720368547758090": true,
		"15e-1":                     false,
		"150e-2":                    false,
		"100e-2":                    true,
		"1.50e1":                    true,
		"1.55e1":                    false,
		"-1.5e-9223372036854775808": false,
		"-0.0e-9223372036854775808": true,
	} {
		if got := schemaInteger(json.Number(text)); got != want {
			t.Errorf("schemaInteger(%s) = %v, want %v", text, got, want)
		}
	}
}

func TestEnumUnderATypeListKeepsOnlyWholeNumbersAtExtremeExponents(t *testing.T) {
	out := anthropicSchema(map[string]any{"type": []any{"integer", "null"}, "enum": []any{json.Number("1.5e-9223372036854775808"), json.Number("1e9223372036854775807"), nil}})
	raw, _ := json.Marshal(out["enum"])
	if string(raw) != "[1e9223372036854775807,null]" {
		t.Errorf("enum = %s, want the fractional value dropped and the huge whole value kept", raw)
	}
}

func oracleNumberTextIsInteger(t *testing.T, text string) bool {
	t.Helper()
	text = strings.TrimPrefix(text, "-")
	exponent := new(big.Int)
	if at := strings.IndexAny(text, "eE"); at >= 0 {
		if _, ok := exponent.SetString(strings.TrimPrefix(text[at+1:], "+"), 10); !ok {
			t.Fatalf("bad exponent in %q", text)
		}
		text = text[:at]
	}
	whole, fraction, _ := strings.Cut(text, ".")
	digits := whole + fraction
	shift := new(big.Int).Sub(exponent, big.NewInt(int64(len(fraction))))
	if shift.Sign() >= 0 {
		return true
	}
	drop := new(big.Int).Neg(shift)
	value, _ := new(big.Int).SetString(digits, 10)
	if drop.Cmp(big.NewInt(int64(len(digits)))) > 0 {
		return value.Sign() == 0
	}
	modulus := new(big.Int).Exp(big.NewInt(10), drop, nil)
	return new(big.Int).Mod(value, modulus).Sign() == 0
}

func TestSchemaIntegerAgreesWithAnOverflowFreeOracleAcrossTheFullExponentRange(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	edges := []string{"9223372036854775807", "9223372036854775806", "9223372036854775808", "9223372036854775809", "99999999999999999999999", "0", "1", "2", "3", "4", "5"}
	pick := func() string {
		switch random.Intn(4) {
		case 0:
			return edges[random.Intn(len(edges))]
		case 1:
			return strconv.FormatInt(random.Int63(), 10)
		case 2:
			return strconv.Itoa(random.Intn(12))
		}
		return strconv.FormatInt(random.Int63n(1<<40), 10)
	}
	digitRun := func(n int) string {
		out := make([]byte, n)
		for i := range out {
			out[i] = byte('0' + random.Intn(3))
		}
		return string(out)
	}
	for i := 0; i < 100000; i++ {
		text := digitRun(1 + random.Intn(4))
		if random.Intn(2) == 0 {
			text += "." + digitRun(1+random.Intn(4))
		}
		sign := []string{"", "+", "-"}[random.Intn(3)]
		text += "e" + sign + pick()
		if random.Intn(5) == 0 {
			text = "-" + text
		}
		if want, got := oracleNumberTextIsInteger(t, text), schemaInteger(json.Number(text)); got != want {
			t.Fatalf("schemaInteger(%s) = %v, oracle %v", text, got, want)
		}
	}
}

func TestSchemaIntegerWithLongDigitRunsAtTheExponentBounds(t *testing.T) {
	nines := strings.Repeat("9", 25)
	zeros := strings.Repeat("0", 25)
	cases := map[string]bool{
		nines + "e-9223372036854775808":               false,
		nines + "e-9223372036854775809":               false,
		nines + ".5e-9223372036854775808":             false,
		nines + "e9223372036854775807":                true,
		nines + ".5e9223372036854775808":              true,
		nines + ".5e92233720368547758070":             true,
		zeros + "e-9223372036854775808":               true,
		zeros + "." + zeros + "e-9223372036854775809": true,
		"1" + zeros + "e-1099511627776":               false,
		"1" + zeros + "e-1099511627777":               false,
		"1" + zeros + "e-1099511627775":               false,
		"1" + zeros + "e1099511627776":                true,
		"1" + zeros + "e1099511627777":                true,
		"1." + zeros + "5e-1099511627776":             false,
		"0." + zeros + "e-1099511627776":              true,
		"0." + zeros + "e-1099511627777":              true,
		"123456789012345678901234.5e1":                true,
		"123456789012345678901234.5e0":                false,
		"1234567890123456789012340e-1":                true,
		"1234567890123456789012345e-1":                false,
		"1000000000000000000000000e-24":               true,
		"1000000000000000000000000e-25":               false,
		"1000000000000000000000000e-26":               false,
		"1000000000000000000000000.000e-27":           false,
		"1000000000000000000000000.000e-3":            true,
		"1000000000000000000000000.0001e-3":           false,
	}
	for text, want := range cases {
		if got := schemaInteger(json.Number(text)); got != want {
			t.Errorf("schemaInteger(%s) = %v, want %v", text, got, want)
		}
		if oracle := oracleNumberTextIsInteger(t, text); oracle != want {
			t.Errorf("oracle(%s) = %v, but the row says %v", text, oracle, want)
		}
	}
}

func TestSchemaIntegerAgreesWithTheOracleOnLongDigitRunsNearTheirLength(t *testing.T) {
	random := rand.New(rand.NewSource(11))
	digitRun := func(n int) string {
		out := make([]byte, n)
		for i := range out {
			out[i] = byte('0' + random.Intn(2)*random.Intn(10))
		}
		return string(out)
	}
	for i := 0; i < 50000; i++ {
		whole := digitRun(1 + random.Intn(40))
		text := whole
		fraction := ""
		if random.Intn(2) == 0 {
			fraction = digitRun(1 + random.Intn(40))
			text += "." + fraction
		}
		exponent := random.Intn(2*(len(whole)+len(fraction))+8) - (len(whole) + len(fraction) + 4)
		text += "e" + strconv.Itoa(exponent)
		if want, got := oracleNumberTextIsInteger(t, text), schemaInteger(json.Number(text)); got != want {
			t.Fatalf("schemaInteger(%s) = %v, oracle %v", text, got, want)
		}
	}
}
