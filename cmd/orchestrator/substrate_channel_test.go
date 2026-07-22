package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/spiffe"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// ── hub: push / newest-wins / own-org-only ──────────────────────────────────

func newTestConn(org string) *substrateConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &substrateConn{org: org, send: make(chan channel.Down, 4), ctx: ctx, cancel: cancel}
}

func reqWithBearer(tok string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/substrate/channel", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func TestHub_PushOwnOrgOnly(t *testing.T) {
	h := newSubstrateHub()
	connA := newTestConn("acme")
	h.register(connA)

	// A push to acme is delivered to acme's channel.
	if !h.push("acme", channel.NewKill("a")) {
		t.Fatal("push to connected org acme returned false")
	}
	select {
	case <-connA.send:
	default:
		t.Fatal("acme push not enqueued to acme conn")
	}

	// A push to globex (no connection) is not delivered and reports offline — org-B
	// work can never reach org-A's channel.
	if h.push("globex", channel.NewKill("b")) {
		t.Fatal("push to unconnected org globex returned true")
	}
	if len(connA.send) != 0 {
		t.Fatal("globex push leaked into acme's channel")
	}
}

func TestHub_NewestWinsReplacesPriorConn(t *testing.T) {
	h := newSubstrateHub()
	old := newTestConn("acme")
	h.register(old)
	newConn := newTestConn("acme")
	h.register(newConn) // newest-wins: replaces old

	// The old connection's context must be cancelled.
	select {
	case <-old.ctx.Done():
	default:
		t.Fatal("prior connection was not cancelled on replacement")
	}
	// A push now targets the new connection only.
	h.push("acme", channel.NewKill("x"))
	if len(newConn.send) != 1 {
		t.Fatalf("push did not reach the newest conn (len=%d)", len(newConn.send))
	}
	if len(old.send) != 0 {
		t.Fatal("push reached the replaced (old) conn")
	}
	// Unregistering the OLD conn must NOT evict the current (new) one.
	h.unregister(old)
	if !h.connected("acme") {
		t.Fatal("unregistering the replaced conn wrongly evicted the current one")
	}
}

func TestHub_PushDropsOnFullBuffer(t *testing.T) {
	h := newSubstrateHub()
	conn := &substrateConn{org: "acme", send: make(chan channel.Down)} // unbuffered ⇒ always full
	ctx, cancel := context.WithCancel(context.Background())
	conn.cancel = cancel
	h.register(conn)
	if h.push("acme", channel.NewKill("x")) {
		t.Fatal("push onto a full buffer must report not-delivered")
	}
	select {
	case <-ctx.Done(): // the slow conn was cancelled so it reconnects+reconciles
	default:
		t.Fatal("a full-buffer push must drop (cancel) the connection")
	}
}

// ── substrate SVID authenticator: non-substrate refused, own-org surfaced ────

func TestSpiffeSubstrateAuth_RequiresSubstrateIdentity(t *testing.T) {
	auth := func(spiffeID string) *spiffeSubstrateAuth {
		return &spiffeSubstrateAuth{
			validator:   &spiffe.MockSVIDValidator{SpiffeID: spiffeID},
			trustDomain: "cluster.local",
			audience:    "orchestrator",
		}
	}
	req := reqWithBearer("tok")

	// A substrate-agent SVID authenticates and surfaces its org.
	org, err := auth("spiffe://cluster.local/org/acme/substrate-agent").authenticate(context.Background(), req)
	if err != nil || org != "acme" {
		t.Fatalf("substrate SVID: org=%q err=%v, want acme/nil", org, err)
	}

	// A plain WORKLOAD SVID (not the substrate-agent identity) is refused — only the
	// substrate-agent may drive the channel/Entry API (least privilege).
	if _, err := auth("spiffe://cluster.local/org/acme/agent/t/u/ty/agent-1").authenticate(context.Background(), req); err == nil {
		t.Fatal("a workload SVID must be refused")
	}

	// A wrong trust domain is refused.
	if _, err := auth("spiffe://evil.example/org/acme/substrate-agent").authenticate(context.Background(), req); err == nil {
		t.Fatal("a wrong-trust-domain SVID must be refused")
	}

	// A platform (org-less) SVID is refused (no org to bind).
	if _, err := auth("spiffe://cluster.local/agent/foo").authenticate(context.Background(), req); err == nil {
		t.Fatal("an org-less SVID must be refused")
	}
}
