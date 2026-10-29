package workloadtoken

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// RoundTripper returns a transport that sends requests for target (the
// resource service's base URL: scheme, host and port) through next with
// Authorization: Bearer <workload token for audience and scope>.
//
// It sends only what the caller built for target: a request for any other
// origin, a redirect hop an http.Client follows (the token would be attached
// again to a destination the caller never chose) or a request without a URL
// is refused with ErrRequestRefused and next is never called. When no usable
// token can be had it returns that error and next is never called either: no
// request leaves without the header, with an old token or with any other
// credential. next follows the same explicit-transport rule as
// Config.Transport; an http target needs the client's AllowPlainHTTP.
func (c *Client) RoundTripper(next http.RoundTripper, target, audience string, scope ...string) (http.RoundTripper, error) {
	if !c.ready() {
		return nil, errNotConfigured
	}
	if err := CheckTransport(next); err != nil {
		return nil, err
	}
	base, err := baseURL(target, c.allowPlainHTTP)
	if err != nil {
		return nil, err
	}
	if _, _, err := requestKey(audience, scope); err != nil {
		return nil, err
	}
	return &bearerTransport{client: c, next: next, origin: origin(base), audience: audience, scope: slices.Clone(scope)}, nil
}

type bearerTransport struct {
	client   *Client
	next     http.RoundTripper
	origin   string
	audience string
	scope    []string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, ErrRequestRefused
	}
	if !t.forTarget(req) {
		closeBody(req)
		return nil, ErrRequestRefused
	}
	token, err := t.client.Token(req.Context(), t.audience, t.scope...)
	if err != nil {
		closeBody(req)
		return nil, err
	}
	authorized := req.Clone(req.Context())
	if authorized.Header == nil {
		authorized.Header = http.Header{}
	}
	// Header names are case-insensitive on the wire: a caller credential
	// under any spelling of the key would leave next to the token.
	for key := range authorized.Header {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Proxy-Authorization") {
			delete(authorized.Header, key)
		}
	}
	authorized.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.next.RoundTrip(authorized)
	if err != nil {
		return nil, resourceError(err)
	}
	if resp == nil {
		return nil, ErrResourceTransport
	}
	// The response hands back the caller's request, never the copy that
	// carried the token or any request the transport behind built from it.
	resp.Request = req
	return resp, nil
}

// forTarget reports whether req goes to the target and only there: its URL
// origin (an empty host never matches) and its Host (the authority sent) are
// the target's, and it is not a redirect hop, an opaque request target or a
// URL with user info.
func (t *bearerTransport) forTarget(req *http.Request) bool {
	u := req.URL
	if u == nil || req.Response != nil || u.Opaque != "" || u.User != nil || origin(u) != t.origin {
		return false
	}
	return req.Host == "" || origin(&url.URL{Scheme: u.Scheme, Host: req.Host}) == t.origin
}

// errResourceTimeout keeps a transport timeout visible as a net.Error-style
// Timeout without the failure's text.
type errResourceTimeout struct{}

func (errResourceTimeout) Error() string { return "timeout" }
func (errResourceTimeout) Timeout() bool { return true }

// resourceError drops the text of a failure behind the token transport (a
// transport may quote the request's Authorization header); only context
// cancellation, a deadline or a timeout stays visible.
func resourceError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", ErrResourceTransport, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrResourceTransport, context.DeadlineExceeded)
	case isTimeout(err):
		return fmt.Errorf("%w: %w", ErrResourceTransport, errResourceTimeout{})
	default:
		return ErrResourceTransport
	}
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
