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

		// ── substrate-agent well-known identity (remote-substrate Phase 3) ──
		{"substrate-agent", "spiffe://cluster.local/org/acme/substrate-agent", "cluster.local", "acme", "substrate-agent", false},
		{"substrate-agent other td", "spiffe://spawnly.example/org/globex/substrate-agent", "spawnly.example", "globex", "substrate-agent", false},
		{"substrate-agent empty org spoof", "spiffe://cluster.local/org//substrate-agent", "", "", "", true},
		// A substrate-agent with an extra segment is NOT the well-known identity and
		// is not a valid workload path either ⇒ malformed.
		{"substrate-agent extra seg", "spiffe://cluster.local/org/acme/substrate-agent/x", "", "", "", true},

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

// The SubstrateAgent flag distinguishes the well-known substrate-agent identity
// from a workload whose agentId merely happens to be "substrate-agent".
func TestParsePath_SubstrateAgentFlag(t *testing.T) {
	sa, err := ParsePath("spiffe://cluster.local/org/acme/substrate-agent")
	if err != nil || !sa.SubstrateAgent || sa.OrgID != "acme" || sa.AgentID != SubstrateAgentLeaf {
		t.Fatalf("substrate-agent parse = %+v, err=%v", sa, err)
	}
	// A workload path ending in "substrate-agent" is NOT the well-known identity.
	wl, err := ParsePath("spiffe://cluster.local/org/acme/agent/t/u/ty/substrate-agent")
	if err != nil {
		t.Fatalf("workload parse err: %v", err)
	}
	if wl.SubstrateAgent {
		t.Fatal("a /agent/.../substrate-agent workload must NOT set SubstrateAgent")
	}
	// A platform path never sets it.
	pf, _ := ParsePath("spiffe://cluster.local/agent/substrate-agent")
	if pf.SubstrateAgent {
		t.Fatal("platform path must not set SubstrateAgent")
	}
}

// BuildSubstrateAgentPath round-trips through ParsePath to the same identity.
func TestBuildSubstrateAgentPath_RoundTrip(t *testing.T) {
	id, err := BuildSubstrateAgentPath("cluster.local", "acme")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if id != "spiffe://cluster.local/org/acme/substrate-agent" {
		t.Fatalf("built id = %q", id)
	}
	p, err := ParsePath(id)
	if err != nil || !p.SubstrateAgent || p.OrgID != "acme" {
		t.Fatalf("round-trip parse = %+v, err=%v", p, err)
	}
	if _, err := BuildSubstrateAgentPath("cluster.local", "a/b"); err == nil {
		t.Fatal("invalid org must be rejected")
	}
	if _, err := BuildSubstrateAgentPath("cluster.local", ""); err == nil {
		t.Fatal("empty org must be rejected")
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
