// Package channel defines the NEUTRAL, versioned wire protocol for the
// remote-substrate dispatch channel (remote-substrate Phase 4): the persistent,
// client-initiated WebSocket a substrate-agent holds to the orchestrator.
//
// # Dependency direction
//
// This package is platform contract. The consumer-side cmd/substrate-agent
// depends on it; the orchestrator depends on it; it depends on NOTHING
// kind-, SPIRE-, or transport-specific ([[dependency-direction-rule]]). It knows
// only neutral spawn/kill/status frames — never how a substrate materializes a
// pod. The transport (coder/websocket) lives in the two adapters, not here, so
// the protocol can be exercised by a plain marshal/unmarshal test.
//
// # Directions & trust
//
//	down (CP → substrate): spawn{record fields, template ref}, kill{agentId}
//	up   (substrate → CP): status{agentId, phase}, heartbeat
//
// The channel is SERVER-AUTHORITATIVE on org: the orchestrator binds a
// connection to the org of the caller's verified substrate-agent SVID and never
// reads an org from any frame. SpawnCommand.OrgID is therefore INFORMATIONAL
// only — a compromised substrate cannot widen its scope by lying in a frame,
// and a compromised CP-view cannot be spoofed because the substrate only ever
// receives its own org's work over its own connection.
package channel

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the wire-format version. Both peers stamp it on every frame
// and reject a mismatch (fail closed) so an incompatible build can never be
// silently misinterpreted.
const ProtocolVersion = 1

// Frame type discriminators.
const (
	// Down frame types (CP → substrate).
	TypeSpawn = "spawn"
	TypeKill  = "kill"

	// Up frame types (substrate → CP).
	TypeStatus    = "status"
	TypeHeartbeat = "heartbeat"
)

// Status phases a substrate reports UP for a dispatched agent. They are advisory
// (dashboard/telemetry) — no control decision on the CP trusts them, and a
// substrate can only ever report about its OWN org's agents (its connection is
// org-bound). Kept neutral (no kind-specifics).
const (
	PhaseCreating = "creating" // local CR created, pod not yet observed
	PhaseCreated  = "created"  // pod/workload materialized
	PhaseFailed   = "failed"   // create/reconcile failed on the substrate
	PhaseKilled   = "killed"   // local CR + entry torn down
)

// Down is a control-plane → substrate frame. Exactly one of Spawn/Kill is set,
// selected by Type.
type Down struct {
	V     int           `json:"v"`
	Type  string        `json:"type"`
	Spawn *SpawnCommand `json:"spawn,omitempty"`
	Kill  *KillCommand  `json:"kill,omitempty"`
}

// SpawnCommand carries the neutral registry-record fields the substrate needs to
// materialize a local AgentWorkload. AgentID is the server-assigned workload id
// (the registry record's primary key); the substrate makes a CR of that name and
// requests the org-scoped SPIRE entry for it.
type SpawnCommand struct {
	AgentID   string `json:"agentId"`
	AgentType string `json:"agentType"` // the template ref
	UserID    string `json:"userId"`
	TenantID  string `json:"tenantId,omitempty"`
	// OrgID is INFORMATIONAL only. The substrate's authoritative org is its SVID,
	// fixed server-side for the connection; it MUST NOT use this field for any
	// authorization or entry-scoping decision. The CP sends it purely so the
	// substrate can stamp neutral labels. (Server-authoritative-org invariant.)
	OrgID           string `json:"orgId,omitempty"`
	Task            string `json:"task,omitempty"`
	ParentID        string `json:"parentId,omitempty"`
	Lifecycle       string `json:"lifecycle,omitempty"`
	ConsentRequired bool   `json:"consentRequired,omitempty"`
	SupportsChat    bool   `json:"supportsChat,omitempty"`
}

// KillCommand tears down one dispatched agent (delete local CR + org-scoped
// entry).
type KillCommand struct {
	AgentID string `json:"agentId"`
}

// Up is a substrate → control-plane frame. Status is set for TypeStatus;
// heartbeats carry no payload.
type Up struct {
	V      int           `json:"v"`
	Type   string        `json:"type"`
	Status *StatusReport `json:"status,omitempty"`
}

