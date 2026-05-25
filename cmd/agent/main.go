// cmd/agent/main.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/agent-platform/poc/internal/events"
	"github.com/agent-platform/poc/internal/registry"
)

type SVIDFetcher interface {
	FetchJWT(ctx context.Context, audience string) (string, error)
}

type workloadFetcher struct{ socketPath string }

func (f *workloadFetcher) FetchJWT(ctx context.Context, audience string) (string, error) {
	// SPIRE may not have created the registration entry yet — retry for up to 30s.
	var svid *jwtsvid.SVID
	var err error
	for i := 0; i < 10; i++ {
		svid, err = workloadapi.FetchJWTSVID(ctx,
			jwtsvid.Params{Audience: audience},
			workloadapi.WithAddr(f.socketPath),
		)
		if err == nil {
			return svid.Marshal(), nil
		}
		log.Printf("waiting for SPIRE identity (attempt %d/10): %v", i+1, err)
		time.Sleep(3 * time.Second)
	}
	return "", err
}

type AgentConfig struct {
	TenantID     string
	UserID       string
	AgentType    string
	RegistryURL  string
	ISTokenURL   string
	SampleAPIURL string
	Task         string
}

func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	json.Unmarshal(b, &claims)
	return claims
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func postEvent(ctx context.Context, registryURL, agentID string, e events.Event) {
	if err := events.New(registryURL).PostEvent(ctx, agentID, e); err != nil {
		log.Printf("warn: post event %s: %v", e.Type, err)
	}
}

func selfRegister(ctx context.Context, registryURL, svid, agentType, tenantID, userID string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"agentType": agentType, "tenantId": tenantID, "userId": userID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", registryURL+"/v1/agents",
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
	log.Printf("self-registered as %s (status: %s)", rec.AgentID, rec.Status)
	return rec.AgentID, nil
}

func exchangeForAccessToken(ctx context.Context, isTokenURL, agentType, svid string) (string, error) {
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {agentType},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {svid},
		"scope":                 {"sample-api"},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", isTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("IS returned %d: %s", resp.StatusCode, b)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	return tok.AccessToken, nil
}

func callSampleAPI(ctx context.Context, sampleAPIURL, accessToken, tenantID, task string) (string, error) {
	if task != "" {
		body, _ := json.Marshal(map[string]string{"task": task})
		req, err := http.NewRequestWithContext(ctx, "POST", sampleAPIURL+"/task",
			strings.NewReader(string(body)))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-ID", tenantID)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("calling sample API (task): %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("sample API returned %d: %s", resp.StatusCode, b)
		}
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", fmt.Errorf("decode task response: %w", err)
		}
		return fmt.Sprintf("%v", result["result"]), nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", sampleAPIURL+"/work", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling sample API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("sample API returned %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	log.Printf("agent completed work: %s", body)
	return "", nil
}

func runAgent(ctx context.Context, cfg AgentConfig, fetcher SVIDFetcher) error {
	type bufferedEvent struct {
		e events.Event
	}
	var earlyEvents []bufferedEvent

	// 1. Fetch registry SVID
	regSVID, err := fetcher.FetchJWT(ctx, "registry")
	if err != nil {
		return fmt.Errorf("fetch registry SVID: %w", err)
	}
	earlyEvents = append(earlyEvents, bufferedEvent{events.Event{
		Source: events.SourceAgent,
		Type:   "svid_acquired",
		Payload: mustMarshal(map[string]any{
			"audience": "registry",
			"claims":   decodeJWTPayload(regSVID),
		}),
	}})

	// 2. Self-register
	agentID, err := selfRegister(ctx, cfg.RegistryURL, regSVID, cfg.AgentType, cfg.TenantID, cfg.UserID)
	if err != nil {
		return fmt.Errorf("self-register: %w", err)
	}

	// Flush buffered events now that we have agentID
	for _, be := range earlyEvents {
		postEvent(ctx, cfg.RegistryURL, agentID, be.e)
	}
	postEvent(ctx, cfg.RegistryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "registry_self_registered",
		Payload: mustMarshal(map[string]string{"agentId": agentID}),
	})

	// 3. Fetch IS SVID
	isSVID, err := fetcher.FetchJWT(ctx, cfg.ISTokenURL)
	if err != nil {
		return fmt.Errorf("fetch IS SVID: %w", err)
	}
	postEvent(ctx, cfg.RegistryURL, agentID, events.Event{
		Source: events.SourceAgent,
		Type:   "token_requested",
		Payload: mustMarshal(map[string]any{
			"audience": cfg.ISTokenURL,
			"claims":   decodeJWTPayload(isSVID),
		}),
	})

	// 4. Exchange for access token
	accessToken, err := exchangeForAccessToken(ctx, cfg.ISTokenURL, cfg.AgentType, isSVID)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	postEvent(ctx, cfg.RegistryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "token_received",
		Payload: mustMarshal(map[string]any{"claims": decodeJWTPayload(accessToken)}),
	})

	// 5. Call sample API
	postEvent(ctx, cfg.RegistryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "task_dispatched",
		Payload: mustMarshal(map[string]string{"task": cfg.Task, "url": cfg.SampleAPIURL}),
	})
	result, err := callSampleAPI(ctx, cfg.SampleAPIURL, accessToken, cfg.TenantID, cfg.Task)
	if err != nil {
		return fmt.Errorf("call sample API: %w", err)
	}
	postEvent(ctx, cfg.RegistryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "task_result",
		Payload: mustMarshal(map[string]string{"result": result}),
	})

	// 6. Complete
	postEvent(ctx, cfg.RegistryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "agent_completed",
		Payload: mustMarshal(map[string]string{"status": "ok"}),
	})
	return nil
}

func main() {
	tenantID := os.Getenv("TENANT_ID")
	userID := os.Getenv("USER_ID")
	agentType := os.Getenv("AGENT_TYPE")
	registryURL := os.Getenv("REGISTRY_URL")
	isTokenURL := os.Getenv("IS_TOKEN_URL")
	sampleAPIURL := os.Getenv("SAMPLE_API_URL")
	socketPath := os.Getenv("SPIFFE_ENDPOINT_SOCKET")
	task := os.Getenv("TASK")

	if tenantID == "" || registryURL == "" || isTokenURL == "" || sampleAPIURL == "" {
		log.Fatal("TENANT_ID, REGISTRY_URL, IS_TOKEN_URL, and SAMPLE_API_URL are required")
	}
	if agentType == "" {
		agentType = "worker"
	}
	if socketPath == "" {
		socketPath = "unix:///spiffe-workload-api/spire-agent.sock"
	}

	cfg := AgentConfig{
		TenantID: tenantID, UserID: userID, AgentType: agentType,
		RegistryURL: registryURL, ISTokenURL: isTokenURL, SampleAPIURL: sampleAPIURL,
		Task: task,
	}
	fetcher := &workloadFetcher{socketPath: socketPath}
	if err := runAgent(context.Background(), cfg, fetcher); err != nil {
		log.Fatalf("agent failed: %v", err)
	}
	log.Println("agent exiting successfully")
}
