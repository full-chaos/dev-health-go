package workloadtoken

import (
	"net/http"
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
	return &bearerTransport{client: c, next: next, origin: origin(base), audience: audience, scope: scope}, nil
}

type bearerTransport struct {
	client   *Client
	next     http.RoundTripper
	origin   string
	audience string
	scope    []string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.Response != nil || origin(req.URL) != t.origin {
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
	authorized.Header.Set("Authorization", "Bearer "+token)
	return t.next.RoundTrip(authorized)
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
