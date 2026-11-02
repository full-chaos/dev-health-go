package pep

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func sortedReasons(rs []Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	sort.Strings(out)
	return out
}

func schemaProp(t *testing.T, file string, path ...string) map[string]any {
	t.Helper()
	var doc map[string]any
	readJSON(t, &doc, "jsonschema", file)
	cur := doc
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("%s: no %q", file, p)
		}
		cur = next
	}
	return cur
}

func enumOf(t *testing.T, node map[string]any) []string {
	t.Helper()
	raw, ok := node["enum"].([]any)
	if !ok {
		t.Fatalf("no enum in %v", node)
	}
	var out []string
	for _, v := range raw {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return out
}

func TestWireReasonsEqualTheContract(t *testing.T) {
	want := enumOf(t, schemaProp(t, "authorization_decision.v1.schema.json", "properties", "reason_code"))
	if got := sortedReasons(WireReasons()); !reflect.DeepEqual(got, want) {
		t.Fatalf("WireReasons differ from the decision schema enum:\n got %v\nwant %v", got, want)
	}
	var vocab struct {
		Decision struct {
			Values []string `json:"values"`
		} `json:"decision_reason_code"`
	}
	readJSON(t, &vocab, "vocabulary", "reason-codes.json")
	sort.Strings(vocab.Decision.Values)
	if got := sortedReasons(WireReasons()); !reflect.DeepEqual(got, vocab.Decision.Values) {
		t.Fatalf("WireReasons differ from the vocabulary decision_reason_code:\n got %v\nwant %v", got, vocab.Decision.Values)
	}
}

func TestEveryWireReasonIsAnAllowOrADenyReason(t *testing.T) {
	allows := 0
	for _, r := range WireReasons() {
		if r.Local() {
			t.Errorf("%s is local", r)
		}
		if allowReasons[r] {
			allows++
		}
	}
	if allows != len(allowReasons) || allows != 3 {
		t.Fatalf("allow reasons: %d listed, %d in the wire set, want 3", len(allowReasons), allows)
	}
}

func TestLocalReasonsAreDisjointFromTheWire(t *testing.T) {
	wire := map[Reason]bool{}
	for _, r := range WireReasons() {
		wire[r] = true
	}
	if len(LocalReasons()) != 14 {
		t.Fatalf("%d local reasons", len(LocalReasons()))
	}
	for _, r := range LocalReasons() {
		if wire[r] || !r.Local() || len(r) < 5 || r[:4] != "pdp_" {
			t.Errorf("local reason %q", r)
		}
	}
}

func TestResourceTypesClientServicesAndObligationsEqualTheContract(t *testing.T) {
	types := enumOf(t, schemaProp(t, "authorization_request.v1.schema.json", "properties", "resource", "properties", "type"))
	if got := sortedStrings(resourceTypes); !reflect.DeepEqual(got, types) {
		t.Fatalf("resource types:\n got %v\nwant %v", got, types)
	}
	var profile map[string]any
	readJSON(t, &profile, "endpoint", "endpoint-profile.schema.json")
	service := profile["$defs"].(map[string]any)["endpointProfile"].(map[string]any)["properties"].(map[string]any)["service"].(map[string]any)
	if got := sortedStrings(clientServices); !reflect.DeepEqual(got, enumOf(t, service)) {
		t.Fatalf("client services:\n got %v\nwant %v", got, enumOf(t, service))
	}
	ob := schemaProp(t, "authorization_decision.v1.schema.json", "properties", "obligations", "properties")
	var names []string
	for k := range ob {
		names = append(names, k)
	}
	sort.Strings(names)
	if got := sortedStrings(obligationNames); !reflect.DeepEqual(got, names) {
		t.Fatalf("obligation members:\n got %v\nwant %v", got, names)
	}
	redaction := ob["redaction"].(map[string]any)["items"].(map[string]any)
	if got := sortedStrings(redactionClasses); !reflect.DeepEqual(got, enumOf(t, redaction)) {
		t.Fatalf("redaction classes:\n got %v\nwant %v", got, enumOf(t, redaction))
	}
	var principal map[string]any
	readJSON(t, &principal, "jsonschema", "principal.v1.schema.json")
	assurance := principal["properties"].(map[string]any)["authentication"].(map[string]any)["properties"].(map[string]any)["assurance"].(map[string]any)
	if got := sortedStrings(assuranceLevels); !reflect.DeepEqual(got, enumOf(t, assurance)) {
		t.Fatalf("assurance levels:\n got %v\nwant %v", got, enumOf(t, assurance))
	}
}

func TestBatchBoundEqualsTheContract(t *testing.T) {
	var doc struct {
		Properties struct {
			Requests struct {
				Max int `json:"maxItems"`
			} `json:"requests"`
		} `json:"properties"`
	}
	readJSON(t, &doc, "jsonschema", "authorization_batch_request.v1.schema.json")
	if doc.Properties.Requests.Max != MaxBatch {
		t.Fatalf("MaxBatch %d, contract %d", MaxBatch, doc.Properties.Requests.Max)
	}
}

func TestPatternsEqualTheContract(t *testing.T) {
	check := func(name, got, file string, path ...string) {
		t.Helper()
		node := schemaProp(t, file, path...)
		if want, _ := node["pattern"].(string); got != want {
			t.Errorf("%s pattern:\n got %s\nwant %s", name, got, want)
		}
	}
	check("principal_reference", principalReferencePattern, "authorization_request.v1.schema.json", "properties", "principal_reference")
	check("decision_id", decisionIDPattern.String(), "authorization_decision.v1.schema.json", "properties", "decision_id")
	check("request_id", requestIDPattern.String(), "error.v1.schema.json", "properties", "request_id")
	check("principal_id", principalIDPattern.String(), "principal.v1.schema.json", "properties", "principal_id")
	check("action", actionPattern.String(), "principal.v1.schema.json", "properties", "actor_chain", "items", "properties", "permitted_actions", "items")
	check("expires_at", expiresAtPattern.String(), "principal.v1.schema.json", "properties", "expires_at")
	_ = json.Marshal
}
