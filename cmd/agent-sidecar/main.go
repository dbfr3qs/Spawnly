package main

import (
	"context"
	"encoding/json"
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

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/agent-platform/poc/internal/events"
	"github.com/agent-platform/poc/internal/registry"
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
}

func fetchJWT(ctx context.Context, socketPath, audience string) (string, error) {
	var svid *jwtsvid.SVID
	var err error
	for i := 0; i < 10; i++ {
		svid, err = workloadapi.FetchJWTSVID(ctx,
			jwtsvid.Params{Audience: audience},
			workloadapi.WithAddr(socketPath),
		)
		if err == nil {
			return svid.Marshal(), nil
		}
		log.Printf("waiting for SPIRE identity (attempt %d/10): %v", i+1, err)
		time.Sleep(3 * time.Second)
	}
	return "", err
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

func postEvent(ctx context.Context, registryURL, agentID string, e events.Event) {
	if err := events.New(registryURL).PostEvent(ctx, agentID, e); err != nil {
		log.Printf("warn: post event %s: %v", e.Type, err)
	}
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

type tokenCache struct {
	mu         sync.Mutex
	token      string
	expiry     time.Time
	cfg        config
	socketPath string
}

func (tc *tokenCache) get(ctx context.Context, scope string) (string, int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.token != "" && time.Until(tc.expiry) > 30*time.Second {
		return tc.token, int(time.Until(tc.expiry).Seconds()), nil
	}

	svid, err := fetchJWT(ctx, tc.socketPath, tc.cfg.isTokenURL)
	if err != nil {
		return "", 0, fmt.Errorf("fetch SVID: %w", err)
	}

	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {tc.cfg.agentType},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {svid},
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
func exchangeToken(ctx context.Context, cfg config, socketPath, subjectToken, audience, scope string) (string, int, error) {
	svid, err := fetchJWT(ctx, socketPath, cfg.isTokenURL)
	if err != nil {
		return "", 0, fmt.Errorf("fetch SVID: %w", err)
	}

	form := url.Values{
		"grant_type":            {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":         {subjectToken},
		"subject_token_type":    {"urn:ietf:params:oauth:token-type:access_token"},
		"actor_token":           {svid},
		"actor_token_type":      {"urn:ietf:params:oauth:token-type:jwt"},
		"client_id":             {cfg.agentType},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {svid},
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
func clientCredentialsToken(ctx context.Context, cfg config, socketPath, scope, audience string) (string, int, error) {
	svid, err := fetchJWT(ctx, socketPath, cfg.isTokenURL)
	if err != nil {
		return "", 0, fmt.Errorf("fetch SVID: %w", err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {cfg.agentType},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {svid},
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
	// 1. Fetch SVID for registry
	regSVID, err := fetchJWT(ctx, cfg.socketPath, "registry")
	if err != nil {
		return fmt.Errorf("fetch registry SVID: %w", err)
	}

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
	postEvent(ctx, cfg.registryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "started",
		Payload: mustMarshal(map[string]string{"agentId": agentID}),
	})
	postEvent(ctx, cfg.registryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "svid_fetched",
		Payload: mustMarshal(map[string]string{"svid": regSVID}),
	})

	// 4. Start HTTP server (registration already succeeded at this point)
	tc := &tokenCache{cfg: cfg, socketPath: cfg.socketPath}

	registered := true // we're past self-register
	_ = registered

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

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
			tok, expiresIn, err = exchangeToken(r.Context(), cfg, cfg.socketPath, subjectToken, audience, scope)
		} else if audience != "" {
			// Client-credentials with an explicit audience — used to mint a
			// delegation token (audience=delegation) to hand to a child. Not
			// cached: audience/scope are request-specific.
			tok, expiresIn, err = clientCredentialsToken(r.Context(), cfg, cfg.socketPath, scope, audience)
		} else {
			if scope == "" {
				scope = "sample-api"
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
		postEvent(r.Context(), cfg.registryURL, agentID, events.Event{
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

	srv := &http.Server{Addr: ":8089", Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		postEvent(shutCtx, cfg.registryURL, agentID, events.Event{
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

func main() {
	cfg := config{
		agentID:     os.Getenv("AGENT_ID"),
		agentType:   os.Getenv("AGENT_TYPE"),
		tenantID:    os.Getenv("TENANT_ID"),
		userID:      os.Getenv("USER_ID"),
		parentID:    os.Getenv("PARENT_ID"),
		registryURL: os.Getenv("REGISTRY_URL"),
		isTokenURL:  os.Getenv("IS_TOKEN_URL"),
		socketPath:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}

	// tenantID is optional: a global agent has no tenant. The empty value
	// flows harmlessly through selfRegister's "tenantId" field, and the token
	// path does not use it at all.
	if cfg.registryURL == "" || cfg.isTokenURL == "" {
		log.Fatal("REGISTRY_URL and IS_TOKEN_URL are required")
	}
	if cfg.agentType == "" {
		cfg.agentType = "worker"
	}
	if cfg.socketPath == "" {
		cfg.socketPath = "unix:///spiffe-workload-api/spire-agent.sock"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("sidecar error: %v", err)
	}
	log.Println("sidecar exiting")
}
