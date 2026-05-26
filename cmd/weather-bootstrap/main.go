// cmd/weather-bootstrap/main.go
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

func main() {
	registryURL := os.Getenv("REGISTRY_URL")
	tenantID := os.Getenv("TENANT_ID")
	userID := os.Getenv("USER_ID")
	isTokenURL := os.Getenv("IS_TOKEN_URL")
	agentType := os.Getenv("AGENT_TYPE")
	socketPath := os.Getenv("SPIFFE_ENDPOINT_SOCKET")

	if registryURL == "" || tenantID == "" || userID == "" || isTokenURL == "" {
		log.Fatal("REGISTRY_URL, TENANT_ID, USER_ID, and IS_TOKEN_URL are required")
	}
	if agentType == "" {
		agentType = "weather-monitor"
	}
	if socketPath == "" {
		socketPath = "unix:///spiffe-workload-api/spire-agent.sock"
	}

	ctx := context.Background()
	fetcher := &workloadFetcher{socketPath: socketPath}

	// 1. Fetch SVID for registry audience
	regSVID, err := fetcher.FetchJWT(ctx, "registry")
	if err != nil {
		log.Fatalf("fetch registry SVID: %v", err)
	}

	// 2. Buffer svid_acquired event (no agentId yet)
	type bufferedEvent struct {
		e events.Event
	}
	earlyEvents := []bufferedEvent{
		{events.Event{
			Source: events.SourceAgent,
			Type:   "svid_acquired",
			Payload: mustMarshal(map[string]any{
				"audience": "registry",
				"claims":   decodeJWTPayload(regSVID),
			}),
		}},
	}

	// 3. Self-register → get agentId
	agentID, err := selfRegister(ctx, registryURL, regSVID, agentType, tenantID, userID)
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}

	// 4. Flush buffered events + post registry_self_registered
	for _, be := range earlyEvents {
		postEvent(ctx, registryURL, agentID, be.e)
	}
	postEvent(ctx, registryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "registry_self_registered",
		Payload: mustMarshal(map[string]string{"agentId": agentID}),
	})

	// 5. Fetch SVID for IS_TOKEN_URL audience
	isSVID, err := fetcher.FetchJWT(ctx, isTokenURL)
	if err != nil {
		log.Fatalf("fetch IS SVID: %v", err)
	}

	// 6. Post token_requested event
	postEvent(ctx, registryURL, agentID, events.Event{
		Source: events.SourceAgent,
		Type:   "token_requested",
		Payload: mustMarshal(map[string]any{
			"audience": isTokenURL,
			"claims":   decodeJWTPayload(isSVID),
		}),
	})

	// 7. Exchange for OAuth2 access token
	accessToken, err := exchangeForAccessToken(ctx, isTokenURL, agentType, isSVID)
	if err != nil {
		log.Fatalf("token exchange: %v", err)
	}

	// 8. Post token_received event
	postEvent(ctx, registryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "token_received",
		Payload: mustMarshal(map[string]any{"claims": decodeJWTPayload(accessToken)}),
	})

	// 9. Post agent_loop_started event
	postEvent(ctx, registryURL, agentID, events.Event{
		Source:  events.SourceAgent,
		Type:    "agent_loop_started",
		Payload: mustMarshal(map[string]string{"agentId": agentID}),
	})

	// 10. Print agentId to stdout so start.sh can capture it
	fmt.Println(agentID)
	os.Exit(0)
}
