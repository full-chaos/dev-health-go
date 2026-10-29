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

// sentMethods are the methods the token transport sends; any other is refused.
var sentMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
}

// forwardedHeaders are the only caller headers the token transport sends on
// (matched in any key spelling, written under the canonical key). Every other
// caller header is dropped: credentials (Authorization, Cookie, Proxy-*),
// forwarding headers, Content-Length (net/http derives it) and the rest.
var forwardedHeaders = []string{"Accept", "Content-Type", "User-Agent", "X-Request-Id", "Traceparent", "Tracestate"}

// Transport returns a transport that sends requests to target (the resource
// service's base URL: scheme, host and port) with Authorization: Bearer
// <workload token for audience and scope>.
//
// It never sends the caller's request: it reads and checks it, then builds a
// new one from the fixed target, the caller's method, path, query, body and
// context, the forwardedHeaders and exactly one Authorization. A request it
// will not send (no URL, a redirect hop, a URL or Host naming another origin,
// an opaque target, user info, a path not starting with "/", a method outside
// GET, HEAD, POST, PUT, PATCH, DELETE) is refused with ErrRequestRefused, as is
// a request for which no usable token can be had; next is then never called.
// A failure of next is ErrResourceTransport, without its text. The response
// always carries the caller's request. next follows the same explicit-transport
// rule as Config.Transport; an http target needs the client's AllowPlainHTTP.
func (c *Client) Transport(next http.RoundTripper, target, audience string, scope ...string) (http.RoundTripper, error) {
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
	return &bearerTransport{
		client: c, next: next, scheme: base.Scheme, host: base.Host, origin: origin(base),
		audience: audience, scope: slices.Clone(scope),
	}, nil
}

// HTTPClient is an http.Client over Transport that refuses every redirect.
func (c *Client) HTTPClient(next http.RoundTripper, target, audience string, scope ...string) (*http.Client, error) {
	rt, err := c.Transport(next, target, audience, scope...)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRequestRefused },
	}, nil
}

type bearerTransport struct {
	client   *Client
	next     http.RoundTripper
	scheme   string
	host     string
	origin   string
	audience string
	scope    []string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, ErrRequestRefused
	}
	out, ok := t.build(req)
	if !ok {
		closeBody(req)
		return nil, ErrRequestRefused
	}
	token, err := t.client.Token(req.Context(), t.audience, t.scope...)
	if err != nil {
		closeBody(req)
		return nil, fmt.Errorf("%w: %w", ErrRequestRefused, err)
	}
	out.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, resourceError(err)
	}
	if resp == nil {
		return nil, ErrResourceTransport
	}
	resp.Request = req
	return resp, nil
}

// build checks req and returns the request to send: new, for the target.
func (t *bearerTransport) build(req *http.Request) (*http.Request, bool) {
	u := req.URL
	if u == nil || req.Response != nil || u.Opaque != "" || u.User != nil {
		return nil, false
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if !sentMethods[method] {
		return nil, false
	}
	if (u.Scheme != "" || u.Host != "") && origin(u) != t.origin {
		return nil, false
	}
	if req.Host != "" && origin(&url.URL{Scheme: t.scheme, Host: req.Host}) != t.origin {
		return nil, false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	target, err := url.Parse(t.scheme + "://" + t.host + path)
	if err != nil {
		return nil, false
	}
	target.RawQuery = u.RawQuery
	header := http.Header{}
	for key, values := range req.Header {
		for _, name := range forwardedHeaders {
			if strings.EqualFold(key, name) {
				header[name] = append(header[name], values...)
			}
		}
	}
	out := (&http.Request{
		Method: method, URL: target, Host: t.host, Header: header,
		Body: req.Body, GetBody: req.GetBody, ContentLength: req.ContentLength, Close: req.Close,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}).WithContext(req.Context())
	return out, true
}

// errResourceTimeout keeps a transport timeout visible as a net.Error-style
// Timeout without the failure's text.
type errResourceTimeout struct{}

func (errResourceTimeout) Error() string { return "timeout" }
func (errResourceTimeout) Timeout() bool { return true }

// resourceError drops the text of a failure behind the token transport (a
// transport may quote the request's headers); only context cancellation, a
// deadline or a timeout stays visible.
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
