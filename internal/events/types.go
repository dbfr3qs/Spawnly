package events

import (
	"encoding/json"
	"time"
)

type Source string

const (
	SourceOrchestrator Source = "ORCHESTRATOR"
	SourceOperator     Source = "OPERATOR"
	SourceRegistry     Source = "REGISTRY"
	SourceAgent        Source = "AGENT"
)

type Event struct {
	ID        string          `json:"id"`
	Timestamp time.Time       `json:"timestamp"`
	Source    Source          `json:"source"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}
