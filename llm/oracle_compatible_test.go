package llm

import (
	"context"
	"fmt"
	"testing"
)

// The differential oracle of the Chat Completions client:
// testdata/python_oracle/compatible.oracle.json holds what the frozen Python
// LocalProvider, GeminiProvider and QwenProvider (dev-health-ops
// llm/providers/local.py, gemini.py, qwen.py at the header's ops_sha, with the
// openai SDK) sent and returned for each case, recorded by executing them
// (driver.py, testdata/README.md). Each case runs here through llm.Compatible
// of the same kind against the same recorded answers. The Python body is the
// oracle for the request (path, bearer key, every body field, the 400 retry's
// response_format); the Python result is the oracle for the parse (text, model,
// usage, error class, number of HTTP calls).
//
// Inputs a Python provider derived from the prompt are caller inputs here:
// the Go request takes the system text the Python one sent, and the Format
// (name and schema) of the response_format it sent, if any.

const compatiblePythonOracleDigest = "92325ccb55dc34c546d959784d208fd0a843a3d7876e16815e86887cad3d8371"

// compatibleDivergences are the cases this client differs on, on purpose.
// There are none: every case equals the Python provider.
var compatibleDivergences = map[string]string{}

func TestCompatibleMatchesThePythonProviders(t *testing.T) {
	header, cases := loadPythonOracle(t, "testdata/python_oracle/compatible.oracle.json", compatiblePythonOracleDigest)
	t.Logf("oracle: %v at ops %v, sdk %v", header["producer"], header["ops_sha"], header["sdk"])
	kinds := map[string]int{}
	for _, oc := range cases {
		kinds[oc.Kind]++
		server, sent := servePythonAnswers(t, oc.Answers)
		first := plainJSON(t, oc.Requests[0]["body"]).(map[string]any)
		messages := first["messages"].([]any)
		request := Request{System: messages[0].(map[string]any)["content"].(string), Prompt: oc.Prompt}
		if format, ok := first["response_format"].(map[string]any); ok && format["type"] == "json_schema" {
			spec := format["json_schema"].(map[string]any)
			request.Format = &JSONFormat{Name: spec["name"].(string), Schema: spec["schema"].(map[string]any)}
		}
		provider, err := NewCompatible(Config{Kind: Kind(oc.Kind), APIKey: NewSecret("oracle-placeholder"),
			BaseURL: server.URL + "/v1", Model: first["model"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := provider.Complete(context.Background(), request)
		got := sent()

		equal, detail := true, ""
		fail := func(format string, args ...any) {
			if equal {
				equal, detail = false, fmt.Sprintf(format, args...)
			}
		}
		if len(got) != len(oc.Requests) {
			fail("calls go=%d python=%d", len(got), len(oc.Requests))
		}
		for i := 0; i < min(len(got), len(oc.Requests)); i++ {
			python := oc.Requests[i]
			headers := python["headers"].(map[string]any)
			if got[i].path != python["path"] {
				fail("request %d path go=%s python=%s", i, got[i].path, python["path"])
			}
			if got[i].header.Get("Authorization") != headers["authorization"] {
				fail("request %d authorization go=%q python=%q", i, got[i].header.Get("Authorization"), headers["authorization"])
			}
			if ok, why := sameJSON(got[i].body, plainJSON(t, python["body"])); !ok {
				fail("request %d body: %s", i, why)
			}
		}
		class, _ := ClassOf(err)
		var typed *Error
		if asError(err, &typed) && string(typed.Provider) != oc.Kind {
			fail("error labelled %q, the Python provider_name is %q", typed.Provider, oc.Kind)
		}
		if want := oc.Outcome["error_class"].(string); string(class) != want {
			fail("error class go=%q python=%q (err %v)", class, want, err)
		} else if want == "" {
			if !sameText(result.Text, oc.Outcome["text"].(string)) {
				fail("text go=%q python=%q", result.Text, oc.Outcome["text"])
			}
			if result.Model != oc.Outcome["model"] {
				fail("model go=%q python=%q", result.Model, oc.Outcome["model"])
			}
			if !sameTokens(result.Usage.InputTokens, oc.Outcome["input_tokens"]) || !sameTokens(result.Usage.OutputTokens, oc.Outcome["output_tokens"]) {
				fail("tokens go=%v/%v python=%v/%v", deref(result.Usage.InputTokens), deref(result.Usage.OutputTokens), oc.Outcome["input_tokens"], oc.Outcome["output_tokens"])
			}
		}
		divergenceCheck(t, compatibleDivergences, oc.Name, equal, detail)
	}
	for _, kind := range []string{"local", "gemini", "qwen"} {
		if kinds[kind] == 0 {
			t.Errorf("the oracle has no %s case", kind)
		}
	}
	t.Logf("compared %d executed Python cases %v, %d declared divergences", len(cases), kinds, len(compatibleDivergences))
}

func TestCompatiblePythonOracleReExecutes(t *testing.T) {
	reExecutePythonOracle(t, "compatible", "testdata/python_oracle/compatible.oracle.json")
}
