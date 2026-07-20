package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/registry"
)

// entryClient is the substrate's use of the CP-mediated Entry API: it authors and
// tears down its OWN org's SPIRE workload entries. The mechanism is HTTP to the
// orchestrator, authenticated per-call by a fresh substrate-agent SVID; the
// interface keeps the connect/handler logic testable.
type entryClient interface {
	CreateEntry(ctx context.Context, spiffeID, parentID string, selectors []string) error
	DeleteEntry(ctx context.Context, spiffeID string) error
}

// desiredLister pulls the caller org's substrate-placed records — the reconcile
// desired state. Org is derived server-side from the SVID, never sent.
type desiredLister interface {
	ListDesired(ctx context.Context) ([]registry.AgentRecord, error)
}

// cpClient talks to the orchestrator over the public contract, authenticating
// every request with a FRESH substrate-agent JWT-SVID (audience = the
// orchestrator) from the local SPIRE workload API. Fetching per call (not once)
// means a long-lived channel never presents an expired SVID to the Entry API or
// list endpoint.
type cpClient struct {
	orchestratorURL string
	audience        string
	source          attestor.Source
	http            *http.Client
}

func newCPClient(orchestratorURL, audience string, source attestor.Source) *cpClient {
	return &cpClient{
		orchestratorURL: orchestratorURL,
		audience:        audience,
		source:          source,
		http:            &http.Client{Timeout: 30 * time.Second},
	}
}

// svid fetches a fresh JWT-SVID for the orchestrator audience.
func (c *cpClient) svid(ctx context.Context) (string, error) {
	cred, err := c.source.Fetch(ctx, c.audience)
	if err != nil {
		return "", fmt.Errorf("fetch SVID: %w", err)
	}
	return cred.Value, nil
}

func (c *cpClient) CreateEntry(ctx context.Context, spiffeID, parentID string, selectors []string) error {
	body, _ := json.Marshal(map[string]any{"spiffeId": spiffeID, "parentId": parentID, "selectors": selectors})
	req, err := c.authed(ctx, http.MethodPost, c.orchestratorURL+"/v1/substrate/entries", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, http.StatusCreated)
}

func (c *cpClient) DeleteEntry(ctx context.Context, spiffeID string) error {
	u := c.orchestratorURL + "/v1/substrate/entries?spiffeId=" + url.QueryEscape(spiffeID)
	req, err := c.authed(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	return c.do(req, http.StatusNoContent)
}

func (c *cpClient) ListDesired(ctx context.Context) ([]registry.AgentRecord, error) {
	req, err := c.authed(ctx, http.MethodGet, c.orchestratorURL+"/v1/substrate/agents", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("list substrate agents: %d: %s", resp.StatusCode, b)
	}
	var recs []registry.AgentRecord
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// authed builds a request carrying a fresh substrate-agent SVID bearer.
func (c *cpClient) authed(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	svid, err := c.svid(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+svid)
	return req, nil
}

func (c *cpClient) do(req *http.Request, wantStatus int) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("%s %s: %d: %s", req.Method, req.URL.Path, resp.StatusCode, b)
	}
	return nil
}
