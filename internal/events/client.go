package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type Client interface {
	PostEvent(ctx context.Context, agentID string, e Event) error
}

// HTTPClient posts events to the registry's events endpoint.
type HTTPClient struct {
	base string
	http *http.Client
}

func New(baseURL string) *HTTPClient {
	return &HTTPClient{base: baseURL, http: &http.Client{}}
}

func (c *HTTPClient) PostEvent(ctx context.Context, agentID string, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.base+"/v1/agents/"+agentID+"/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
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

// MockClient collects posted events for use in tests.
type MockClient struct {
	mu     sync.Mutex
	Events map[string][]Event
}

func NewMock() *MockClient {
	return &MockClient{Events: map[string][]Event{}}
}

func (m *MockClient) PostEvent(_ context.Context, agentID string, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Events[agentID] = append(m.Events[agentID], e)
	return nil
}
