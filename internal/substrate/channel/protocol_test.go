package channel

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestEncodeDecodeDown_RoundTrip(t *testing.T) {
	orig := NewSpawn(SpawnCommand{AgentID: "agent-1", AgentType: "worker", UserID: "u", OrgID: "acme", Task: "go"})
	b, err := EncodeDown(orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeDown(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != TypeSpawn || got.Spawn == nil || got.Spawn.AgentID != "agent-1" || got.Spawn.AgentType != "worker" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.V != ProtocolVersion {
		t.Fatalf("version = %d", got.V)
	}
}

func TestEncodeDecodeUp_RoundTrip(t *testing.T) {
	for _, u := range []Up{
		NewStatus("agent-1", PhaseCreated, "ok"),
		NewHeartbeat(),
	} {
		b, err := EncodeUp(u)
		if err != nil {
			t.Fatalf("encode %s: %v", u.Type, err)
		}
		got, err := DecodeUp(b)
		if err != nil {
			t.Fatalf("decode %s: %v", u.Type, err)
		}
		if got.Type != u.Type {
			t.Fatalf("type = %q want %q", got.Type, u.Type)
		}
	}
}

func TestDown_Validate(t *testing.T) {
	tests := []struct {
		name string
		d    Down
		ok   bool
	}{
		{"good spawn", NewSpawn(SpawnCommand{AgentID: "a", AgentType: "t"}), true},
		{"good kill", NewKill("a"), true},
		{"spawn missing agentId", Down{V: ProtocolVersion, Type: TypeSpawn, Spawn: &SpawnCommand{AgentType: "t"}}, false},
		{"spawn missing type payload", Down{V: ProtocolVersion, Type: TypeSpawn}, false},
		{"kill missing agentId", Down{V: ProtocolVersion, Type: TypeKill, Kill: &KillCommand{}}, false},
		{"unknown type", Down{V: ProtocolVersion, Type: "nope"}, false},
		{"bad version", Down{V: 999, Type: TypeKill, Kill: &KillCommand{AgentID: "a"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.d.Validate()
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want error")
			}
		})
	}
}

func TestUp_Validate_BadVersionIsTyped(t *testing.T) {
	err := Up{V: 2, Type: TypeHeartbeat}.Validate()
	if !errors.Is(err, ErrBadVersion) {
		t.Fatalf("want ErrBadVersion, got %v", err)
	}
}

// A decoder must reject a frame whose version is wrong even if the JSON is valid.
func TestDecodeDown_RejectsBadVersion(t *testing.T) {
	b, _ := json.Marshal(Down{V: 42, Type: TypeKill, Kill: &KillCommand{AgentID: "a"}})
	if _, err := DecodeDown(b); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("want ErrBadVersion, got %v", err)
	}
}
