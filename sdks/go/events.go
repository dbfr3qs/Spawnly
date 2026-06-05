package spawnly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// eventEnvelope is the neutral wire shape the registry expects for lifecycle
// events. Replicated locally so the SDK never imports a platform package.
type eventEnvelope struct {
	Source  string `json:"source"`
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// PostEvent posts a lifecycle event for agentID to the registry at
//
//	POST <registryURL>/v1/agents/<agentID>/events
//
// with the body {"source":"agent","type":eventType,"payload":payload}.
//
// This is BEST-EFFORT telemetry: a failure here should not break the agent.
// Unlike the TypeScript SDK (which swallows the error and logs a warning), this
// returns the error so callers can decide what to do, but it is safe to ignore.
// It never panics.
func PostEvent(ctx context.Context, registryURL, agentID, eventType string, payload any) error {
	body, err := json.Marshal(eventEnvelope{
		Source:  "agent",
		Type:    eventType,
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	url := registryURL + "/v1/agents/" + agentID + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post event: %w", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("post event: registry returned %d", res.StatusCode)
	}
	return nil
}
