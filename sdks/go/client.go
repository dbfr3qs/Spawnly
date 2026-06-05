package spawnly

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// tenantHeaderKey is the canonical header carrying the tenant scope.
const tenantHeaderKey = "X-Tenant-ID"

// TenantHeader returns the X-Tenant-ID header for a tenanted agent, or an empty
// http.Header for a global (tenant-less) agent. It centralizes the "assert a
// tenant only when we have one" rule so callers never hard-code the header.
func TenantHeader(tenantID string) http.Header {
	h := http.Header{}
	if tenantID != "" {
		h.Set(tenantHeaderKey, tenantID)
	}
	return h
}

// AuthenticatedClient makes HTTP requests that are automatically authenticated
// against the platform. For every request it:
//
//   - fetches (and caches via the TokenClient) a bearer token for its scope,
//   - sets Authorization: Bearer <token>,
//   - sets X-Tenant-ID when a tenant ID is present (see TenantHeader), and
//   - resolves relative paths against its base URL (absolute URLs — those
//     starting with "http" — pass through unchanged).
//
// We expose an explicit Do(ctx, method, path, body) plus thin Get/Post helpers
// rather than an http.RoundTripper. A RoundTripper cannot take a context.Context
// beyond the one already on the *http.Request, and the token fetch is a
// blocking, retrying, context-aware operation; threading the context explicitly
// keeps cancellation honest and the API obvious.
type AuthenticatedClient struct {
	baseURL     string
	scope       string
	tokenClient *TokenClient
	tenantID    string
	httpClient  *http.Client
}

// AuthenticatedClientOption configures an AuthenticatedClient.
type AuthenticatedClientOption func(*AuthenticatedClient)

// WithTenantID sets the tenant ID whose X-Tenant-ID header is attached to every
// request. Omit it (or pass "") for a global, tenant-less agent.
func WithTenantID(tenantID string) AuthenticatedClientOption {
	return func(c *AuthenticatedClient) { c.tenantID = tenantID }
}

// WithClientHTTPClient overrides the http.Client used to send requests
// (distinct from the TokenClient's). Intended mainly for tests.
func WithClientHTTPClient(h *http.Client) AuthenticatedClientOption {
	return func(c *AuthenticatedClient) { c.httpClient = h }
}

// NewAuthenticatedClient constructs an AuthenticatedClient targeting baseURL,
// authenticating with a token for scope obtained from tokenClient.
func NewAuthenticatedClient(baseURL, scope string, tokenClient *TokenClient, opts ...AuthenticatedClientOption) *AuthenticatedClient {
	c := &AuthenticatedClient{
		baseURL:     baseURL,
		scope:       scope,
		tokenClient: tokenClient,
		httpClient:  &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Do builds, authenticates, and sends a request. path may be relative (resolved
// against the base URL) or an absolute http(s) URL (passed through). The caller
// owns closing the returned response body.
func (c *AuthenticatedClient) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.tokenClient.GetToken(ctx, c.scope)
	if err != nil {
		return nil, fmt.Errorf("authenticated request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.resolveURL(path), body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	if c.tenantID != "" {
		req.Header.Set(tenantHeaderKey, c.tenantID)
	}

	return c.httpClient.Do(req)
}

// Get is a thin helper for Do with method GET.
func (c *AuthenticatedClient) Get(ctx context.Context, path string) (*http.Response, error) {
	return c.Do(ctx, http.MethodGet, path, nil)
}

// Post is a thin helper for Do with method POST.
func (c *AuthenticatedClient) Post(ctx context.Context, path string, body io.Reader) (*http.Response, error) {
	return c.Do(ctx, http.MethodPost, path, body)
}

// resolveURL resolves path against the base URL. An absolute http(s) URL passes
// through unchanged; anything else is treated as a relative path.
func (c *AuthenticatedClient) resolveURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return c.baseURL + path
}
