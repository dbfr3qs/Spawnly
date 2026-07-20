package spiffe

import "testing"

// The canonical path contract, exhaustively. These vectors are the single
// source of truth the Java SpiffePathTest mirrors — keep them in lockstep.
func TestParsePath(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantTD  string
		wantOrg string
		wantID  string
		wantErr bool
	}{
		// ── platform (org-less) shapes — must stay bit-identical to path.Base ──
		{"platform tenanted", "spiffe://cluster.local/agent/acme/alice/weather/agent-abc", "cluster.local", "", "agent-abc", false},
		{"platform global", "spiffe://cluster.local/agent/weather/agent-xyz", "cluster.local", "", "agent-xyz", false},
		{"platform single segment", "spiffe://cluster.local/agent/legacy", "cluster.local", "", "legacy", false},
		{"platform trailing slash", "spiffe://cluster.local/agent/weather/id/", "cluster.local", "", "id", false},

		// ── org-scoped shape ──
		{"org-scoped full", "spiffe://cluster.local/org/acme/agent/t/u/type/id", "cluster.local", "acme", "id", false},
		{"org-scoped trailing slash", "spiffe://cluster.local/org/acme/agent/id/", "cluster.local", "acme", "id", false},
		{"org-scoped different td", "spiffe://spawnly.example/org/globex/agent/id", "spawnly.example", "globex", "id", false},

		// ── spoofs & malformed — MUST fail closed ──
		{"empty org spoof //", "spiffe://cluster.local/org//agent/x", "", "", "", true},
		{"org prefix no agent", "spiffe://cluster.local/org/acme", "", "", "", true},
		{"org prefix wrong third seg", "spiffe://cluster.local/org/acme/foo/x", "", "", "", true},
		{"org prefix truncated", "spiffe://cluster.local/org", "", "", "", true},
		{"org agent no id segment", "spiffe://cluster.local/org/acme/agent", "", "", "", true},

		// ── "org" appearing deeper is NOT an org claim — platform, no smuggle ──
		{"agent-then-org subpath", "spiffe://cluster.local/agent/org/x", "cluster.local", "", "x", false},

		// ── non-spiffe / degenerate ──
		{"not spiffe", "https://cluster.local/agent/x", "", "", "", true},
		{"empty string", "", "", "", "", true},
		{"scheme only", "spiffe://", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParsePath(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParsePath(%q) = %+v, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePath(%q): unexpected error %v", c.in, err)
			}
			if got.TrustDomain != c.wantTD || got.OrgID != c.wantOrg || got.AgentID != c.wantID {
				t.Fatalf("ParsePath(%q) = {td:%q org:%q id:%q}, want {td:%q org:%q id:%q}",
					c.in, got.TrustDomain, got.OrgID, got.AgentID, c.wantTD, c.wantOrg, c.wantID)
			}
		})
	}
}

// BuildOrgAgentPath is the inverse of ParsePath's org-scoped shape: whatever it
// emits MUST parse back to the same {td, org, agentID}. This round-trip is the
// contract the substrate entry creator relies on.
func TestBuildOrgAgentPath(t *testing.T) {
	t.Run("round-trips through ParsePath", func(t *testing.T) {
		cases := []struct {
			td, org string
			segs    []string
			wantID  string
		}{
			{"cluster.local", "acme", []string{"t", "u", "weather", "agent-abc"}, "agent-abc"},
			{"cluster.local", "globex", []string{"agent-xyz"}, "agent-xyz"},
			{"spawnly.example", "acme", []string{"tenant", "alice", "chain", "id-1"}, "id-1"},
		}
		for _, c := range cases {
			id, err := BuildOrgAgentPath(c.td, c.org, c.segs...)
			if err != nil {
				t.Fatalf("BuildOrgAgentPath(%q,%q,%v): %v", c.td, c.org, c.segs, err)
			}
			p, err := ParsePath(id)
			if err != nil {
				t.Fatalf("ParsePath(%q) built id failed: %v", id, err)
			}
			if p.TrustDomain != c.td || p.OrgID != c.org || p.AgentID != c.wantID {
				t.Fatalf("round-trip %q = {td:%q org:%q id:%q}, want {td:%q org:%q id:%q}",
					id, p.TrustDomain, p.OrgID, p.AgentID, c.td, c.org, c.wantID)
			}
		}
	})

	t.Run("fails closed on smuggling / empty inputs", func(t *testing.T) {
		bad := []struct {
			name    string
			td, org string
			segs    []string
		}{
			{"empty td", "", "acme", []string{"id"}},
			{"td with slash smuggle", "cluster.local/x", "acme", []string{"id"}},
			{"empty org", "cluster.local", "", []string{"id"}},
			{"org with slash smuggle", "cluster.local", "a/agent/x", []string{"id"}},
			{"no segments", "cluster.local", "acme", nil},
			{"empty segment", "cluster.local", "acme", []string{""}},
			{"segment with slash", "cluster.local", "acme", []string{"a/b"}},
		}
		for _, c := range bad {
			if id, err := BuildOrgAgentPath(c.td, c.org, c.segs...); err == nil {
				t.Fatalf("%s: BuildOrgAgentPath = %q, want error", c.name, id)
			}
		}
	})
}
