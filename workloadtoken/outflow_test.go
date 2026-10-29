package workloadtoken

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

// cloneEcho answers with Request set to a copy of the request it received (headers included), as a wrapping transport may.
type cloneEcho struct{}

func (cloneEcho) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: req.Clone(req.Context())}, nil
}

// TestNoResponseCarriesTheTokenInItsRequest: whatever request the transport behind it puts on the response (the copy
// that carried the token, a clone of it, or none), the caller gets back its own request.
func TestNoResponseCarriesTheTokenInItsRequest(t *testing.T) {
	for name, next := range map[string]http.RoundTripper{"same pointer": headerEcho{}, "clone": cloneEcho{}, "no request": &hostResource{}} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, writeSubject(t, testSubject))
			rt, err := c.RoundTripper(next, testTarget, testAudience)
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
				t.Fatalf("resp.Request is not the caller's request (Authorization %q)", resp.Request.Header.Get("Authorization"))
			}
			client := &http.Client{Transport: rt}
			resp, err = client.Get(testTarget + "/v1/thing")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if got := resp.Request.Header.Get("Authorization"); got != "" {
				t.Fatalf("through http.Client resp.Request carries %q", got)
			}
		})
	}
}

// TestTypedNilTransportsAreRefusedWithoutAPanic: an interface holding a nil pointer is not nil; every entry point that
// takes a transport refuses it before using it.
func TestTypedNilTransportsAreRefusedWithoutAPanic(t *testing.T) {
	path := writeSubject(t, testSubject)
	for name, rt := range map[string]http.RoundTripper{
		"nil *http.Transport": (*http.Transport)(nil),
		"nil custom pointer":  (*fakeExchange)(nil),
		"nil func":            roundTripFunc(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckTransport(rt); err == nil {
				t.Fatal("CheckTransport accepted it")
			}
			if _, err := New(Config{ExchangeURL: testBase, Subject: KubernetesTokenFile(path), Transport: rt}); err == nil {
				t.Fatal("New accepted it")
			}
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path)
			if _, err := c.RoundTripper(rt, testTarget, testAudience); err == nil {
				t.Fatal("RoundTripper accepted it")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestNilAndZeroValuesNeverPanic covers every exported method and argument that can be nil or a zero value.
func TestNilAndZeroValuesNeverPanic(t *testing.T) {
	path := writeSubject(t, testSubject)
	var nilClient *Client
	zeroClient := &Client{}
	var nilCtx context.Context
	checks := map[string]func() error{
		"nil client Token":         func() error { _, err := nilClient.Token(context.Background(), testAudience); return err },
		"nil client RoundTripper":  func() error { _, err := nilClient.RoundTripper(&hostResource{}, testTarget, testAudience); return err },
		"zero client Token":        func() error { _, err := zeroClient.Token(context.Background(), testAudience); return err },
		"zero client RoundTripper": func() error { _, err := zeroClient.RoundTripper(&hostResource{}, testTarget, testAudience); return err },
		"nil context": func() error {
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path)
			_, err := c.Token(nilCtx, testAudience)
			return err
		},
		"nil request": func() error {
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path)
			rt, err := c.RoundTripper(&hostResource{}, testTarget, testAudience)
			if err != nil {
				return nil
			}
			_, err = rt.RoundTrip(nil)
			return err
		},
		"next answers nil, nil": func() error {
			c := newTestClient(t, &fakeExchange{answer: issueEach(300)}, path)
			rt, err := c.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil }), testTarget, testAudience)
			if err != nil {
				return nil
			}
			req, _ := http.NewRequest(http.MethodGet, testTarget+"/v1/thing", nil)
			resp, err := rt.RoundTrip(req)
			if resp != nil {
				return nil
			}
			return err
		},
		"exchange answers nil, nil": func() error {
			c := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil }), path)
			_, err := c.Token(context.Background(), testAudience)
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panic: %v", r)
						t.Errorf("panicked: %v", r)
					}
				}()
				err = check()
			}()
			if err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
	nilClient.Invalidate(testAudience)
	zeroClient.Invalidate(testAudience)
	t.Run("exchange answers a nil body", func(t *testing.T) {
		c := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}}, nil
		}), path)
		if _, err := c.Token(context.Background(), testAudience); !errors.Is(err, ErrExchangeUnavailable) {
			t.Fatalf("err %v", err)
		}
	})
}

// TestTheProjectedVolumeRotationIsFollowed: the kubelet writes a new timestamped directory and swaps the ..data symlink
// to it; the token file is a symlink through ..data. The next exchange sends the new token.
func TestTheProjectedVolumeRotationIsFollowed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		volume := t.TempDir()
		write := func(stamp, value string) {
			dir := filepath.Join(volume, stamp)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "token"), []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, "token"), 0o644); err != nil {
				t.Fatal(err)
			}
			tmp := filepath.Join(volume, "..data_tmp")
			if err := os.Symlink(stamp, tmp); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, filepath.Join(volume, "..data")); err != nil {
				t.Fatal(err)
			}
		}
		write("..2026_10_29_04_00_00.000000001", testSubject)
		if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(volume, "token")); err != nil {
			t.Fatal(err)
		}
		exchange := &fakeExchange{answer: issueEach(300)}
		c := newTestClient(t, exchange, filepath.Join(volume, "token"))
		start := time.Now()
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatal(err)
		}
		rotated := "eyJhbGciOiJSUzI1NiJ9.c3ViamVjdC12Mw.c2lnbmF0dXJl"
		write("..2026_10_29_04_48_00.000000002", rotated)
		if err := os.RemoveAll(filepath.Join(volume, "..2026_10_29_04_00_00.000000001")); err != nil {
			t.Fatal(err)
		}
		sleepUntil(start, 150*time.Second)
		if _, err := c.Token(context.Background(), testAudience); err != nil {
			t.Fatalf("after the ..data swap: %v", err)
		}
		if got := exchange.call(t, 1).form.Get("subject_token"); got != rotated {
			t.Fatal("the exchange after the ..data swap sent the old token")
		}
	})
}
