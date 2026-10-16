package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// No-regression guard for the ops adoption of this package (not a Python-parity
// claim): testdata/openai_sync.oracle.json holds what the ops Go port
// (dev-health-ops internal/jobs/investment/categorize OpenAIProvider.Complete,
// at the header's ops_sha) sent and returned for each case, recorded by running
// that code against scripted servers (testdata/README.md). Each case runs here
// through llm.OpenAI against the same scripted answers. Every request body must
// be byte-equal, so swapping ops onto this package changes nothing on the wire.
// The response side is caller-relied: text and JSON (the categorization
// parse), Usage (input, output, cached; nil = not reported, for budget and
// spend accounting), and the error class (retry and fallback decisions) must
// equal what ops' callers get today. A deliberate improvement over the ops
// port is named in the pull request and gets its own case outside this file.
// The oracle file is pinned by digest: a changed byte is a new recording, not
// an edit.

const openAISyncOracleDigest = "42480d41492fbaa147f4fe78f88be5a8e1edabf994cb2a6dfacb7e8151b3b6b5"

type syncOracleStep struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header"`
	Body   string            `json:"body"`
}

type syncOracleCase struct {
	Name            string           `json:"name"`
	Model           string           `json:"model"`
	ConfigFloor     int              `json:"config_max_output_tokens"`
	System          string           `json:"system"`
	Prompt          string           `json:"prompt"`
	FormatName      string           `json:"format_name"`
	Schema          json.RawMessage  `json:"schema"`
	RequestFloor    int              `json:"request_max_output_tokens"`
	Steps           []syncOracleStep `json:"steps"`
	SentBodies      []string         `json:"sent_bodies"`
	Calls           int              `json:"calls"`
	ResultText      string           `json:"result_text"`
	ResultModel     string           `json:"result_model"`
	InputTokens     *int             `json:"input_tokens"`
	OutputTokens    *int             `json:"output_tokens"`
	CachedTokens    *int             `json:"cached_input_tokens"`
	ErrorClass      string           `json:"error_class"`
	RetryAfterNanos int64            `json:"retry_after_nanos"`
}

func loadOracle(t *testing.T, path, digest string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256Hex(raw); got != digest {
		t.Fatalf("%s digest = %s, want %s: an oracle is re-recorded, never edited", path, got, digest)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(into); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAICallerReliedNoRegressionAgainstTheOpsPort(t *testing.T) {
	var oracle struct {
		Header map[string]string `json:"header"`
		Cases  []syncOracleCase  `json:"cases"`
	}
	loadOracle(t, "testdata/openai_sync.oracle.json", openAISyncOracleDigest, &oracle)
	if oracle.Header["ops_sha"] == "" || len(oracle.Cases) < 20 {
		t.Fatalf("oracle header %v, %d cases: not a recording", oracle.Header, len(oracle.Cases))
	}
	compared := 0
	for _, oc := range oracle.Cases {
		t.Run(oc.Name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				bodies []string
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				index := min(len(bodies), len(oc.Steps)-1)
				bodies = append(bodies, string(raw))
				mu.Unlock()
				s := oc.Steps[index]
				for k, v := range s.Header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(s.Status)
				_, _ = w.Write([]byte(s.Body))
			}))
			defer server.Close()

			request := Request{System: oc.System, Prompt: oc.Prompt, MaxOutputTokens: oc.RequestFloor}
			if string(oc.Schema) != "null" {
				var schema map[string]any
				decoder := json.NewDecoder(bytes.NewReader(oc.Schema))
				decoder.UseNumber()
				if err := decoder.Decode(&schema); err != nil {
					t.Fatal(err)
				}
				request.Format = &JSONFormat{Name: oc.FormatName, Schema: schema}
			}
			provider, err := NewOpenAI(Config{APIKey: NewSecret("oracle-placeholder"), BaseURL: server.URL, Model: oc.Model, MaxOutputTokens: oc.ConfigFloor})
			if err != nil {
				t.Fatal(err)
			}
			result, err := provider.Complete(context.Background(), request)

			if len(bodies) != oc.Calls {
				t.Fatalf("calls = %d, ops port made %d", len(bodies), oc.Calls)
			}
			for i := range bodies {
				if bodies[i] != oc.SentBodies[i] {
					t.Errorf("request %d differs from the ops port's bytes:\n go  %s\n ops %s", i, bodies[i], oc.SentBodies[i])
				}
			}
			class, _ := ClassOf(err)
			if string(class) != oc.ErrorClass {
				t.Errorf("error class = %q, ops port %q (err %v)", class, oc.ErrorClass, err)
			}
			if oc.ErrorClass != "" {
				var typed *Error
				if asError(err, &typed) && int64(typed.RetryAfter) != oc.RetryAfterNanos {
					t.Errorf("RetryAfter = %v, ops port %v", typed.RetryAfter, time.Duration(oc.RetryAfterNanos))
				}
			} else {
				if result.Text != oc.ResultText || result.Model != oc.ResultModel {
					t.Errorf("result %q/%q, ops port %q/%q", result.Text, result.Model, oc.ResultText, oc.ResultModel)
				}
				for _, pair := range []struct {
					name      string
					got, want *int
				}{{"input", result.Usage.InputTokens, oc.InputTokens}, {"output", result.Usage.OutputTokens, oc.OutputTokens}, {"cached", result.Usage.CachedInputTokens, oc.CachedTokens}} {
					if (pair.got == nil) != (pair.want == nil) || (pair.got != nil && *pair.got != *pair.want) {
						t.Errorf("%s tokens = %v, ops port %v", pair.name, deref(pair.got), deref(pair.want))
					}
				}
			}
			mu.Lock()
			compared++
			mu.Unlock()
		})
	}
	t.Logf("compared %d ops-port cases", compared)
	if compared != len(oracle.Cases) {
		t.Fatalf("compared %d of %d cases", compared, len(oracle.Cases))
	}
}

// TestOpenAIRetryConstantsArePinned fails when a floor, the ceiling, the retry
// count or the Retry-After handling changes; the oracle above exercises them,
// this names them.
func TestOpenAIRetryConstantsArePinned(t *testing.T) {
	pins := []struct {
		name      string
		got, want any
	}{
		{"openAIMinOutputTokens", openAIMinOutputTokens, 2048},
		{"openAIMaxOutputTokens", openAIMaxOutputTokens, 8192},
		{"openAIMaxRetries", openAIMaxRetries, 1},
		{"maxRetryAfterSecs", maxRetryAfterSecs, 60.0},
		{"baseRetryDelay", baseRetryDelay, 500 * time.Millisecond},
		{"maxRetryDelay", maxRetryDelay, 30 * time.Second},
		{"hardenedTimeout", hardenedTimeout, 60 * time.Second},
	}
	for _, pin := range pins {
		if pin.got != pin.want {
			t.Errorf("%s = %v, want %v", pin.name, pin.got, pin.want)
		}
	}
	server, c := scriptedServer(t, step{status: 429, body: `{}`, header: http.Header{"Retry-After": {"1"}}}, step{status: 200, body: openAIOK(`{}`)})
	provider, _ := NewOpenAI(Config{APIKey: NewSecret(testKey), BaseURL: server.URL})
	started := time.Now()
	if _, err := provider.Complete(context.Background(), Request{Prompt: "p"}); err != nil || c.calls() != 2 {
		t.Fatalf("err = %v calls = %d", err, c.calls())
	}
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Errorf("retried after %v: Retry-After 1 was not honoured", elapsed)
	}
}

func deref(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
