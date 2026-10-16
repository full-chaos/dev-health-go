package llm

import (
	"context"
	"fmt"
	"testing"
)

// The differential oracle of the Anthropic client:
// testdata/python_oracle/anthropic.oracle.json holds what the frozen Python
// AnthropicProvider (dev-health-ops llm/providers/anthropic.py at the
// header's ops_sha, with its anthropic SDK) sent and returned for each case,
// recorded by executing it (driver.py, ../README.md). Each case runs here
// through llm.Anthropic against the same recorded answers. The Python body is
// the oracle for the request (path, x-api-key, anthropic-version, every body
// field); the Python result is the oracle for the parse (text, model, usage,
// error class, number of HTTP calls).
//
// The request inputs a Python provider derived from the prompt (its system
// text, picked by sniffing the prompt) are caller inputs in this package, so
// the Go request takes the system text the Python one sent.

const anthropicPythonOracleDigest = "db6fdf0b019531ce7b237f64e0f657a31f2b04b7d995c254dcd2176c5e46ac36"

// anthropicDivergences are the cases this client differs on, on purpose.
var anthropicDivergences = map[string]string{
	"divergence_cache_tokens": "input tokens count cache reads and writes, as the OpenAI client's do; " +
		"anthropic.py reported input_tokens alone",
	"divergence_two_text_blocks": "every text block is joined; anthropic.py read content[0] only",
	"divergence_thinking_first":  "thinking blocks are skipped; anthropic.py failed on a thinking block at content[0]",
	"divergence_overloaded_retries": "one retry in all (call_with_retry); the Anthropic SDK's own two internal " +
		"retries per attempt are not ported, so 2 calls, not 6",
	"divergence_rate_limit_retries": "same: 2 calls, not 6",
}

func TestAnthropicMatchesThePythonProvider(t *testing.T) {
	header, cases := loadPythonOracle(t, "testdata/python_oracle/anthropic.oracle.json", anthropicPythonOracleDigest)
	t.Logf("oracle: %v at ops %v, sdk %v", header["producer"], header["ops_sha"], header["sdk"])
	seen := map[string]bool{}
	for _, oc := range cases {
		seen[oc.Name] = true
		server, sent := servePythonAnswers(t, oc.Answers)
		first := oc.Requests[0]["body"].(map[string]any)
		provider, err := NewAnthropic(Config{APIKey: NewSecret("oracle-placeholder"), BaseURL: server.URL, Model: first["model"].(string)})
		if err != nil {
			t.Fatal(err)
		}
		request := Request{System: first["system"].(string), Prompt: oc.Prompt}
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
			for _, name := range []string{"x-api-key", "anthropic-version", "content-type"} {
				if got[i].header.Get(name) != headers[name] {
					fail("request %d header %s go=%q python=%q", i, name, got[i].header.Get(name), headers[name])
				}
			}
			if got[i].header.Get("Authorization") != "" {
				fail("request %d sends an Authorization header", i)
			}
			if ok, why := sameJSON(got[i].body, plainJSON(t, python["body"])); !ok {
				fail("request %d body: %s", i, why)
			}
		}
		class, _ := ClassOf(err)
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
		divergenceCheck(t, anthropicDivergences, oc.Name, equal, detail)
	}
	for name := range anthropicDivergences {
		if !seen[name] {
			t.Errorf("%s: declared divergence has no case", name)
		}
	}
	t.Logf("compared %d executed Python cases, %d declared divergences", len(cases), len(anthropicDivergences))
}

// TestAnthropicStructuredOutputIsTheOnlyRequestDifference: anthropic.py had
// no structured outputs. Under a Format this client adds output_config and
// nothing else; every other body field is the Python body's.
func TestAnthropicStructuredOutputIsTheOnlyRequestDifference(t *testing.T) {
	_, cases := loadPythonOracle(t, "testdata/python_oracle/anthropic.oracle.json", anthropicPythonOracleDigest)
	for _, oc := range cases {
		if oc.Name != "categorization_prompt_json" {
			continue
		}
		server, sent := servePythonAnswers(t, oc.Answers)
		python := plainJSON(t, oc.Requests[0]["body"]).(map[string]any)
		provider, _ := NewAnthropic(Config{APIKey: NewSecret("oracle-placeholder"), BaseURL: server.URL, Model: python["model"].(string)})
		if _, err := provider.Complete(context.Background(), Request{System: python["system"].(string), Prompt: oc.Prompt, Format: testFormat}); err != nil {
			t.Fatal(err)
		}
		body := sent()[0].body.(map[string]any)
		if _, ok := body["output_config"]; !ok {
			t.Fatal("no output_config under a Format")
		}
		delete(body, "output_config")
		if ok, why := sameJSON(body, python); !ok {
			t.Errorf("beyond output_config the body differs from Python: %s", why)
		}
		return
	}
	t.Fatal("the oracle has no categorization_prompt_json case")
}

func TestAnthropicPythonOracleReExecutes(t *testing.T) {
	reExecutePythonOracle(t, "anthropic", "testdata/python_oracle/anthropic.oracle.json")
}
