package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
)

type config struct {
	agentID     string
	agentType   string
	tenantID    string
	userID      string
	parentID    string
	registryURL string
	isTokenURL  string
	socketPath  string
	// placement is AGENT_PLACEMENT: "" / "k8s" for a platform-hosted agent (the
	// default), or "remote" for a CONSUMER-hosted agent running on the consumer's
	// own compute (multi-tenancy Phase 10). A remote agent has no operator to
	// assign its id or SPIRE socket: it MUST carry an AGENT_ID (from the org-scoped
	// registration API) and it attests via AWS STS (ATTESTOR=aws-sts), never the
	// SPIFFE workload socket.
	placement string
	// source fetches the attestation credential the sidecar presents to
	// Keycloak (a JWT-SVID under SPIFFE/SPIRE). Pluggable per ATTESTOR.
	source attestor.Source
	// consentRequired (CONSENT_REQUIRED=true, stamped by the orchestrator from
	// the parent template) gates the agent's tokens behind a CIBA backchannel
	// authentication approved by the spawning user. consentScopes is the
	// template-declared scope set that consent covers.
	consentRequired bool
	consentScopes   string
}

func selfRegister(ctx context.Context, cfg config, svid string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"agentType": cfg.agentType,
		"tenantId":  cfg.tenantID,
		"userId":    cfg.userID,
		"parentId":  cfg.parentID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.registryURL+"/v1/agents",
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+svid)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("registry returned %d: %s", resp.StatusCode, b)
	}
	var rec registry.AgentRecord
	json.NewDecoder(resp.Body).Decode(&rec)
	log.Printf("registered as %s (status: %s)", rec.AgentID, rec.Status)
	return rec.AgentID, nil
}

func postEvent(ctx context.Context, client events.Client, agentID string, e events.Event) {
	if err := client.PostEvent(ctx, agentID, e); err != nil {
		log.Printf("warn: post event %s: %v", e.Type, err)
	}
}

// newEventProxyHandler builds the sidecar's agent-facing event proxy. Agents are
// identity-blind by design — the SVID lives in this sidecar, never in the agent
// process — so an agent's own event posts (work_ok, started, child_spawned, …)
// carry no credential and the now-gated registry events endpoint would 401 them.
// The SDK's postEvent therefore targets the sidecar (localhost:8089); this
// handler forwards the body to the registry with the sidecar's OWN SVID as the
// bearer (via the shared `bearer` closure), so the agent never holds a token.
//
// Security model: this endpoint binds pod-wide (:8089 on all interfaces),
// exactly like the existing unauthenticated /token endpoint on the same server —
// so it adds NO exposure beyond what /token already grants a co-resident pod that
// knows the pod IP (agent pods are excluded from the NetworkPolicies). It is NOT
// loopback-only; do not assume otherwise. The real enforcement point is the
// REGISTRY's SVID gate: the proxy attaches only the sidecar's own SVID (never the
// control-plane secret), and the registry's events gate accepts a post only when
// that SVID resolves to the target id's own agent or its direct parent. So even
// though this hop trusts {id} from the URL, any caller — including a co-resident
// pod — can at most post as THIS agent (or its parent); it cannot forge events
// onto an unrelated agent.
func newEventProxyHandler(registryURL string, bearer func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		outURL := registryURL + "/v1/agents/" + url.PathEscape(id) + "/events"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, outURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build request", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if tok := bearer(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "forward to registry", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		// Propagate the registry's status + body back to the agent verbatim.
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

// svidBearer supplies the sidecar's registry-audience JWT-SVID as the bearer on
// event posts, re-fetching a fresh one before the cached copy nears expiry. The
// registry's events endpoint is now authenticated; the registry's SVID verifier
// validates this token and derives THIS agent's id, so the sidecar can post
// events about itself (and its direct parent) without ever holding the
// control-plane secret. Under CONTROL_PLANE_AUTH=none the registry ignores the
// header, so kind behaviour is unchanged.
type svidBearer struct {
	source attestor.Source
	mu     sync.Mutex
	token  string
	exp    time.Time
}

// get returns a cached SVID, refetching when empty or near expiry. Best-effort:
// a fetch failure returns "" (no header), so a post to a gated registry fails
// closed — which postEvent already tolerates (audit posts are never on the
// request critical path). It reuses the same attestor.Source the sidecar uses to
// self-register, so no new credential plumbing is introduced.
func (b *svidBearer) get() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.exp) {
		return b.token
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cred, err := b.source.Fetch(ctx, "registry")
	if err != nil {
		log.Printf("warn: fetch SVID for event auth: %v", err)
		return ""
	}
	b.token = cred.Value
	// Refresh 30s BEFORE the token's own exp, not on a fixed window: JWT-SVIDs
	// default to a SHORT TTL (~5min) and spire-agent serves cached tokens past
	// their half-life, so a fixed 5min window could hand out a token in its stale
	// tail and 401 on a gated tier — dropping the audit event. svidExpiry reads the
	// exp claim locally (no verification; we only use it to time a refetch).
	b.exp = svidRefreshAt(cred.Value, time.Now())
	return b.token
}

