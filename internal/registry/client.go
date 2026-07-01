// internal/registry/client.go
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spawnly/platform/internal/events"
)

type Client interface {
	GetTemplate(ctx context.Context, agentType string) (AgentTemplate, error)
	Complete(ctx context.Context, agentID string) error
	Fail(ctx context.Context, agentID string) error
	ListAgents(ctx context.Context) ([]AgentRecord, error)
	GetAgent(ctx context.Context, id string) (AgentRecord, error)
	ListEvents(ctx context.Context, agentID string) ([]events.Event, error)
	PostEvent(ctx context.Context, agentID string, e events.Event) error
	ListTemplates(ctx context.Context) ([]string, error)
	DismissAgent(ctx context.Context, agentID string) error
	PreRegisterAgent(ctx context.Context, r AgentRecord) error
	CheckSpawnPolicy(ctx context.Context, parentID, childType string) (SpawnDecision, error)
	Subtree(ctx context.Context, id, userId string) ([]string, error)
}

type HTTPClient struct {
	base string
	http *http.Client
}

func New(baseURL string) *HTTPClient {
	return NewWithToken(baseURL, "")
}

// NewWithToken is New plus a STATIC control-plane bearer token. When token != "",
// every outbound request carries "Authorization: Bearer <token>". When token == "",
// it behaves like New (no Authorization header, default transport). For a bearer
// that changes over time (e.g. a refreshing oidc client-credentials token) use
// NewWithTokenSource instead.
func NewWithToken(baseURL, token string) *HTTPClient {
	if token == "" {
		return NewWithTokenSource(baseURL, nil)
	}
	return NewWithTokenSource(baseURL, func() string { return token })
}

// NewWithTokenSource is New plus a DYNAMIC control-plane bearer: token is called
// per request, so a refreshing source (oidc client-credentials) is picked up on
// every call. A nil token, or one that returns "", sends no Authorization header
// (the local "none" demo tier, where the registry enforces nothing) — and, for a
// refreshing source, fails closed (no header → the registry 401s) rather than
// sending a stale/empty bearer.
//
// The client carries a 30s Timeout: every call is a short JSON round-trip
// (registry GET/POST/PATCH); none stream, so a client-wide timeout is safe and
// prevents a hung registry socket from blocking a caller forever.
func NewWithTokenSource(baseURL string, token func() string) *HTTPClient {
	hc := &http.Client{Timeout: 30 * time.Second}
	if token != nil {
		hc.Transport = &bearerTransport{token: token, base: http.DefaultTransport}
	}
	return &HTTPClient{base: baseURL, http: hc}
}

// bearerTransport sets an Authorization header on every request from its token
// source before delegating to the wrapped RoundTripper. It clones the request so
// it never mutates a caller-owned *http.Request (RoundTripper contract). An empty
// token yields no header (fail-closed: the registry rejects rather than the
// client sending a blank bearer).
type bearerTransport struct {
	token func() string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := t.token()
	if tok == "" {
		return t.base.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(r)
}

func (c *HTTPClient) GetTemplate(ctx context.Context, agentType string) (AgentTemplate, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/templates/"+agentType, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return AgentTemplate{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return AgentTemplate{}, fmt.Errorf("template %q not found", agentType)
	}
	if resp.StatusCode != http.StatusOK {
		return AgentTemplate{}, fmt.Errorf("get template: status %d", resp.StatusCode)
	}
	var t AgentTemplate
	return t, json.NewDecoder(resp.Body).Decode(&t)
}

func (c *HTTPClient) patchStatus(ctx context.Context, agentID, status string) error {
	body, _ := json.Marshal(map[string]string{"status": status})
	req, _ := http.NewRequestWithContext(ctx, "PATCH", c.base+"/v1/agents/"+agentID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (c *HTTPClient) Complete(ctx context.Context, agentID string) error {
	return c.patchStatus(ctx, agentID, "completed")
}

func (c *HTTPClient) Fail(ctx context.Context, agentID string) error {
	return c.patchStatus(ctx, agentID, "failed")
}

func (c *HTTPClient) ListAgents(ctx context.Context) ([]AgentRecord, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/agents", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list agents: status %d", resp.StatusCode)
	}
	var agents []AgentRecord
	return agents, json.NewDecoder(resp.Body).Decode(&agents)
}

func (c *HTTPClient) GetAgent(ctx context.Context, id string) (AgentRecord, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/agents/"+id, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return AgentRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return AgentRecord{}, fmt.Errorf("agent %q not found", id)
	}
	if resp.StatusCode != http.StatusOK {
		return AgentRecord{}, fmt.Errorf("get agent: status %d", resp.StatusCode)
	}
	var a AgentRecord
	return a, json.NewDecoder(resp.Body).Decode(&a)
}

func (c *HTTPClient) ListEvents(ctx context.Context, agentID string) ([]events.Event, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/agents/"+agentID+"/events", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list events: status %d", resp.StatusCode)
	}
	var evts []events.Event
	return evts, json.NewDecoder(resp.Body).Decode(&evts)
}

