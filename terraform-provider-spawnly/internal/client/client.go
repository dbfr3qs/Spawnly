// Package client is a thin HTTP client for the Spawnly registry's control-plane
// API. It depends on the platform only through this wire contract — it never
// imports platform code — so the dependency points one way, from this adapter
// toward the registry's neutral HTTP surface.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the registry control-plane endpoints. Token is the
// shared-secret bearer; an empty Token sends no Authorization header, which is
// correct against a registry running open (CONTROL_PLANE_AUTH=none).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a Client with a sane default timeout.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Wire types mirror the registry's AgentTemplate JSON. They are intentionally a
// copy (not an import of the platform package): the provider is a separate
// module and a standalone client of the HTTP contract.

type Template struct {
	AgentType      string           `json:"agentType"`
	Version        string           `json:"version"`
	Status         string           `json:"status,omitempty"`
	Meta           TemplateMeta     `json:"meta"`
	Runtime        RuntimeSpec      `json:"runtimeSpec"`
	AuthZ          AuthZSpec        `json:"authzTemplate"`
	Delegation     DelegationPolicy `json:"delegation"`
	RequiresTenant bool             `json:"requiresTenant"`
	OAuthScopes    []string         `json:"oauthScopes,omitempty"`
}

type TemplateMeta struct {
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type RuntimeSpec struct {
	Image        string            `json:"image"`
	Resources    ResourceLimits    `json:"resources"`
	EnvDefaults  map[string]string `json:"envDefaults,omitempty"`
	Lifecycle    string            `json:"lifecycle"`
	SupportsChat bool              `json:"supportsChat"`
}

type ResourceLimits struct {
	CPULimit    string `json:"cpuLimits"`
	MemoryLimit string `json:"memoryLimits"`
}

type AuthZSpec struct {
	SpiceDBRelations []SpiceDBRelation `json:"spiceDbRelations"`
}

type SpiceDBRelation struct {
	Resource string `json:"resource"`
	Relation string `json:"relation"`
	Subject  string `json:"subject"`
}

type DelegationPolicy struct {
	AllowedChildTypes []string                    `json:"allowedChildTypes"`
	GrantableScopes   []string                    `json:"grantableScopes"`
	MaxDepth          int                         `json:"maxDepth"`
	ChildPolicies     map[string]ChildSpawnPolicy `json:"childPolicies,omitempty"`
}

type ChildSpawnPolicy struct {
	RequireUserConsent bool   `json:"requireUserConsent"`
	ConsentTTL         string `json:"consentTTL,omitempty"`
}

// Schema is the registry's active SpiceDB schema (GET /v1/schema).
type Schema struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	Source  string `json:"source"`
}

// APIError carries a non-success HTTP status and the response body so callers
// (and Terraform diagnostics) can surface the registry's own message.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("registry returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// GetTemplate fetches one template. found is false (with a nil error) on 404.
func (c *Client) GetTemplate(ctx context.Context, agentType string) (tmpl *Template, found bool, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/templates/"+agentType, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, readAPIError(resp)
	}
	var t Template
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

// ListTemplateTypes returns the spawnable (active) template type names. Disabled
// templates are excluded by the registry, so this is a catalog view, not a full
// inventory.
func (c *Client) ListTemplateTypes(ctx context.Context) ([]string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/templates", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var types []string
	if err := json.NewDecoder(resp.Body).Decode(&types); err != nil {
		return nil, err
	}
	return types, nil
}

// PutTemplate registers (or, since POST is an upsert, overwrites) a template.
func (c *Client) PutTemplate(ctx context.Context, t Template) error {
	buf, err := json.Marshal(t)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/templates", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return readAPIError(resp)
	}
	return nil
}

// SetStatus flips a template's status (active|disabled) via PATCH.
func (c *Client) SetStatus(ctx context.Context, agentType, status string) error {
	buf, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPatch, "/v1/templates/"+agentType, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}
	return nil
}

// DeleteTemplate removes a template. The registry 409s unless the template is
// already disabled; callers that want a one-shot teardown should disable first.
func (c *Client) DeleteTemplate(ctx context.Context, agentType string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/templates/"+agentType, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return readAPIError(resp)
	}
	return nil
}

// GetSchema returns the registry's active SpiceDB schema.
func (c *Client) GetSchema(ctx context.Context) (*Schema, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/schema", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var s Schema
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &APIError{Status: resp.StatusCode, Body: string(body)}
}