// svidRefreshAt returns when to refetch the SVID: 30s before its exp claim, so a
// post never carries a token in its stale tail. The token is decoded WITHOUT
// signature verification — the exp is used only to schedule a refetch, never to
// trust the token (the registry verifies it). Falls back to a conservative 1min
// window if the exp can't be read.
func svidRefreshAt(token string, now time.Time) time.Time {
	fallback := now.Add(time.Minute)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return fallback
	}
	refresh := time.Unix(claims.Exp, 0).Add(-30 * time.Second)
	if !refresh.After(now) {
		return fallback // already near/at exp — refetch soon rather than immediately hot-loop
	}
	return refresh
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

type tokenCache struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
	cfg    config
}

func (tc *tokenCache) get(ctx context.Context, scope string) (string, int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.token != "" && time.Until(tc.expiry) > 30*time.Second {
		return tc.token, int(time.Until(tc.expiry).Seconds()), nil
	}

	cred, err := tc.cfg.source.Fetch(ctx, tc.cfg.isTokenURL)
	if err != nil {
		return "", 0, fmt.Errorf("fetch credential: %w", err)
	}

	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {tc.cfg.agentType},
		"client_assertion_type": {cred.AssertionType},
		"client_assertion":      {cred.Value},
		"scope":                 {scope},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", tc.cfg.isTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("IS returned %d: %s", resp.StatusCode, b)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", 0, fmt.Errorf("decode token response: %w", err)
	}

	expiresIn := tok.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}
	tc.token = tok.AccessToken
	tc.expiry = time.Now().Add(time.Duration(expiresIn) * time.Second)
	return tc.token, expiresIn, nil
}