// StatusReport is an advisory phase report for one agent the substrate is
// handling. Message is optional human detail (never trusted for control).
type StatusReport struct {
	AgentID string `json:"agentId"`
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
}

// ── constructors ──────────────────────────────────────────────────────────────

// NewSpawn builds a versioned spawn Down frame.
func NewSpawn(cmd SpawnCommand) Down {
	return Down{V: ProtocolVersion, Type: TypeSpawn, Spawn: &cmd}
}

// NewKill builds a versioned kill Down frame.
func NewKill(agentID string) Down {
	return Down{V: ProtocolVersion, Type: TypeKill, Kill: &KillCommand{AgentID: agentID}}
}

// NewStatus builds a versioned status Up frame.
func NewStatus(agentID, phase, msg string) Up {
	return Up{V: ProtocolVersion, Type: TypeStatus, Status: &StatusReport{AgentID: agentID, Phase: phase, Message: msg}}
}

// NewHeartbeat builds a versioned heartbeat Up frame.
func NewHeartbeat() Up { return Up{V: ProtocolVersion, Type: TypeHeartbeat} }

// ── validation (fail closed) ────────────────────────────────────────────────

// ErrBadVersion is returned when a frame's protocol version is not
// ProtocolVersion. Kept distinct so a peer can log a clear "upgrade needed".
var ErrBadVersion = errors.New("substrate channel: unsupported protocol version")

// Validate checks a Down frame is well-formed for its version and type. A
// malformed frame is rejected rather than acted on.
func (d Down) Validate() error {
	if d.V != ProtocolVersion {
		return fmt.Errorf("%w: got %d want %d", ErrBadVersion, d.V, ProtocolVersion)
	}
	switch d.Type {
	case TypeSpawn:
		if d.Spawn == nil || d.Spawn.AgentID == "" || d.Spawn.AgentType == "" {
			return errors.New("substrate channel: spawn frame missing agentId/agentType")
		}
	case TypeKill:
		if d.Kill == nil || d.Kill.AgentID == "" {
			return errors.New("substrate channel: kill frame missing agentId")
		}
	default:
		return fmt.Errorf("substrate channel: unknown down type %q", d.Type)
	}
	return nil
}

// Validate checks an Up frame is well-formed for its version and type.
func (u Up) Validate() error {
	if u.V != ProtocolVersion {
		return fmt.Errorf("%w: got %d want %d", ErrBadVersion, u.V, ProtocolVersion)
	}
	switch u.Type {
	case TypeStatus:
		if u.Status == nil || u.Status.AgentID == "" || u.Status.Phase == "" {
			return errors.New("substrate channel: status frame missing agentId/phase")
		}
	case TypeHeartbeat:
		// no payload
	default:
		return fmt.Errorf("substrate channel: unknown up type %q", u.Type)
	}
	return nil
}

// ── encode/decode helpers ───────────────────────────────────────────────────
//
// These give the two adapters (and the encode/decode unit test) one funnel that
// stamps/validates the version, independent of the WebSocket transport.

// EncodeDown marshals+validates a Down frame to JSON bytes.
func EncodeDown(d Down) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

// DecodeDown unmarshals+validates a Down frame from JSON bytes.
func DecodeDown(b []byte) (Down, error) {
	var d Down
	if err := json.Unmarshal(b, &d); err != nil {
		return Down{}, err
	}
	if err := d.Validate(); err != nil {
		return Down{}, err
	}
	return d, nil
}

// EncodeUp marshals+validates an Up frame to JSON bytes.
func EncodeUp(u Up) ([]byte, error) {
	if err := u.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(u)
}

// DecodeUp unmarshals+validates an Up frame from JSON bytes.
func DecodeUp(b []byte) (Up, error) {
	var u Up
	if err := json.Unmarshal(b, &u); err != nil {
		return Up{}, err
	}
	if err := u.Validate(); err != nil {
		return Up{}, err
	}
	return u, nil
}
