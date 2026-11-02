package pep

import (
	"crypto/rand"
	"encoding/json"
	"regexp"
)

// MaxBatch is the contract's bound on a batch: 1 to 100 requests.
const MaxBatch = 100

const (
	principalReferencePattern = `^[A-Za-z0-9._~-]{16,1000}$`
	repositoryKeyPattern      = `^([a-z0-9][a-z0-9._-]{0,99}/[a-z0-9][a-z0-9._-]{0,99}|\*)$`
	otherKeyPattern           = `^[a-z0-9][a-z0-9._:-]{0,255}$`
	requestIDAlphabet         = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
)

var (
	principalReferenceRE = regexp.MustCompile(principalReferencePattern)
	actionPattern        = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	requestIDPattern     = regexp.MustCompile(`^req_[A-Za-z0-9_-]{26}$`)
	repositoryKeyRE      = regexp.MustCompile(repositoryKeyPattern)
	otherKeyRE           = regexp.MustCompile(otherKeyPattern)

	resourceTypes  = []string{"organization", "repository", "membership", "service_account", "grant", "credential", "session", "signing_key", "subscription", "platform"}
	clientServices = []string{"dev-health-ops-api", "dev-health-ops-billing-edge", "dev-health-web", "dev-health-acr-api", "dev-health-acr-mcp", "dev-health-query-api", "dev-health-auth-service"}
)

// Request is one question put to the PDP: may this principal perform this
// action on this resource. It carries no authority: the principal is an opaque
// reference the PDP resolves, and the PDP verifies OrganizationID.
type Request struct {
	// PrincipalReference is the opaque reference the PDP resolves to a principal.
	PrincipalReference string
	// Action is a registered dotted action such as acr.context.read.
	Action string
	// ResourceType is one of the contract's closed resource kinds.
	ResourceType string
	// ResourceKey is the canonical lowercase key: owner/repo or "*" for a
	// repository, a lowercase token for every other type.
	ResourceKey string
	// OrganizationID is the resource's organization; empty for a platform
	// resource.
	OrganizationID string
	// RequestID correlates the call in the PDP's audit row. Empty makes the
	// client mint one. It is never echoed in the response.
	RequestID string
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (r Request) valid() bool {
	if !principalReferenceRE.MatchString(r.PrincipalReference) || !actionPattern.MatchString(r.Action) || !contains(resourceTypes, r.ResourceType) {
		return false
	}
	if r.ResourceType == "repository" {
		if !repositoryKeyRE.MatchString(r.ResourceKey) {
			return false
		}
	} else if !otherKeyRE.MatchString(r.ResourceKey) {
		return false
	}
	if len(r.OrganizationID) > 256 || (r.OrganizationID != "" && !printable(r.OrganizationID)) {
		return false
	}
	return r.RequestID == "" || requestIDPattern.MatchString(r.RequestID)
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func mintRequestID() string {
	raw := make([]byte, 26)
	// crypto/rand.Read does not return an error (Go 1.24+): it crashes the
	// program instead of handing back predictable bytes.
	_, _ = rand.Read(raw)
	out := make([]byte, 0, 30)
	out = append(out, "req_"...)
	for _, b := range raw {
		out = append(out, requestIDAlphabet[b&63])
	}
	return string(out)
}

type wireResource struct {
	Type           string `json:"type"`
	OrganizationID string `json:"organization_id,omitempty"`
	Key            string `json:"key"`
}

type wireContext struct {
	RequestID     string `json:"request_id"`
	ClientService string `json:"client_service"`
}

type wireRequest struct {
	SchemaVersion      string       `json:"schema_version"`
	PrincipalReference string       `json:"principal_reference"`
	Action             string       `json:"action"`
	Resource           wireResource `json:"resource"`
	Context            wireContext  `json:"context"`
}

type wireBatchRequest struct {
	SchemaVersion string        `json:"schema_version"`
	Requests      []wireRequest `json:"requests"`
}

func (c *Client) wire(r Request) wireRequest {
	id := r.RequestID
	if id == "" {
		id = mintRequestID()
	}
	return wireRequest{
		SchemaVersion:      "authorization_request.v1",
		PrincipalReference: r.PrincipalReference,
		Action:             r.Action,
		Resource:           wireResource{Type: r.ResourceType, OrganizationID: r.OrganizationID, Key: r.ResourceKey},
		Context:            wireContext{RequestID: id, ClientService: c.service},
	}
}

func (c *Client) singleBody(r Request) ([]byte, error) { return json.Marshal(c.wire(r)) }

func (c *Client) batchBody(rs []Request) ([]byte, error) {
	doc := wireBatchRequest{SchemaVersion: "authorization_batch_request.v1", Requests: make([]wireRequest, len(rs))}
	for i, r := range rs {
		doc.Requests[i] = c.wire(r)
	}
	return json.Marshal(doc)
}
