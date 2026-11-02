package pep

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 3, 5, 0, 0, 0, time.UTC)

const (
	testOrg     = "org_EXAMPLE0000000000000001"
	testRequest = "req_EXAMPLE0000000000000000001"
)

func baseRequest() Request {
	return Request{
		PrincipalReference: "prf_EXAMPLE0000000000000001",
		Action:             "acr.context.read",
		ResourceType:       "repository",
		ResourceKey:        "full-chaos/dev-health-ops",
		OrganizationID:     testOrg,
		RequestID:          testRequest,
	}
}

type fakePDP struct {
	srv    *httptest.Server
	calls  atomic.Int32
	paths  chan string
	bodies chan []byte
}

func newPDP(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fakePDP {
	t.Helper()
	p := &fakePDP{paths: make(chan string, 64), bodies: make(chan []byte, 64)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		p.paths <- r.Method + " " + r.URL.Path
		p.bodies <- body
		h(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func replyBytes(status int, contentType string, body []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func replyJSON(body []byte) func(http.ResponseWriter, *http.Request) {
	return replyBytes(http.StatusOK, "application/json", body)
}

func (p *fakePDP) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: p.srv.URL, AllowPlainHTTP: true, HTTPClient: p.srv.Client(), ClientService: "dev-health-web", Timeout: 2 * time.Second, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readFile(t testing.TB, rel ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func readJSON(t testing.TB, v any, rel ...string) {
	t.Helper()
	if err := json.Unmarshal(readFile(t, rel...), v); err != nil {
		t.Fatalf("%v: %v", rel, err)
	}
}