// exchangeToken performs an RFC 8693 OAuth2 token exchange. The provided
// subjectToken (a delegated access token received from an upstream caller) is
// exchanged for a fresh token, with this agent's SVID as the actor token so the
// resulting token carries an extended `act` chain. Exchanged tokens bypass the
// cache: they are short-lived and request-specific (audience/scope vary).
func exchangeToken(ctx context.Context, cfg config, subjectToken, audience, scope string) (string, int, error) {
	cred, err := cfg.source.Fetch(ctx, cfg.isTokenURL)
	if err != nil {
		return "", 0, fmt.Errorf("fetch credential: %w", err)
	}

	form := url.Values{
		"grant_type":            {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":         {subjectToken},
		"subject_token_type":    {"urn:ietf:params:oauth:token-type:access_token"},
		"actor_token":           {cred.Value},
		"actor_token_type":      {"urn:ietf:params:oauth:token-type:jwt"},
		"client_id":             {cfg.agentType},
		"client_assertion_type": {cred.AssertionType},
		"client_assertion":      {cred.Value},
	}
	if audience != "" {
		form.Set("audience", audience)
	}
	if scope != "" {
		form.Set("scope", scope)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.isTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("IS returned %d: %s", resp.StatusCode, b)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", 0, fmt.Errorf("decode token response: %w", err)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}
	return tok.AccessToken, expiresIn, nil
}

// clientCredentialsToken mints a fresh client_credentials token with an explicit
// audience. Used to obtain a delegation token (audience="delegation") that a
// parent hands to a child as the subject_token of a later exchange. Not cached:
// audience/scope are request-specific.
func clientCredentialsToken(ctx context.Context, cfg config, scope, audience string) (string, int, error) {
	cred, err := cfg.source.Fetch(ctx, cfg.isTokenURL)
	if err != nil {
		return "", 0, fmt.Errorf("fetch credential: %w", err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {cfg.agentType},
		"client_assertion_type": {cred.AssertionType},
		"client_assertion":      {cred.Value},
		"scope":                 {scope},
	}
	if audience != "" {
		form.Set("audience", audience)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.isTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("IS returned %d: %s", resp.StatusCode, b)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", 0, fmt.Errorf("decode token response: %w", err)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}
	return tok.AccessToken, expiresIn, nil
}

func run(ctx context.Context, cfg config) error {
	// Authenticated events client: posts carry the sidecar's registry-audience
	// SVID so the now-gated events endpoint accepts them (no-op header under the
	// local auth=none tier). Built once and threaded to every postEvent call. The
	// bearer instance is captured (not anonymous) so the same cached SVID is shared
	// by both the sidecar's own posts and the agent-facing event proxy below.
	bearer := &svidBearer{source: cfg.source}
	evtClient := events.NewWithToken(cfg.registryURL, bearer.get)

	// 1. Fetch the attestation credential for registry self-registration.
	regCred, err := cfg.source.Fetch(ctx, "registry")
	if err != nil {
		return fmt.Errorf("fetch registry credential: %w", err)
	}
	regSVID := regCred.Value

	// 2. Self-register; agentID may come from env or be assigned by registry
	agentID := cfg.agentID
	if agentID == "" {
		agentID, err = selfRegister(ctx, cfg, regSVID)
		if err != nil {
			return fmt.Errorf("self-register: %w", err)
		}
	} else {
		// Still register but use the pre-assigned ID implicitly; registry returns the record.
		assigned, err := selfRegister(ctx, cfg, regSVID)
		if err != nil {
			return fmt.Errorf("self-register: %w", err)
		}
		// Prefer operator-assigned ID but log if registry returned a different one.
		if assigned != agentID {
			log.Printf("note: AGENT_ID=%s but registry assigned %s; using registry value", agentID, assigned)
			agentID = assigned
		}
	}

	// 3. Post lifecycle events
	postEvent(ctx, evtClient, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "started",
		Payload: mustMarshal(map[string]string{"agentId": agentID}),
	})
	// Breadcrumb that attestation succeeded and the sidecar obtained its
	// registry-audience SVID. Do NOT put the token itself in the payload: events
	// are persisted and read back through the control-plane, and a JWT-SVID is a
	// live bearer credential — logging it would let any timeline reader replay it
	// as this agent for the token's lifetime. Record only that it happened.
	postEvent(ctx, evtClient, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "svid_fetched",
		Payload: mustMarshal(map[string]string{"result": "ok"}),
	})

	// 4. Consent gate: a consent-gated agent flips to awaiting-consent and runs
	// the CIBA flow in the background. /token serves 503 while pending (SDK
	// clients retry 5xx); denial or startup expiry is terminal — events are
	// posted to this agent and its parent and the record is marked failed
	// (dropping SpiceDB authority). The sidecar stays up serving 403s rather
	// than exiting: a native sidecar container is restarted by the kubelet
	// regardless of the pod's restart policy, and a restart would re-run the
	// whole consent flow for an agent the user already rejected.
	var ciba *cibaTokenSource
	var failConsent func(reason string)
	if cfg.consentRequired {
		if cfg.userID == "" {
			return fmt.Errorf("consent required but no USER_ID: nobody to ask")
		}
		ciba = newCibaTokenSource(cfg)

		// Terminal consent failure: notify this agent and its parent, mark the
		// record failed. Shared by the startup verdict and a denial at renewal;
		// Once keeps repeated /token 403s from re-posting the events.
		var failOnce sync.Once
		failConsent = func(reason string) {
			failOnce.Do(func() {
				log.Printf("consent not granted: %s", reason)
				payload := mustMarshal(map[string]string{
					"agentId": agentID, "childType": cfg.agentType, "reason": reason,
				})
				postEvent(ctx, evtClient, agentID, events.Event{
					Source: events.SourceAgent, Type: "consent_denied", Payload: payload,
				})
				if cfg.parentID != "" {
					postEvent(ctx, evtClient, cfg.parentID, events.Event{
						Source: events.SourceAgent, Type: "consent_denied", Payload: payload,
					})
				}
				if err := updateStatus(ctx, cfg, agentID, "failed"); err != nil {
					log.Printf("warn: mark failed: %v", err)
				}
			})
		}

		if err := updateStatus(ctx, cfg, agentID, "awaiting-consent"); err != nil {
			log.Printf("warn: set awaiting-consent: %v", err)
		}
		postEvent(ctx, evtClient, agentID, events.Event{
			Source: events.SourceAgent,
			Type:   "consent_requested",
			Payload: mustMarshal(map[string]string{
				"agentId": agentID, "userId": cfg.userID, "scopes": ciba.scopes(),
			}),
		})
		go func() {
			if err := ciba.waitForGrant(ctx); err != nil {
				if ctx.Err() != nil {
					return // shutting down anyway
				}
				failConsent(err.Error())
				return
			}
			if err := updateStatus(ctx, cfg, agentID, "active"); err != nil {
				log.Printf("warn: set active: %v", err)
			}
			postEvent(ctx, evtClient, agentID, events.Event{
				Source: events.SourceAgent,
				Type:   "consent_granted",
				Payload: mustMarshal(map[string]string{
					"agentId": agentID, "userId": cfg.userID, "scopes": ciba.scopes(),
				}),
			})
			log.Printf("user consent granted (user=%s scopes=%q)", cfg.userID, ciba.scopes())
		}()
	}

	// 5. Start HTTP server (registration already succeeded at this point)
	tc := &tokenCache{cfg: cfg}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	// Event proxy: agents post their own lifecycle/work events here (the SDK's
	// postEvent targets the sidecar, not the registry), and the sidecar forwards
	// them to the now-gated registry events endpoint with its OWN registry-audience
	// SVID as the bearer — the SAME svidBearer instance (bearer.get) the sidecar's
	// own event client uses, so both share one cached SVID.
	mux.HandleFunc("POST /v1/agents/{id}/events", newEventProxyHandler(cfg.registryURL, bearer.get))

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		scope := r.URL.Query().Get("scope")
		subjectToken := r.URL.Query().Get("subject_token")
		audience := r.URL.Query().Get("audience")

		var (
			tok       string
			expiresIn int
			err       error
		)
		if subjectToken != "" {
			// RFC 8693 token exchange: re-exchange an upstream caller's token,
			// adding this agent to the act chain.
			tok, expiresIn, err = exchangeToken(r.Context(), cfg, subjectToken, audience, scope)
		} else if audience != "" {
			// Client-credentials with an explicit audience — used to mint a
			// delegation token (audience=delegation) to hand to a child. Not
			// cached: audience/scope are request-specific.
			tok, expiresIn, err = clientCredentialsToken(r.Context(), cfg, scope, audience)
		} else if ciba != nil {
			// Consent-gated agent: plain tokens come from the CIBA grant, so
			// every renewal re-checks the stored consent — revoking it stops
			// token issuance within the token lifetime.
			if !ciba.covered(scope) {
				http.Error(w, fmt.Sprintf("scope %q exceeds the user-consented set %q", scope, ciba.scopes()),
					http.StatusForbidden)
				return
			}
			tok, expiresIn, err = ciba.get(r.Context())
			switch {
			case errors.Is(err, errConsentPending):
				w.Header().Set("Retry-After", "5")
				http.Error(w, "awaiting user consent", http.StatusServiceUnavailable)
				return
			case errors.Is(err, errConsentDenied):
				// A denial at renewal is as terminal as one at startup: same
				// events to agent and parent, same failed status (idempotent).
				failConsent("user denied consent")
				http.Error(w, "user denied consent", http.StatusForbidden)
				return
			}
		} else {
			if scope == "" {
				scope = "sample-api-a:read"
			}
			tok, expiresIn, err = tc.get(r.Context(), scope)
		}
		if err != nil {
			log.Printf("token error: %v", err)
			http.Error(w, "token exchange failed", http.StatusInternalServerError)
			return
		}

		eventType := "token_issued"
		if subjectToken != "" {
			eventType = "token_exchanged"
		}
		postEvent(r.Context(), evtClient, agentID, events.Event{
			Source: events.SourceAgent,
			Type:   eventType,
			Payload: mustMarshal(map[string]any{
				"scope":      scope,
				"audience":   audience,
				"expires_in": expiresIn,
				"token":      tok,
			}),
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": tok,
			"expires_in":   expiresIn,
		})
	})

	srv := &http.Server{
		Addr:              ":8089",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // Slowloris defense; /token requests are short.
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		postEvent(shutCtx, evtClient, agentID, events.Event{
			Source:  events.SourceAgent,
			Type:    "stopping",
			Payload: mustMarshal(map[string]string{"agentId": agentID}),
		})
		srv.Shutdown(shutCtx)
	}()

	log.Printf("sidecar listening on :8089 (agentId=%s)", agentID)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// validateConfig checks the sidecar's required configuration and applies the
// SPIFFE socket default. It mutates cfg in place (the socket default) and
// returns an error rather than fataling so main's invariants are unit-testable.
//
// Remote (consumer-hosted) mode (AGENT_PLACEMENT=remote, Phase 10): there is no
// operator to assign the id or mount a SPIRE socket, so the agent MUST carry an
// AGENT_ID (from the org-scoped registration API — the id whose record was
// preregistered remote+org-scoped) and it attests via AWS STS, so the SPIFFE
// socket default is deliberately NOT applied. The consumer's ambient AWS
// credentials presign the STS presentation; Spawnly never sees those credentials,
// only the presigned presentation the sidecar forwards.
func validateConfig(cfg *config) error {
	// tenantID is optional: a global agent has no tenant. The empty value flows
	// harmlessly through selfRegister's "tenantId" field, and the token path does
	// not use it at all.
	if cfg.registryURL == "" || cfg.isTokenURL == "" {
		return fmt.Errorf("REGISTRY_URL and IS_TOKEN_URL are required")
	}
	if cfg.agentType == "" {
		return fmt.Errorf("AGENT_TYPE is required (it is the OAuth client_id used to mint tokens)")
	}
	switch cfg.placement {
	case "", registry.PlacementK8s:
		// Platform-hosted: default the SPIFFE workload-API socket for the (default)
		// spiffe attestor; a non-spiffe ATTESTOR simply ignores it.
		if cfg.socketPath == "" {
			cfg.socketPath = "unix:///spiffe-workload-api/spire-agent.sock"
		}
	case registry.PlacementRemote:
		// Consumer-hosted: the id is not operator-assigned, so it must be supplied.
		if cfg.agentID == "" {
			return fmt.Errorf("AGENT_PLACEMENT=remote requires AGENT_ID (from the org-scoped registration API)")
		}
		// Do NOT default the SPIFFE socket: a remote agent has no SPIRE agent to
		// talk to; it attests via ATTESTOR=aws-sts against the org's anchor.
	default:
		return fmt.Errorf("unknown AGENT_PLACEMENT %q (want %q or %q)", cfg.placement, registry.PlacementK8s, registry.PlacementRemote)
	}
	return nil
}

func main() {
	cfg := config{
		agentID:         os.Getenv("AGENT_ID"),
		agentType:       os.Getenv("AGENT_TYPE"),
		tenantID:        os.Getenv("TENANT_ID"),
		userID:          os.Getenv("USER_ID"),
		parentID:        os.Getenv("PARENT_ID"),
		registryURL:     os.Getenv("REGISTRY_URL"),
		isTokenURL:      os.Getenv("IS_TOKEN_URL"),
		socketPath:      os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		placement:       os.Getenv("AGENT_PLACEMENT"),
		consentRequired: os.Getenv("CONSENT_REQUIRED") == "true",
		consentScopes:   os.Getenv("CONSENT_SCOPES"),
	}

	if err := validateConfig(&cfg); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Select the attestation credential source. Default is SPIFFE/SPIRE
	// (JWT-SVID via the workload API); other attestors (e.g. AWS IRSA) plug in
	// here behind the ATTESTOR selector.
	switch v := os.Getenv("ATTESTOR"); v {
	case "", "spiffe":
		cfg.source = &attestor.SpiffeSource{SocketPath: cfg.socketPath}
	case "aws-sts":
		src, err := attestor.NewAwsSource(ctx)
		if err != nil {
			log.Fatalf("AWS STS source init: %v", err)
		}
		cfg.source = src
	case "aws-stsweb":
		src, err := attestor.NewStsWebSource(ctx, os.Getenv("STSWEB_AUDIENCE"))
		if err != nil {
			log.Fatalf("AWS STS web-identity source init: %v", err)
		}
		cfg.source = src
	default:
		log.Fatalf("unknown ATTESTOR %q", v)
	}

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("sidecar error: %v", err)
	}
	log.Println("sidecar exiting")
}
