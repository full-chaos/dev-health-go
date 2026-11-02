package pep

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type manifestRow struct {
	File string `json:"file"`
}

type manifest struct {
	Accept         []manifestRow   `json:"accept"`
	Reject         []manifestRow   `json:"reject"`
	RejectByClient []manifestRow   `json:"reject_by_client"`
	Request        json.RawMessage `json:"request"`
}

func readManifest(t testing.TB, set string) manifest {
	t.Helper()
	var m manifest
	readJSON(t, &m, set, "manifest.json")
	if len(m.Accept) == 0 || len(m.Reject) == 0 {
		t.Fatalf("%s manifest has an empty category", set)
	}
	return m
}

type wireDoc struct {
	Allowed        bool            `json:"allowed"`
	ReasonCode     string          `json:"reason_code"`
	OrganizationID *string         `json:"organization_id"`
	PolicyRevision int64           `json:"policy_revision"`
	DecisionID     string          `json:"decision_id"`
	PrincipalID    string          `json:"principal_id"`
	Obligations    json.RawMessage `json:"obligations"`
}

// requestFor is the request a fixture decision answers: its organization.
func requestFor(doc wireDoc) Request {
	r := baseRequest()
	r.OrganizationID = ""
	if doc.OrganizationID != nil {
		r.OrganizationID = *doc.OrganizationID
	}
	if r.OrganizationID == "" {
		r.ResourceType, r.ResourceKey, r.Action = "signing_key", "key-2026-09", "admin.orgs"
	}
	return r
}

// rejectClass is the closed local reason a refused fixture must produce. A
// file not listed is a malformed document.
var rejectClass = map[string]Reason{
	"decision-client-expired-allow.json":            ReasonExpired,
	"decision-invalid-unknown-obligation-key.json":  ReasonUnknownObligation,
	"decision-invalid-unknown-redaction-class.json": ReasonUnknownObligation,
	"decision-invalid-unknown-assurance.json":       ReasonUnknownObligation,
}

func decideBody(t *testing.T, body []byte, req Request) Decision {
	t.Helper()
	pdp := newPDP(t, replyJSON(body))
	return pdp.client(t).Decide(context.Background(), req)
}

func TestGoldenDecisionAccept(t *testing.T) {
	m := readManifest(t, "authorization_decision")
	for _, row := range m.Accept {
		t.Run(row.File, func(t *testing.T) {
			raw := readFile(t, "authorization_decision", row.File)
			var doc wireDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			d := decideBody(t, raw, requestFor(doc))
			if d.Local() || d.Err() != nil {
				t.Fatalf("a valid decision became a local deny: %v", d.Err())
			}
			if d.Allowed() != doc.Allowed || string(d.Reason()) != doc.ReasonCode {
				t.Fatalf("allowed=%v reason=%s, want %v %s", d.Allowed(), d.Reason(), doc.Allowed, doc.ReasonCode)
			}
			if int64(d.PolicyRevision()) != doc.PolicyRevision || d.DecisionID() != doc.DecisionID || d.PrincipalID() != doc.PrincipalID {
				t.Fatalf("ids/revision differ: %s %s %d", d.DecisionID(), d.PrincipalID(), d.PolicyRevision())
			}
			if org, ok := d.OrganizationID(); ok != (doc.OrganizationID != nil) || (ok && org != *doc.OrganizationID) {
				t.Fatalf("organization %q %v", org, ok)
			}
			if d.ExpiresAt().IsZero() {
				t.Fatal("no expiry")
			}
		})
	}
}

func TestGoldenDecisionRefused(t *testing.T) {
	m := readManifest(t, "authorization_decision")
	rows := append(append([]manifestRow{}, m.Reject...), m.RejectByClient...)
	for _, row := range rows {
		t.Run(row.File, func(t *testing.T) {
			raw := readFile(t, "authorization_decision", row.File)
			var doc wireDoc
			_ = json.Unmarshal(raw, &doc)
			d := decideBody(t, raw, requestFor(doc))
			if d.Allowed() {
				t.Fatal("a refused document was allowed")
			}
			want, ok := rejectClass[row.File]
			if !ok {
				want = ReasonMalformed
			}
			if !d.Local() || d.Reason() != want {
				t.Fatalf("reason %s local=%v, want local %s", d.Reason(), d.Local(), want)
			}
		})
	}
}