func (c *HTTPClient) PostEvent(ctx context.Context, agentID string, e events.Event) error {
	body, _ := json.Marshal(e)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.base+"/v1/agents/"+agentID+"/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("post event: status %d", resp.StatusCode)
	}
	return nil
}

func (c *HTTPClient) ListTemplates(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/templates", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list templates: status %d", resp.StatusCode)
	}
	var types []string
	return types, json.NewDecoder(resp.Body).Decode(&types)
}

func (c *HTTPClient) PreRegisterAgent(ctx context.Context, r AgentRecord) error {
	body, _ := json.Marshal(r)
	req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/agents/preregister", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("preregister agent: status %d", resp.StatusCode)
	}
	return nil
}

func (c *HTTPClient) CheckSpawnPolicy(ctx context.Context, parentID, childType string) (SpawnDecision, error) {
	q := url.Values{"parentId": {parentID}, "childType": {childType}}
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/spawn-policy?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return SpawnDecision{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SpawnDecision{}, fmt.Errorf("spawn policy: status %d", resp.StatusCode)
	}
	var d SpawnDecision
	return d, json.NewDecoder(resp.Body).Decode(&d)
}

// Subtree returns the agent's id followed by all descendant ids (root-first),
// regardless of their lifecycle status. An unknown id yields (nil, nil) so the
// caller can distinguish "registry never heard of this id" (first-pass-empty →
// 404) from a transport/registry error.
func (c *HTTPClient) Subtree(ctx context.Context, id, userId string) ([]string, error) {
	u := c.base + "/v1/agents/" + id + "/subtree"
	if userId != "" {
		u += "?userId=" + url.QueryEscape(userId)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subtree: status %d", resp.StatusCode)
	}
	var out struct {
		Subtree []string `json:"subtree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Subtree, nil
}

func (c *HTTPClient) DismissAgent(ctx context.Context, agentID string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/agents/"+agentID+"/dismiss", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("dismiss agent: status %d", resp.StatusCode)
	}
	return nil
}

type Mock struct {
	Templates  map[string]AgentTemplate
	Agents     []AgentRecord
	Completed  []string
	Failed     []string
	EventStore map[string][]events.Event
	Subtrees   map[string][]string
}

func NewMock(templates map[string]AgentTemplate) *Mock {
	return &Mock{
		Templates:  templates,
		EventStore: map[string][]events.Event{},
	}
}

func (m *Mock) GetTemplate(_ context.Context, agentType string) (AgentTemplate, error) {
	t, ok := m.Templates[agentType]
	if !ok {
		return AgentTemplate{}, fmt.Errorf("template %q not found", agentType)
	}
	return t, nil
}

func (m *Mock) Complete(_ context.Context, agentID string) error {
	m.Completed = append(m.Completed, agentID)
	return nil
}

func (m *Mock) Fail(_ context.Context, agentID string) error {
	m.Failed = append(m.Failed, agentID)
	return nil
}

func (m *Mock) ListAgents(_ context.Context) ([]AgentRecord, error) {
	return m.Agents, nil
}

func (m *Mock) GetAgent(_ context.Context, id string) (AgentRecord, error) {
	for _, a := range m.Agents {
		if a.AgentID == id {
			return a, nil
		}
	}
	return AgentRecord{}, fmt.Errorf("agent %q not found", id)
}

func (m *Mock) ListEvents(_ context.Context, agentID string) ([]events.Event, error) {
	return m.EventStore[agentID], nil
}

func (m *Mock) PostEvent(_ context.Context, agentID string, e events.Event) error {
	m.EventStore[agentID] = append(m.EventStore[agentID], e)
	return nil
}

func (m *Mock) ListTemplates(_ context.Context) ([]string, error) {
	types := make([]string, 0, len(m.Templates))
	for k := range m.Templates {
		types = append(types, k)
	}
	return types, nil
}

func (m *Mock) DismissAgent(_ context.Context, agentID string) error {
	return nil
}

func (m *Mock) Subtree(_ context.Context, id, _ string) ([]string, error) {
	return m.Subtrees[id], nil
}

func (m *Mock) PreRegisterAgent(_ context.Context, r AgentRecord) error {
	r.Status = "pending"
	m.Agents = append(m.Agents, r)
	return nil
}

func (m *Mock) CheckSpawnPolicy(_ context.Context, parentID, childType string) (SpawnDecision, error) {
	var parentType string
	for _, a := range m.Agents {
		if a.AgentID == parentID {
			parentType = a.AgentType
			break
		}
	}
	if parentType == "" {
		return SpawnDecision{Reason: "unknown parent"}, nil
	}
	tpl, ok := m.Templates[parentType]
	if !ok {
		return SpawnDecision{Reason: "parent template not found"}, nil
	}
	for _, ct := range tpl.Delegation.AllowedChildTypes {
		if ct == childType {
			return SpawnDecision{Allowed: true}, nil
		}
	}
	return SpawnDecision{Reason: fmt.Sprintf("%s may not spawn %s", parentType, childType)}, nil
}
