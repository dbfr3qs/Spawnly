// cmd/agent-sidecar/ciba.go
//
// CIBA (OpenID Client-Initiated Backchannel Authentication) driver for
// consent-gated agents. When the parent template flags this agent type with
// requireUserConsent, the sidecar runs a backchannel authentication request at
// spawn: the spawning user approves it on the dashboard (or a stored consent
// auto-approves it server-side), and the resulting access token — bound to the
// user, with this agent in the act chain — is what /token serves to the agent.
//
// Renewals re-run the grant. While the stored consent stands they auto-approve
// on the first poll (no human involved); if the user revokes consent, the next
// renewal goes pending and token issuance stops within the token lifetime —
// real-time revocation of the agent's user-bound access.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/registry"
)

var (
	errConsentPending = errors.New("user consent pending")
	errConsentDenied  = errors.New("user consent denied")
	errConsentExpired = errors.New("consent request expired before the user responded")
)

// cibaTokenSource owns the lifecycle of one agent's CIBA grant: at most one
// outstanding backchannel request, the granted token, and poll pacing state.
type cibaTokenSource struct {
	cfg     config
	cibaURL string
	// source fetches the attestation credential presented on the backchannel
	// (client_assertion). Injectable for tests; defaults to cfg.source.
	source attestor.Source

	mu        sync.Mutex
	token     string
	expiry    time.Time
	authReqID string
	reqExpiry time.Time
	interval  time.Duration
	nextPoll  time.Time
	denied    bool
}

func newCibaTokenSource(cfg config) *cibaTokenSource {
	return &cibaTokenSource{
		cfg: cfg,
		// The backchannel endpoint lives next to the token endpoint.
		cibaURL: strings.Replace(cfg.isTokenURL, "/connect/token", "/connect/ciba", 1),
		source:  cfg.source,
	}
}

// scopes returns the consent scope set the backchannel request asks for —
// the template-declared oauthScopes threaded through CONSENT_SCOPES.
func (cs *cibaTokenSource) scopes() string {
	if cs.cfg.consentScopes == "" {
		return "openid"
	}
	return cs.cfg.consentScopes
}

// covered reports whether every requested scope is inside the consent set, so
// /token can refuse scope escalation locally with a clear error.
func (cs *cibaTokenSource) covered(requested string) bool {
	return registry.FirstUncoveredScope(strings.Fields(cs.scopes()), strings.Fields(requested)) == ""
}

// initiate opens a fresh backchannel authentication request. Callers hold mu.
func (cs *cibaTokenSource) initiate(ctx context.Context) error {
	cred, err := cs.source.Fetch(ctx, cs.cfg.isTokenURL)
	if err != nil {
		return fmt.Errorf("fetch credential: %w", err)
	}
	form := url.Values{
		"client_id":             {cs.cfg.agentType},
		"client_assertion_type": {cred.AssertionType},
		"client_assertion":      {cred.Value},
		"scope":                 {cs.scopes()},
		"login_hint":            {"user:" + cs.cfg.userID},
		"binding_message":       {fmt.Sprintf("%s requests %s", cs.cfg.agentType, cs.scopes())},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cs.cibaURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backchannel auth request: IS returned %d: %s", resp.StatusCode, b)
	}
	var out struct {
		AuthReqID string `json:"auth_req_id"`
		ExpiresIn int    `json:"expires_in"`
		Interval  int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode backchannel auth response: %w", err)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	cs.authReqID = out.AuthReqID
	cs.reqExpiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	cs.interval = time.Duration(out.Interval) * time.Second
	cs.nextPoll = time.Now() // an auto-approved request resolves on the first poll
	cs.denied = false
	return nil
}

// pollOnce redeems the outstanding auth_req_id at the token endpoint once.
// Returns nil when the token was granted, errConsentPending while waiting,
// or a terminal error (denied / expired). Callers hold mu.
func (cs *cibaTokenSource) pollOnce(ctx context.Context) error {
	cred, err := cs.source.Fetch(ctx, cs.cfg.isTokenURL)
	if err != nil {
		return fmt.Errorf("fetch credential: %w", err)
	}
	form := url.Values{
		"grant_type":            {"urn:openid:params:grant-type:ciba"},
		"auth_req_id":           {cs.authReqID},
		"client_id":             {cs.cfg.agentType},
		"client_assertion_type": {cred.AssertionType},
		"client_assertion":      {cred.Value},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cs.cfg.isTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var tok struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &tok); err != nil {
			return fmt.Errorf("decode CIBA token response: %w", err)
		}
		if tok.ExpiresIn == 0 {
			tok.ExpiresIn = 3600
		}
		cs.token = tok.AccessToken
		cs.expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		cs.authReqID = ""
		return nil
	}

	var oauthErr struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &oauthErr)
	switch oauthErr.Error {
	case "authorization_pending":
		cs.nextPoll = time.Now().Add(cs.interval)
		return errConsentPending
	case "slow_down":
		cs.interval += 5 * time.Second
		cs.nextPoll = time.Now().Add(cs.interval)
		return errConsentPending
	case "access_denied":
		cs.authReqID = ""
		cs.denied = true
		return errConsentDenied
	case "expired_token":
		cs.authReqID = ""
		return errConsentExpired
	default:
		return fmt.Errorf("CIBA token poll: IS returned %d: %s", resp.StatusCode, body)
	}
}

