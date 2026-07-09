// Command remote-demo is a MINIMAL e2e demo of a consumer-hosted (remote) agent
// (multi-tenancy Phase 10). It stands in for a real consumer workload: it does
// nothing but exercise the platform's neutral token contract — mint a token from
// the co-located sidecar (localhost:8089/token) on a loop and post an audit event
// recording whether the mint SUCCEEDED or FAILED.
//
// Those events let the e2e observe lifecycle parity end-to-end: while the org
// anchor governs the agent, mints succeed (criterion 11 mint half); after a
// dashboard REVOKE the record is severed and the next mint FAILS (criterion 11
// lifecycle-parity half). The agent holds no credential itself — it only talks to
// the local sidecar, exactly like any platform-hosted agent.
//
// stdlib only: this lives in the platform module and imports no SDK, so it builds
// with the standard builder stage. A real remote agent would use the Go SDK
// (sdks/go) instead.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	agentID := os.Getenv("AGENT_ID")
	if agentID == "" {
		log.Fatal("AGENT_ID is required")
	}
	scope := os.Getenv("MINT_SCOPE")
	if scope == "" {
		scope = "openid"
	}
	// Requesting an audience routes the sidecar through its UNCACHED mint path, so
	// every loop hits Keycloak fresh. That makes a dashboard REVOKE observable
	// within one tick (the sidecar's plain-token cache would otherwise keep serving
	// a valid token until it expired, masking the revocation for minutes).
	audience := os.Getenv("MINT_AUDIENCE")
	if audience == "" {
		audience = "spawnly"
	}
	// The sidecar listens on localhost:8089; both /token and the event proxy.
	sidecar := "http://localhost:8089"

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log.Printf("remote-demo starting (agentId=%s scope=%s)", agentID, scope)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		ok, detail := mintOnce(ctx, sidecar, scope, audience)
		evType := "mint_ok"
		if !ok {
			evType = "mint_failed"
		}
		postEvent(ctx, sidecar, agentID, evType, detail)
		log.Printf("%s: %s", evType, detail)
		select {
		case <-ctx.Done():
			log.Println("remote-demo exiting")
			return
		case <-ticker.C:
		}
	}
}

// mintOnce requests a token from the sidecar and reports success + a short detail.
func mintOnce(ctx context.Context, sidecar, scope, audience string) (bool, string) {
	reqURL := sidecar + "/token?scope=" + url.QueryEscape(scope)
	if audience != "" {
		reqURL += "&audience=" + url.QueryEscape(audience)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "sidecar unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, "token issued"
	}
	return false, fmt.Sprintf("token status %d", resp.StatusCode)
}

// postEvent posts an audit event through the sidecar's event proxy (which attaches
// the sidecar's own credential). Best-effort — a failed post never stops the loop.
func postEvent(ctx context.Context, sidecar, agentID, evType, detail string) {
	body := fmt.Sprintf(`{"source":"agent","type":%q,"payload":{"detail":%q}}`, evType, detail)
	reqURL := sidecar + "/v1/agents/" + url.PathEscape(agentID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
