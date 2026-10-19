package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
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