// waitForGrant drives the startup consent flow to a verdict: initiate the
// backchannel request and poll until the user (or a stored consent) approves,
// denies, or the request expires. The lock is held per request, never across
// a poll sleep — /token is served concurrently during startup and must get
// its documented 503-while-pending instead of blocking on the mutex.
func (cs *cibaTokenSource) waitForGrant(ctx context.Context) error {
	cs.mu.Lock()
	var err error
	// An early /token call may have initiated already; never open a second
	// outstanding request (it would surface a duplicate dashboard prompt).
	if cs.authReqID == "" && cs.token == "" && !cs.denied {
		err = cs.initiate(ctx)
	}
	cs.mu.Unlock()
	if err != nil {
		return err
	}
	for {
		cs.mu.Lock()
		// A concurrent /token call may have polled the verdict while we slept;
		// honor it instead of re-polling a consumed auth_req_id.
		switch {
		case cs.denied:
			cs.mu.Unlock()
			return errConsentDenied
		case cs.token != "":
			cs.mu.Unlock()
			return nil
		case cs.authReqID == "":
			cs.mu.Unlock()
			return errConsentExpired
		}
		err := cs.pollOnce(ctx)
		next := cs.nextPoll
		cs.mu.Unlock()
		if err == nil {
			return nil
		}
		if !errors.Is(err, errConsentPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(next)):
		}
	}
}

// get serves the agent's /token path: the cached user-bound token while it is
// fresh, otherwise a renewal via a new backchannel request. While the stored
// consent stands, renewal resolves on the first poll (auto-approve); a revoked
// consent leaves the request pending (errConsentPending, surfaced as 503) and
// subsequent calls keep polling the same request so a dashboard re-approval
// brings the agent back without restarts.
func (cs *cibaTokenSource) get(ctx context.Context) (string, int, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.denied {
		return "", 0, errConsentDenied
	}
	if cs.token != "" && time.Until(cs.expiry) > 30*time.Second {
		return cs.token, int(time.Until(cs.expiry).Seconds()), nil
	}

	if cs.authReqID == "" || time.Now().After(cs.reqExpiry) {
		if err := cs.initiate(ctx); err != nil {
			return "", 0, err
		}
	}
	if time.Now().Before(cs.nextPoll) {
		return "", 0, errConsentPending
	}
	if err := cs.pollOnce(ctx); err != nil {
		if errors.Is(err, errConsentExpired) {
			// The pending renewal lapsed; a fresh request opens on the next call.
			return "", 0, errConsentPending
		}
		return "", 0, err
	}
	return cs.token, int(time.Until(cs.expiry).Seconds()), nil
}

// updateStatus PATCHes the agent's registry record (e.g. awaiting-consent,
// active, failed). Terminal statuses also drop SpiceDB authority registry-side.
//
// The registry authorizes this PATCH via the agent's SVID (self-scope: an agent
// may set only its OWN status). These calls happen AFTER the CIBA consent wait
// (minutes later), so a startup SVID can be stale — fetch a FRESH credential per
// call, exactly as run()/selfRegister authenticate to the registry.
func updateStatus(ctx context.Context, cfg config, agentID, status string) error {
	cred, err := cfg.source.Fetch(ctx, "registry")
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"status": status})
	req, err := http.NewRequestWithContext(ctx, "PATCH",
		cfg.registryURL+"/v1/agents/"+agentID, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cred.Value)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry PATCH %s -> %s: %d: %s", agentID, status, resp.StatusCode, b)
	}
	return nil
}
