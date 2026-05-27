// cmd/agent/main.go
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
	"time"
)

func getSidecarToken(ctx context.Context, scope string) (string, error) {
	url := "http://localhost:8089/token?scope=" + scope
	for i := 0; i < 5; i++ {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("sidecar not ready (attempt %d/5): %v", i+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("sidecar token endpoint returned %d: %s", resp.StatusCode, b)
		}
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return "", fmt.Errorf("decode sidecar token response: %w", err)
		}
		return tok.AccessToken, nil
	}
	return "", fmt.Errorf("sidecar token endpoint not available after 5 attempts")
}

func callSampleAPI(ctx context.Context, sampleAPIURL, accessToken, task string) (string, error) {
	body, _ := json.Marshal(map[string]string{"task": task})
	req, err := http.NewRequestWithContext(ctx, "POST", sampleAPIURL+"/task",
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
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
	task := os.Getenv("TASK")
	sampleAPIURL := os.Getenv("SAMPLE_API_URL")

	if sampleAPIURL == "" {
		log.Fatal("SAMPLE_API_URL is required")
	}

	ctx := context.Background()

	accessToken, err := getSidecarToken(ctx, "sample-api")
	if err != nil {
		log.Fatalf("get access token: %v", err)
	}

	result, err := callSampleAPI(ctx, sampleAPIURL, accessToken, task)
	if err != nil {
		log.Fatalf("call sample API: %v", err)
	}

	log.Printf("task result: %s", result)
}
