// agents/go-worker/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/spawnly/sdk-go"
)

// config holds the worker's runtime inputs, sourced from the environment.
type config struct {
	task         string
	sampleAPIURL string
	tenantID     string
	scope        string
}

// configFromEnv reads the worker's env contract. SCOPE defaults to "sample-api";
// SAMPLE_API_URL is required.
func configFromEnv() (config, error) {
	cfg := config{
		task:         os.Getenv("TASK"),
		sampleAPIURL: os.Getenv("SAMPLE_API_URL"),
		tenantID:     os.Getenv("TENANT_ID"),
		scope:        os.Getenv("SCOPE"),
	}
	if cfg.scope == "" {
		cfg.scope = "sample-api"
	}
	if cfg.sampleAPIURL == "" {
		return config{}, fmt.Errorf("SAMPLE_API_URL is required")
	}
	return cfg, nil
}

// run authenticates against the sidecar (via the SDK's TokenClient) and POSTs
// the task to the sample API, returning the API's "result" field. The SDK's
// AuthenticatedClient injects Authorization and X-Tenant-ID for us.
func run(ctx context.Context, cfg config, tc *spawnly.TokenClient) (string, error) {
	client := spawnly.NewAuthenticatedClient(cfg.sampleAPIURL, cfg.scope, tc, spawnly.WithTenantID(cfg.tenantID))

	body, err := json.Marshal(map[string]string{"task": cfg.task})
	if err != nil {
		return "", fmt.Errorf("marshal task body: %w", err)
	}

	resp, err := client.Post(ctx, "/task", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("calling sample API: %w", err)
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

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	result, err := run(ctx, cfg, spawnly.NewTokenClient())
	if err != nil {
		log.Fatalf("call sample API: %v", err)
	}

	log.Printf("task result: %s", result)
}
