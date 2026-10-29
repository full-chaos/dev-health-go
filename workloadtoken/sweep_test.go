package workloadtoken

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestTheTargetCheckCoversEveryDestinationField: the request's authority (Host), an opaque request target, user info
// and an empty URL host each change or blur where the request goes; each is refused before a token is fetched or sent.
func TestTheTargetCheckCoversEveryDestinationField(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeSubject(t, testSubject))
	api := &hostResource{}
	rt, err := c.Transport(api, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]func(*http.Request){
		"host override":           func(r *http.Request) { r.Host = "attacker.test" },
		"host override with port": func(r *http.Request) { r.Host = "api.test:8443" },
		"opaque target":           func(r *http.Request) { r.URL.Opaque = "//attacker.test/x" },
		"user info":               func(r *http.Request) { r.URL.User = url.UserPassword("u", "p") },
		"empty URL host":          func(r *http.Request) { r.URL.Host = "" },
	}
	for name, mutate := range refused {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
			mutate(req)
			resp, err := rt.RoundTrip(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("the request was sent")
			}
			if !errors.Is(err, ErrRequestRefused) {
				t.Fatalf("err %v, want ErrRequestRefused", err)
			}
		})
	}
	if got := api.seen("api.test"); len(got) != 0 {
		t.Fatalf("a refused request reached the resource: %v", got)
	}
	if exchange.count() != 0 {
		t.Fatal("a refused request fetched a token")
	}
	for _, host := range []string{"", "api.test", "API.TEST", "api.test:443"} {
		req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
		req.Host = host
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("Host %q: %v", host, err)
		}
		_ = resp.Body.Close()
	}
}

// echoTransport fails with an error that quotes the request's Authorization header, as a caller's transport may.
type echoTransport struct{ cause error }

func (e echoTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("gateway rejected Authorization=%s: %w", req.Header.Get("Authorization"), e.cause)
}

func TestAResourceTransportErrorNeverCarriesTheToken(t *testing.T) {
	for name, cause := range map[string]error{
		"plain":    errors.New("connection reset"),
		"canceled": context.Canceled,
		"deadline": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
			rt, err := c.Transport(echoTransport{cause: cause}, testTarget, testAudience)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
			_, err = rt.RoundTrip(req)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), tokenN(0)) || strings.Contains(fmt.Sprintf("%+v", err), tokenN(0)) {
				t.Fatalf("the error carries the token: %v", err)
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), tokenN(0)) {
					t.Fatalf("an error in the chain carries the token: %v", e)
				}
			}
			if !errors.Is(err, ErrResourceTransport) {
				t.Fatalf("err %v, want ErrResourceTransport", err)
			}
			if cause == context.Canceled || cause == context.DeadlineExceeded {
				if !errors.Is(err, cause) {
					t.Fatalf("err %v lost %v", err, cause)
				}
			}
			// Through an http.Client the error is wrapped once more; the token stays out of it.
			client := &http.Client{Transport: rt}
			if _, err := client.Get(testTarget + "/v1/thing"); err == nil || strings.Contains(err.Error(), tokenN(0)) {
				t.Fatalf("through http.Client: %v", err)
			}
		})
	}
}

func TestTheTransportPinsItsScope(t *testing.T) {
	exchange := &fakeExchange{answer: issueEach(300)}
	c := newTestClient(t, exchange, writeSubject(t, testSubject))
	scope := []string{"read"}
	rt, err := c.Transport(&hostResource{}, testTarget, testAudience, scope...)
	if err != nil {
		t.Fatal(err)
	}
	scope[0] = "admin"
	req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := exchange.call(t, 0).form.Get("scope"); got != "read" {
		t.Fatalf("the exchange asked for scope %q, want the scope pinned at construction %q", got, "read")
	}
}

// headerEcho answers with Request set to the request it received, as net/http transports do.
type headerEcho struct{}

func (headerEcho) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
}

func TestTheResponseDoesNotCarryTheAuthorizedRequest(t *testing.T) {
	c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
	rt, err := c.Transport(headerEcho{}, testTarget, testAudience)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Request != req || resp.Request.Header.Get("Authorization") != "" {
		t.Fatalf("resp.Request carries the authorized copy (Authorization %q)", resp.Request.Header.Get("Authorization"))
	}
}
