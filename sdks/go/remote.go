package spawnly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// RemoteRegistration is the request to register a CONSUMER-HOSTED (remote) agent
// with an organization (multi-tenancy Phase 10). It is sent by an org MEMBER,
// authenticated with that member's delegated user token — the same auth model as
// a dashboard spawn, so the registering member owns the resulting agent. The org
// must already have an ACTIVE attestation anchor (an org admin registers + proves
// control of it once); otherwise registration is refused, since a remote agent
// can only ever authenticate through the org's anchor.
type RemoteRegistration struct {
	// OrchestratorURL is the base URL of the Spawnly orchestrator (the public
	// spawnly.run edge in a real deploy).
	OrchestratorURL string
	// OrgID is the organization the agent is registered into (the org whose anchor
	// will attest it). It is the {id} path segment of the registration endpoint.
	OrgID string
	// MemberToken is the registering member's delegated access token (the same
	// token the dashboard uses to spawn). It carries the org claim; the server
	// derives ownership + org from it, never from this body.
	MemberToken string
	// AgentType is the template (OAuth client_id) the remote agent runs as. It must
	// be visible in the org (a global template or one scoped to OrgID).
	AgentType string
	// Task is an optional free-form task string, mirroring a dashboard spawn.
	Task string
	// TenantID is the optional consumer tenant primitive (required only when the
	// template has requiresTenant=true). Orthogonal to OrgID.
	TenantID string
	// HTTPClient overrides the client used for the registration call (tests).
	HTTPClient *http.Client
}

// RegisterRemoteAgent registers a consumer-hosted agent with the member token and
// returns the server-generated agentId. Run the sidecar (ATTESTOR=aws-sts,
// AGENT_PLACEMENT=remote) with AGENT_ID set to this value and the consumer's
// ambient AWS credentials for the anchored role; the sidecar self-registers and
// mints tokens through the org's anchor, and the agent appears in the registry/
// dashboard with full lifecycle parity (list, events, revoke). Spawnly never
// sees the consumer's AWS credentials — only the presigned STS presentation.
//
// This helper talks only to the neutral orchestrator HTTP contract (POST
// /v1/orgs/{id}/agents); it imports no platform package.
func RegisterRemoteAgent(ctx context.Context, reg RemoteRegistration) (string, error) {
	if reg.OrchestratorURL == "" || reg.OrgID == "" || reg.MemberToken == "" || reg.AgentType == "" {
		return "", fmt.Errorf("RegisterRemoteAgent: OrchestratorURL, OrgID, MemberToken and AgentType are required")
	}
	body, err := json.Marshal(map[string]string{
		"agentType": reg.AgentType,
		"task":      reg.Task,
		"tenantId":  reg.TenantID,
	})
	if err != nil {
		return "", err
	}
	endpoint := reg.OrchestratorURL + "/v1/orgs/" + url.PathEscape(reg.OrgID) + "/agents"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+reg.MemberToken)
	req.Header.Set("Content-Type", "application/json")

	hc := reg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return "", fmt.Errorf("register remote agent: %d %s", resp.StatusCode, string(b))
	}
	var out struct {
		AgentID string `json:"agentId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AgentID == "" {
		return "", fmt.Errorf("register remote agent: response missing agentId")
	}
	return out.AgentID, nil
}
