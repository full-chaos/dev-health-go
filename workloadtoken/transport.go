package workloadtoken

import (
	"net/http"
)

// RoundTripper returns a transport that sends every request through next with
// Authorization: Bearer <workload token for audience and scope>. When no
// usable token can be had it returns the error and next is never called: no
// request leaves without the header, with an old token or with any other
// credential. next follows the same explicit-transport rule as Config.Transport.
func (c *Client) RoundTripper(next http.RoundTripper, target, audience string, scope ...string) (http.RoundTripper, error) {
	if err := CheckTransport(next); err != nil {
		return nil, err
	}
	if _, _, err := requestKey(audience, scope); err != nil {
		return nil, err
	}
	return &bearerTransport{client: c, next: next, audience: audience, scope: scope}, nil
}

type bearerTransport struct {
	client   *Client
	next     http.RoundTripper
	audience string
	scope    []string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.client.Token(req.Context(), t.audience, t.scope...)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	authorized := req.Clone(req.Context())
	authorized.Header.Set("Authorization", "Bearer "+token)
	return t.next.RoundTrip(authorized)
}
