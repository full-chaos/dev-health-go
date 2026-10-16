package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Helpers of the recorded-response goldens (testdata/*_responses.golden.json).
// A golden stores every value type-tagged ({"t": type, "v": string}); untag
// turns it back into plain Go values with every number a json.Number, so no
// number is compared through float64.

func untag(node any) any {
	tagged, ok := node.(map[string]any)
	if !ok {
		panic(fmt.Sprintf("untagged node %T", node))
	}
	value := tagged["v"]
	switch tagged["t"] {
	case "null":
		return nil
	case "bool":
		return value == "true"
	case "number":
		return json.Number(value.(string))
	case "str":
		return value.(string)
	case "array":
		items := value.([]any)
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = untag(item)
		}
		return out
	case "object":
		fields := value.(map[string]any)
		out := make(map[string]any, len(fields))
		for key, item := range fields {
			out[key] = untag(item)
		}
		return out
	}
	panic(fmt.Sprintf("unknown tag %v", tagged["t"]))
}

// recordedCase is one golden case: the answers served and the outcome the
// callers rely on.
type recordedCase struct {
	Name     string
	Kind     string
	Prompt   string
	Answers  []map[string]any
	Requests []map[string]any
	Outcome  map[string]any
}

func loadRecordedCases(t *testing.T, path, digest string) (map[string]any, []recordedCase) {
	t.Helper()
	var file struct {
		Header map[string]any `json:"header"`
		Cases  []any          `json:"cases"`
	}
	loadOracle(t, path, digest, &file)
	if file.Header["ops_sha"] == "" || len(file.Cases) == 0 {
		t.Fatalf("%s: header %v, %d cases: not a recording", path, file.Header, len(file.Cases))
	}
	cases := make([]recordedCase, 0, len(file.Cases))
	for _, raw := range file.Cases {
		c := untag(raw).(map[string]any)
		entry := recordedCase{Name: c["name"].(string), Prompt: c["prompt"].(string), Outcome: c["outcome"].(map[string]any)}
		if kind, ok := c["kind"].(string); ok {
			entry.Kind = kind
		}
		for _, a := range c["answers"].([]any) {
			entry.Answers = append(entry.Answers, a.(map[string]any))
		}
		for _, r := range c["requests"].([]any) {
			entry.Requests = append(entry.Requests, r.(map[string]any))
		}
		cases = append(cases, entry)
	}
	return file.Header, cases
}

// sentRequest is one request this package sent to the scripted server.
type sentRequest struct {
	path   string
	header http.Header
	body   any
}

// serveRecordedAnswers serves a case's recorded answers in order, repeating the
// last, and records what was sent.
func serveRecordedAnswers(t *testing.T, answers []map[string]any) (*httptest.Server, func() []sentRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		sent []sentRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		_ = decoder.Decode(&body)
		mu.Lock()
		index := min(len(sent), len(answers)-1)
		sent = append(sent, sentRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
		answer := answers[index]
		if headers, ok := answer["headers"].(map[string]any); ok {
			for k, v := range headers {
				w.Header().Set(k, v.(string))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		status, _ := answer["status"].(json.Number).Int64()
		w.WriteHeader(int(status))
		_, _ = w.Write([]byte(answer["body"].(string)))
	}))
	t.Cleanup(server.Close)
	return server, func() []sentRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]sentRequest(nil), sent...)
	}
}

// plainJSON decodes a JSON document with numbers kept as json.Number.
func plainJSON(t *testing.T, raw any) any {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameJSON compares two decoded documents; numbers compare as exact
// rationals (JSON has one number type: 4096 and 4096.0 are one value).
func sameJSON(a, b any) (bool, string) {
	return sameJSONAt(a, b, "$")
}

func sameJSONAt(a, b any, path string) (bool, string) {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return false, fmt.Sprintf("%s: object vs %T", path, b)
		}
		keys := map[string]struct{}{}
		for k := range x {
			keys[k] = struct{}{}
		}
		for k := range y {
			keys[k] = struct{}{}
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			left, inLeft := x[k]
			right, inRight := y[k]
			if !inLeft || !inRight {
				return false, fmt.Sprintf("%s.%s: present go=%v want=%v", path, k, inLeft, inRight)
			}
			if ok, detail := sameJSONAt(left, right, path+"."+k); !ok {
				return false, detail
			}
		}
		return true, ""
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false, fmt.Sprintf("%s: array go=%v want=%v", path, a, b)
		}
		for i := range x {
			if ok, detail := sameJSONAt(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); !ok {
				return false, detail
			}
		}
		return true, ""
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false, fmt.Sprintf("%s: number go=%v want=%v", path, a, b)
		}
		left, okLeft := new(big.Rat).SetString(x.String())
		right, okRight := new(big.Rat).SetString(y.String())
		if !okLeft || !okRight || left.Cmp(right) != 0 {
			return false, fmt.Sprintf("%s: number go=%s want=%s", path, x, y)
		}
		return true, ""
	}
	if !reflect.DeepEqual(a, b) {
		return false, fmt.Sprintf("%s: go=%#v want=%#v", path, a, b)
	}
	return true, ""
}

// sameText compares completion texts: a JSON document on both sides compares
// as the document (the golden may hold json.dumps spacing; this package
// compacts), anything else byte for byte.
func sameText(goText, wantText string) bool {
	if goText == wantText {
		return true
	}
	if CompactJSON(goText) == "" || CompactJSON(wantText) == "" {
		return false
	}
	var a, b any
	da := json.NewDecoder(strings.NewReader(goText))
	da.UseNumber()
	db := json.NewDecoder(strings.NewReader(wantText))
	db.UseNumber()
	if da.Decode(&a) != nil || db.Decode(&b) != nil {
		return false
	}
	ok, _ := sameJSON(a, b)
	return ok
}

// tokenField is a golden token count as this package reports it.
func tokenField(value any) *int {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	n, err := number.Int64()
	if err != nil {
		return nil
	}
	out := int(n)
	return &out
}

func sameTokens(got *int, golden any) bool {
	want := tokenField(golden)
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}

// divergenceCheck reports a case against its declared divergence: a declared
// divergence that no longer differs means the comparison broke.
func divergenceCheck(t *testing.T, declared map[string]string, name string, equal bool, detail string) {
	t.Helper()
	reason, isDivergence := declared[name]
	switch {
	case isDivergence && equal:
		t.Errorf("%s: declared divergence (%s) no longer differs: the comparison or the case broke", name, reason)
	case isDivergence:
		t.Logf("%s: declared divergence (%s): %s", name, reason, detail)
	case !equal:
		t.Errorf("%s: differs from the caller-relied outcome: %s", name, detail)
	}
}
