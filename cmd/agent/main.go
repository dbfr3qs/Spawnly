// cmd/agent/main.go
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
	"strings"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/agent-platform/poc/internal/registry"
)

type SVIDFetcher interface {
	FetchJWT(ctx context.Context, audience string) (string, error)
}

type workloadFetcher struct{ socketPath string }

func (f *workloadFetcher) FetchJWT(ctx context.Context, audience string) (string, error) {
	svid, err := workloadapi.FetchJWTSVID(ctx,
		jwtsvid.Params{Audience: audience},
		workloadapi.WithAddr(f.socketPath),
	)
	if err != nil {
		return "", err
	}
	return svid.Marshal(), nil
}

type AgentConfig struct {
	TenantID     string
	UserID       string
	AgentType    string
	RegistryURL  string
	ISTokenURL   string
	SampleAPIURL string
}

func runAgent(ctx context.Context, cfg AgentConfig, fetcher SVIDFetcher) error {
	regSVID, err := fetcher.FetchJWT(ctx, "registry")
	if err != nil {
		return fmt.Errorf("fetch registry SVID: %w", err)
	}

	if err := selfRegister(ctx, cfg.RegistryURL, regSVID, cfg.AgentType, cfg.TenantID, cfg.UserID); err != nil {
		return fmt.Errorf("self-register: %w", err)
	}

	isSVID, err := fetcher.FetchJWT(ctx, cfg.ISTokenURL)
	if err != nil {
		return fmt.Errorf("fetch IS SVID: %w", err)
	}

	accessToken, err := exchangeForAccessToken(ctx, cfg.ISTokenURL, cfg.AgentType, isSVID)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	return callSampleAPI(ctx, cfg.SampleAPIURL, accessToken, cfg.TenantID)
}

func selfRegister(ctx context.Context, registryURL, svid, agentType, tenantID, userID string) error {
	body, _ := json.Marshal(map[string]string{
		"agentType": agentType, "tenantId": tenantID, "userId": userID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", registryURL+"/v1/agents",
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+svid)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry returned %d: %s", resp.StatusCode, b)
	}
	var rec registry.AgentRecord
	json.NewDecoder(resp.Body).Decode(&rec)
	log.Printf("self-registered as %s (status: %s)", rec.AgentID, rec.Status)
	return nil
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

func callSampleAPI(ctx context.Context, sampleAPIURL, accessToken, tenantID string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", sampleAPIURL+"/work", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling sample API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sample API returned %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	log.Printf("agent completed work: %s", body)
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

	if tenantID == "" || registryURL == "" || isTokenURL == "" || sampleAPIURL == "" {
		log.Fatal("TENANT_ID, REGISTRY_URL, IS_TOKEN_URL, and SAMPLE_API_URL are required")
	}
	if agentType == "" {
		agentType = "worker"
	}
	if socketPath == "" {
		socketPath = "unix:///spiffe-workload-api/agent.sock"
	}

	cfg := AgentConfig{
		TenantID: tenantID, UserID: userID, AgentType: agentType,
		RegistryURL: registryURL, ISTokenURL: isTokenURL, SampleAPIURL: sampleAPIURL,
	}
	fetcher := &workloadFetcher{socketPath: socketPath}
	if err := runAgent(context.Background(), cfg, fetcher); err != nil {
		log.Fatalf("agent failed: %v", err)
	}
	log.Println("agent exiting successfully")
}
