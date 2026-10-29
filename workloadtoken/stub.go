package workloadtoken

import (
	"context"
	"net/http"
)

// Client is not built yet.
type Client struct{ endpoint string }

// New is not built yet.
func New(cfg Config) (*Client, error) { return nil, errNotBuilt }

// Token is not built yet.
func (c *Client) Token(ctx context.Context, audience string, scope ...string) (string, error) {
	return "", errNotBuilt
}

// Invalidate is not built yet.
func (c *Client) Invalidate(audience string, scope ...string) {}

// RoundTripper is not built yet.
func (c *Client) RoundTripper(next http.RoundTripper, audience string, scope ...string) (http.RoundTripper, error) {
	return nil, errNotBuilt
}
