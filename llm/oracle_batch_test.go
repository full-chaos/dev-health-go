package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"testing"
)

// The differential oracle of the Batch API: testdata/openai_batch.oracle.json
// holds, for ops' frozen-Python batch corpus, both the frozen Python answer
// (ops testdata/golden/batch.golden.json, recorded from openai.py at
// python_build) and the ops Go port's answer at ops_sha, with the request
// inputs each line was built from. llm must equal the ops port byte for byte
// (lines) and field for field (parse, statuses), and equal Python everywhere
// except ops' named divergences, which must still differ.

const openAIBatchOracleDigest = "1dc0cda0ae0dfd79b36835d28783a45fb9f042a25c5710d90b68ec30f0d0aac8"

type batchOracleFile struct {
	Header map[string]string `json:"header"`
	Lines  []struct {
		Name            string          `json:"name"`
		Model           string          `json:"model"`
		ConfigFloor     int             `json:"config_max_output_tokens"`
		CustomID        string          `json:"custom_id"`
		System          string          `json:"system"`
		Prompt          string          `json:"prompt"`
		FormatName      string          `json:"format_name"`
		Schema          json.RawMessage `json:"schema"`
		RequestFloor    int             `json:"request_max_output_tokens"`
		OpsLine         string          `json:"ops_line"`
		PythonLine      json.RawMessage `json:"python_line"`
		NamedDivergence bool            `json:"named_divergence"`
	} `json:"lines"`
	Parse []struct {
		Name            string          `json:"name"`
		Text            string          `json:"text"`
		OpsError        bool            `json:"ops_error"`
		OpsResults      json.RawMessage `json:"ops_results"`
		PythonError     bool            `json:"python_error"`
		PythonResults   json.RawMessage `json:"python_results"`
		NamedDivergence bool            `json:"named_divergence"`
	} `json:"parse"`
	Statuses     map[string]map[string]string `json:"statuses"`
	ResultFields []string                     `json:"result_fields"`
	MetadataKeys []string                     `json:"metadata_keys"`
}

// opsBatchItemResult is the ops port's BatchItemResult as it marshals.
type opsBatchItemResult struct {
	CustomID, Text, ErrorCode, ErrorMessage, LineID, RequestID string
	StatusCode, InputTokens, OutputTokens, CachedInputTokens   *int
	ProviderErrorCode, ProviderErrorType                       string
}

func TestBatchMatchesTheOpsPortAndFrozenPython(t *testing.T) {
	var oracle batchOracleFile
	loadOracle(t, "testdata/openai_batch.oracle.json", openAIBatchOracleDigest, &oracle)
	if oracle.Header["ops_sha"] == "" || oracle.Header["python_build"] == "" {
		t.Fatalf("header %v: not a recording", oracle.Header)
	}
	if got, want := sortedKeysOf(pythonResultFields), sortedStrings(oracle.ResultFields); !reflect.DeepEqual(got, want) {
		t.Fatalf("BatchItemResult fields: Python has %v, the comparator maps %v", want, got)
	}
	if got, want := sortedKeysOf(pythonMetadataFields), sortedStrings(oracle.MetadataKeys); !reflect.DeepEqual(got, want) {
		t.Fatalf("provider_metadata keys: Python sets %v, the comparator maps %v", want, got)
	}

	vsPython := func(name string, divergence, equal bool, detail string) {
		t.Helper()
		switch {
		case divergence && equal:
			t.Errorf("%s: a named divergence from Python is no longer found (the comparison or the case broke)", name)
		case !divergence && !equal:
			t.Errorf("%s: differs from Python: %s", name, detail)
		}
	}
	compared := 0
	for _, lc := range oracle.Lines {
		name := "lines/" + lc.Name
		request := Request{System: lc.System, Prompt: lc.Prompt, MaxOutputTokens: lc.RequestFloor}
		if string(lc.Schema) != "null" {
			var schema map[string]any
			decoder := json.NewDecoder(bytes.NewReader(lc.Schema))
			decoder.UseNumber()
			if err := decoder.Decode(&schema); err != nil {
				t.Fatal(err)
			}
			request.Format = &JSONFormat{Name: lc.FormatName, Schema: schema}
		}
		provider, err := NewOpenAI(Config{APIKey: NewSecret("oracle-placeholder"), Model: lc.Model, MaxOutputTokens: lc.ConfigFloor})
		if err != nil {
			t.Fatal(err)
		}
		line, err := json.Marshal(openAIBatchLine{CustomID: lc.CustomID, Method: "POST", URL: openAIBatchEndpoint,
			Body: provider.responsesBody(request, max(provider.cfg.MaxOutputTokens, request.MaxOutputTokens))})
		if err != nil {
			t.Fatal(err)
		}
		if string(line) != lc.OpsLine {
			t.Errorf("%s: differs from the ops port's bytes:\n go  %s\n ops %s", name, line, lc.OpsLine)
		}
		tagged, err := tagJSON(line)
		if err != nil {
			t.Fatal(err)
		}
		equal, detail := taggedEqual(tagged, lc.PythonLine)
		vsPython(name, lc.NamedDivergence, equal, detail)
		compared++
	}
	for _, pc := range oracle.Parse {
		name := "parse/" + pc.Name
		results, err := parseOpenAIBatchLines([]byte(pc.Text))
		if (err != nil) != pc.OpsError {
			t.Errorf("%s: error=%v, ops port error=%v", name, err, pc.OpsError)
		}
		if err == nil {
			var ops []opsBatchItemResult
			if uerr := json.Unmarshal(pc.OpsResults, &ops); uerr != nil {
				t.Fatal(uerr)
			}
			var mine []opsBatchItemResult // nil for no line, as the ops port returns
			for _, r := range results {
				mine = append(mine, opsBatchItemResult{r.CustomID, r.Text, r.ErrorCode, r.ErrorMessage, r.LineID, r.RequestID,
					r.StatusCode, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CachedInputTokens, r.ProviderErrorCode, r.ProviderErrorType})
			}
			a, _ := json.Marshal(mine)
			b, _ := json.Marshal(ops)
			if !bytes.Equal(a, b) {
				t.Errorf("%s: differs from the ops port:\n go  %s\n ops %s", name, a, b)
			}
		}
		if pc.PythonError || err != nil {
			vsPython(name, pc.NamedDivergence, pc.PythonError == (err != nil), fmt.Sprintf("python error=%v go error=%v", pc.PythonError, err))
			compared++
			continue
		}
		rows := make([]any, 0, len(results))
		for _, result := range results {
			row := map[string]any{}
			for field, value := range pythonResultFields {
				row[field] = value(result)
			}
			rows = append(rows, row)
		}
		encoded, _ := json.Marshal(rows)
		tagged, err := tagJSON(encoded)
		if err != nil {
			t.Fatal(err)
		}
		equal, detail := taggedEqual(normalizeRawResponses(tagged), normalizeRawResponses(pc.PythonResults))
		vsPython(name, pc.NamedDivergence, equal, detail)
		compared++
	}
	for status, want := range oracle.Statuses {
		got := string(mapOpenAIBatchStatus(status))
		if got != want["ops"] || got != want["python"] {
			t.Errorf("status %q: %q, ops %q, python %q", status, got, want["ops"], want["python"])
		}
		compared++
	}
	t.Logf("compared %d cases (%d lines, %d parse, %d statuses)", compared, len(oracle.Lines), len(oracle.Parse), len(oracle.Statuses))
}

