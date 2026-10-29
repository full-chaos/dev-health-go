package workloadtoken

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// wireHeaders records the header block each request would put on the wire (http.Header.Write).
type wireHeaders struct {
	mu    sync.Mutex
	wires []string
}

func (w *wireHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	var buf bytes.Buffer
	_ = req.Header.Write(&buf)
	w.mu.Lock()
	w.wires = append(w.wires, buf.String())
	w.mu.Unlock()
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}, nil
}

// TestNoOtherCredentialHeaderLeavesWithTheToken: header names are case-insensitive on the wire, so a caller's
// Authorization or Proxy-Authorization under any spelling of the key is removed; exactly one Authorization line leaves,
// the workload token's.
func TestNoOtherCredentialHeaderLeavesWithTheToken(t *testing.T) {
	for name, header := range map[string]http.Header{
		"lower":               {"authorization": {"Bearer fallback-secret"}},
		"upper":               {"AUTHORIZATION": {"Bearer fallback-secret"}},
		"mixed":               {"AuThOrIzAtIoN": {"Bearer fallback-secret"}},
		"canonical and lower": {"Authorization": {"Bearer fallback-secret"}, "authorization": {"Bearer fallback-secret-2"}},
		"two values":          {"Authorization": {"Bearer fallback-secret", "Basic fallback-secret"}},
		"proxy canonical":     {"Proxy-Authorization": {"Basic fallback-secret"}},
		"proxy lower":         {"proxy-authorization": {"Basic fallback-secret"}},
		"other headers stay":  {"X-Request-Id": {"r-1"}, "accept": {"application/json"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
			wire := &wireHeaders{}
			rt, err := c.Transport(wire, testTarget, testAudience)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
			req.Header = header.Clone()
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			sent := wire.wires[0]
			if strings.Contains(sent, "fallback-secret") {
				t.Fatalf("a caller credential left with the token:\n%s", sent)
			}
			var auth []string
			for _, line := range strings.Split(strings.TrimSpace(sent), "\r\n") {
				key, value, _ := strings.Cut(line, ":")
				if strings.EqualFold(key, "Authorization") {
					auth = append(auth, strings.TrimSpace(value))
				}
				if strings.EqualFold(key, "Proxy-Authorization") {
					t.Fatalf("a Proxy-Authorization line left: %s", line)
				}
			}
			if len(auth) != 1 || auth[0] != "Bearer "+tokenN(0) {
				t.Fatalf("Authorization lines %q, want exactly the workload token:\n%s", auth, sent)
			}
			if name == "other headers stay" && (!strings.Contains(sent, "X-Request-Id: r-1") || !strings.Contains(sent, "Accept: application/json")) {
				t.Fatalf("other headers were dropped:\n%s", sent)
			}
			for key, values := range header {
				if got := req.Header[key]; len(got) != len(values) {
					t.Fatalf("the caller's header %q was changed", key)
				}
			}
		})
	}
}
