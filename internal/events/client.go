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

// NewWithToken is New plus a DYNAMIC bearer credential attached to every POST:
// token is called per request (so a refreshing source — the control-plane oidc
// bearer, or a sidecar re-fetching its SVID — is picked up each call). A nil
// token, or one that returns "", sends no Authorization header, which is exactly
// the local "none" tier where the registry's events endpoint enforces nothing.
// This is what lets the operator/orchestrator present the control-plane bearer
// and the sidecar present its SVID so the registry can authenticate the poster.
func NewWithToken(baseURL string, token func() string) *HTTPClient {
	hc := &http.Client{}
	if token != nil {
		hc.Transport = &bearerTransport{token: token, base: http.DefaultTransport}
	}
	return &HTTPClient{base: baseURL, http: hc}
}

// bearerTransport sets an Authorization header from its token source on every
// request before delegating. It clones the request (RoundTripper must not mutate
// a caller-owned *http.Request); an empty token yields no header (the registry
// then rejects rather than the client sending a blank bearer).
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