// pythonMetadataFields maps every key _batch_payload_metadata sets.
var pythonMetadataFields = map[string]func(BatchItemResult) (any, bool){
	"id":          func(r BatchItemResult) (any, bool) { return r.LineID, r.LineID != "" },
	"custom_id":   func(r BatchItemResult) (any, bool) { return r.CustomID, r.CustomID != "" },
	"request_id":  func(r BatchItemResult) (any, bool) { return r.RequestID, r.RequestID != "" },
	"status_code": func(r BatchItemResult) (any, bool) { return numberOrNil(r.StatusCode), r.StatusCode != nil },
	"input_tokens": func(r BatchItemResult) (any, bool) {
		return numberOrNil(r.Usage.InputTokens), r.Usage.InputTokens != nil
	},
	"output_tokens": func(r BatchItemResult) (any, bool) {
		return numberOrNil(r.Usage.OutputTokens), r.Usage.OutputTokens != nil
	},
	"error_code": func(r BatchItemResult) (any, bool) { return r.ProviderErrorCode, r.ProviderErrorCode != "" },
	"error_type": func(r BatchItemResult) (any, bool) { return r.ProviderErrorType, r.ProviderErrorType != "" },
}

// pythonResultFields maps every dataclass field of batch.py's BatchItemResult.
var pythonResultFields = map[string]func(BatchItemResult) any{
	"custom_id":     func(r BatchItemResult) any { return r.CustomID },
	"raw_response":  func(r BatchItemResult) any { return stringOrNil(r.Text) },
	"error_code":    func(r BatchItemResult) any { return stringOrNil(r.ErrorCode) },
	"error_message": func(r BatchItemResult) any { return stringOrNil(r.ErrorMessage) },
	"provider_metadata": func(r BatchItemResult) any {
		metadata := map[string]any{}
		for key, field := range pythonMetadataFields {
			if value, ok := field(r); ok {
				metadata[key] = value
			}
		}
		return metadata
	},
}

func numberOrNil(value *int) any {
	if value == nil {
		return nil
	}
	return json.Number(fmt.Sprint(*value))
}

func stringOrNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func sortedKeysOf[V any](table map[string]V) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// The comparator below is ops batch_frozen_python_test.go's, unchanged: every
// leaf is type-tagged so no number goes through float64, numbers compare as
// exact rationals, and a raw_response that is a JSON document compares as the
// document (openai.py re-encoded it with json.dumps).

func tagJSON(document []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(tagValue(value))
}

func tagValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return map[string]any{"t": "null", "v": ""}
	case bool:
		return map[string]any{"t": "bool", "v": fmt.Sprint(typed)}
	case json.Number:
		return map[string]any{"t": "number", "v": typed.String()}
	case string:
		return map[string]any{"t": "str", "v": typed}
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = tagValue(item)
		}
		return map[string]any{"t": "array", "v": items}
	case map[string]any:
		object := make(map[string]any, len(typed))
		for key, item := range typed {
			object[key] = tagValue(item)
		}
		return map[string]any{"t": "object", "v": object}
	}
	panic(fmt.Sprintf("untaggable %T", value))
}

type taggedNode struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v"`
}

func normalizeRawResponses(tagged json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(tagged))
	decoder.UseNumber()
	var document map[string]any
	if decoder.Decode(&document) != nil || document["t"] != "array" {
		return tagged
	}
	items, _ := document["v"].([]any)
	for _, item := range items {
		node, _ := item.(map[string]any)
		fields, _ := node["v"].(map[string]any)
		raw, _ := fields["raw_response"].(map[string]any)
		text, isText := raw["v"].(string)
		if raw["t"] != "str" || !isText || CompactJSON(text) == "" {
			continue
		}
		inner, err := tagJSON([]byte(text))
		if err != nil {
			continue
		}
		fields["raw_response"] = map[string]any{"t": "json", "v": inner}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return tagged
	}
	return encoded
}

func taggedEqual(left, right json.RawMessage) (bool, string) {
	var a, b taggedNode
	if err := json.Unmarshal(left, &a); err != nil {
		return false, err.Error()
	}
	if err := json.Unmarshal(right, &b); err != nil {
		return false, err.Error()
	}
	return taggedNodeEqual(a, b, "$")
}

func taggedNodeEqual(a, b taggedNode, path string) (bool, string) {
	if a.T != b.T {
		return false, fmt.Sprintf("%s: type go=%s python=%s", path, a.T, b.T)
	}
	switch a.T {
	case "json":
		var x, y taggedNode
		_ = json.Unmarshal(a.V, &x)
		_ = json.Unmarshal(b.V, &y)
		return taggedNodeEqual(x, y, path)
	case "object":
		var x, y map[string]taggedNode
		_ = json.Unmarshal(a.V, &x)
		_ = json.Unmarshal(b.V, &y)
		keys := map[string]struct{}{}
		for key := range x {
			keys[key] = struct{}{}
		}
		for key := range y {
			keys[key] = struct{}{}
		}
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			left, inLeft := x[key]
			right, inRight := y[key]
			if !inLeft || !inRight {
				return false, fmt.Sprintf("%s.%s: present go=%v python=%v", path, key, inLeft, inRight)
			}
			if equal, detail := taggedNodeEqual(left, right, path+"."+key); !equal {
				return false, detail
			}
		}
		return true, ""
	case "array":
		var x, y []taggedNode
		_ = json.Unmarshal(a.V, &x)
		_ = json.Unmarshal(b.V, &y)
		if len(x) != len(y) {
			return false, fmt.Sprintf("%s: length go=%d python=%d", path, len(x), len(y))
		}
		for index := range x {
			if equal, detail := taggedNodeEqual(x[index], y[index], fmt.Sprintf("%s[%d]", path, index)); !equal {
				return false, detail
			}
		}
		return true, ""
	case "number":
		var x, y string
		_ = json.Unmarshal(a.V, &x)
		_ = json.Unmarshal(b.V, &y)
		left, okLeft := new(big.Rat).SetString(x)
		right, okRight := new(big.Rat).SetString(y)
		if !okLeft || !okRight || left.Cmp(right) != 0 {
			return false, fmt.Sprintf("%s: number go=%s python=%s", path, x, y)
		}
		return true, ""
	}
	var x, y string
	if json.Unmarshal(a.V, &x) != nil || json.Unmarshal(b.V, &y) != nil {
		return false, fmt.Sprintf("%s: undecodable leaf", path)
	}
	if x != y {
		return false, fmt.Sprintf("%s: go=%q python=%q", path, x, y)
	}
	return true, ""
}

// TestBatchComparatorSeesEachKindOfDifference keeps the comparator honest.
func TestBatchComparatorSeesEachKindOfDifference(t *testing.T) {
	cases := []struct {
		name        string
		left, right string
		equal       bool
	}{
		{"same", `{"a":[1,"x",null,true]}`, `{"a":[1,"x",null,true]}`, true},
		{"0 and 0.0 are one number", `{"a":0}`, `{"a":0.0}`, true},
		{"number", `{"a":1}`, `{"a":2}`, false},
		{"string", `{"a":"x"}`, `{"a":"y"}`, false},
		{"type", `{"a":"1"}`, `{"a":1}`, false},
		{"missing key", `{"a":1}`, `{"a":1,"b":1}`, false},
		{"length", `[1]`, `[1,1]`, false},
		{"null vs string", `{"a":null}`, `{"a":""}`, false},
	}
	for _, tc := range cases {
		left, _ := tagJSON([]byte(tc.left))
		right, _ := tagJSON([]byte(tc.right))
		if equal, detail := taggedEqual(left, right); equal != tc.equal {
			t.Errorf("%s: equal=%v (%s), want %v", tc.name, equal, detail, tc.equal)
		}
	}
}
