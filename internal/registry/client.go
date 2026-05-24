// internal/registry/client.go
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type Client interface {
	GetTemplate(ctx context.Context, agentType string) (AgentTemplate, error)
	Complete(ctx context.Context, agentID string) error
	Fail(ctx context.Context, agentID string) error
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

type Mock struct {
	Templates map[string]AgentTemplate
	Completed []string
	Failed    []string
}

func NewMock(templates map[string]AgentTemplate) *Mock {
	return &Mock{Templates: templates}
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
