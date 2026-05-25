// internal/registry/client.go
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agent-platform/poc/internal/events"
)

type Client interface {
	GetTemplate(ctx context.Context, agentType string) (AgentTemplate, error)
	Complete(ctx context.Context, agentID string) error
	Fail(ctx context.Context, agentID string) error
	ListAgents(ctx context.Context) ([]AgentRecord, error)
	ListEvents(ctx context.Context, agentID string) ([]events.Event, error)
	PostEvent(ctx context.Context, agentID string, e events.Event) error
}

type HTTPClient struct {
	base string
	http *http.Client
}

func New(baseURL string) *HTTPClient {
	return &HTTPClient{base: baseURL, http: &http.Client{}}
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

type Mock struct {
	Templates  map[string]AgentTemplate
	Agents     []AgentRecord
	Completed  []string
	Failed     []string
	EventStore map[string][]events.Event
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

func (m *Mock) ListEvents(_ context.Context, agentID string) ([]events.Event, error) {
	return m.EventStore[agentID], nil
}

func (m *Mock) PostEvent(_ context.Context, agentID string, e events.Event) error {
	m.EventStore[agentID] = append(m.EventStore[agentID], e)
	return nil
}