func batchRequests(t testing.TB, m manifest) []Request {
	t.Helper()
	var doc struct {
		Requests []struct {
			PrincipalReference string `json:"principal_reference"`
			Action             string `json:"action"`
			Resource           struct {
				Type, Key      string
				OrganizationID string `json:"organization_id"`
			} `json:"resource"`
			Context struct {
				RequestID string `json:"request_id"`
			} `json:"context"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(m.Request, &doc); err != nil {
		t.Fatal(err)
	}
	var out []Request
	for _, q := range doc.Requests {
		out = append(out, Request{PrincipalReference: q.PrincipalReference, Action: q.Action, ResourceType: q.Resource.Type, ResourceKey: q.Resource.Key, OrganizationID: q.Resource.OrganizationID, RequestID: q.Context.RequestID})
	}
	return out
}

func TestGoldenBatchAccept(t *testing.T) {
	m := readManifest(t, "authorization_batch_response")
	reqs := batchRequests(t, m)
	for _, row := range m.Accept {
		pdp := newPDP(t, replyJSON(readFile(t, "authorization_batch_response", row.File)))
		got := pdp.client(t).DecideBatch(context.Background(), reqs)
		if len(got) != 2 || !got[0].Allowed() || got[1].Allowed() || got[1].Local() {
			t.Fatalf("%s: item 0 allow and item 1 wire deny expected, got %+v", row.File, got)
		}
		if path := <-pdp.paths; path != "POST /v1/authorize:batch" {
			t.Fatalf("path %s", path)
		}
	}
}

var batchClientClass = map[string]Reason{
	"batch-response-client-fewer-decisions.json":    ReasonMismatch,
	"batch-response-client-more-decisions.json":     ReasonMismatch,
	"batch-response-client-swapped-index.json":      ReasonMismatch,
	"batch-response-client-duplicate-index.json":    ReasonMismatch,
	"batch-response-client-skipped-index.json":      ReasonMismatch,
	"batch-response-client-expired-item.json":       ReasonExpired,
	"batch-response-client-decimal-form-index.json": ReasonMalformed,
}

func TestGoldenBatchRefusedEntirely(t *testing.T) {
	m := readManifest(t, "authorization_batch_response")
	reqs := batchRequests(t, m)
	rows := append(append([]manifestRow{}, m.Reject...), m.RejectByClient...)
	for _, row := range rows {
		t.Run(row.File, func(t *testing.T) {
			pdp := newPDP(t, replyJSON(readFile(t, "authorization_batch_response", row.File)))
			got := pdp.client(t).DecideBatch(context.Background(), reqs)
			if len(got) != len(reqs) {
				t.Fatalf("%d decisions for %d requests", len(got), len(reqs))
			}
			for i, d := range got {
				if d.Allowed() || !d.Local() {
					t.Fatalf("item %d: allowed=%v local=%v: one bad binding must deny the whole batch", i, d.Allowed(), d.Local())
				}
				if want, ok := batchClientClass[row.File]; ok && d.Reason() != want {
					t.Fatalf("item %d: reason %s, want %s", i, d.Reason(), want)
				}
			}
		})
	}
}

func TestGoldenRequestBodies(t *testing.T) {
	m := readManifest(t, "authorization_request")
	for _, row := range m.Accept {
		t.Run(row.File, func(t *testing.T) {
			raw := readFile(t, "authorization_request", row.File)
			req := requestFromFixture(t, raw)
			pdp := newPDP(t, replyBytes(500, "", nil))
			pdp.client(t).Decide(context.Background(), req)
			sent := <-pdp.bodies
			var got, want any
			if err := json.Unmarshal(sent, &got); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(raw, &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("sent %s\nwant %s", sent, raw)
			}
		})
	}
}

func requestFromFixture(t testing.TB, raw []byte) Request {
	t.Helper()
	var q struct {
		PrincipalReference string `json:"principal_reference"`
		Action             string `json:"action"`
		Resource           struct {
			Type, Key      string
			OrganizationID string `json:"organization_id"`
		} `json:"resource"`
		Context struct {
			RequestID string `json:"request_id"`
		} `json:"context"`
	}
	_ = json.Unmarshal(raw, &q)
	return Request{PrincipalReference: q.PrincipalReference, Action: q.Action, ResourceType: q.Resource.Type, ResourceKey: q.Resource.Key, OrganizationID: q.Resource.OrganizationID, RequestID: q.Context.RequestID}
}

func TestGoldenInvalidRequestsDenyWithoutACall(t *testing.T) {
	representable := map[string]bool{
		"request-invalid-bare-action.json":                    true,
		"request-invalid-missing-action.json":                 true,
		"request-invalid-missing-principal-reference.json":    true,
		"request-invalid-principal-reference-with-space.json": true,
		"request-invalid-repository-key-uppercase.json":       true,
		"request-invalid-star-on-non-repository.json":         true,
		"request-invalid-unknown-resource-type.json":          true,
	}
	m := readManifest(t, "authorization_request")
	seen := 0
	for _, row := range m.Reject {
		if !representable[row.File] {
			continue
		}
		seen++
		t.Run(row.File, func(t *testing.T) {
			pdp := newPDP(t, replyBytes(500, "", nil))
			d := pdp.client(t).Decide(context.Background(), requestFromFixture(t, readFile(t, "authorization_request", row.File)))
			if d.Allowed() || d.Reason() != ReasonInvalidRequest || pdp.calls.Load() != 0 {
				t.Fatalf("reason %s calls %d: an invalid request must deny locally without a call", d.Reason(), pdp.calls.Load())
			}
		})
	}
	if seen != len(representable) {
		t.Fatalf("the manifest holds %d of the %d expected invalid requests", seen, len(representable))
	}
}

func TestEveryRefusedFixtureNamesItsPath(t *testing.T) {
	for _, set := range []string{"authorization_decision", "authorization_batch_response", "authorization_request", "authorization_batch_request"} {
		m := readManifest(t, set)
		for _, row := range append(append(append([]manifestRow{}, m.Accept...), m.Reject...), m.RejectByClient...) {
			if strings.Contains(row.File, "/") {
				t.Fatalf("%s: %s", set, row.File)
			}
			readFile(t, set, row.File)
		}
	}
}
