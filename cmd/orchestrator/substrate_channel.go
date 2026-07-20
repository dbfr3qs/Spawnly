package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate/channel"
	"github.com/spawnly/platform/internal/substrate/devca"
)

// substrateHub holds the LIVE dispatch-channel connections, keyed by orgId. It is
// the orchestrator's in-memory liveness+routing state for the remote-substrate
// feature (remote-substrate Phase 4), consistent with the dashboard-session
// precedent (in-memory, lost on restart — the substrate reconnects and
// reconciles). It is the ONLY place a substrate connection lives; every push is
// org-keyed here, so a compromised substrate can never receive another org's
// work (own-org-only, both directions).
//
// One connection per org, NEWEST-WINS: a second connect for an org replaces
// (and closes) the first. Rationale: a substrate that reconnects after a network
// blip must not be locked out by a stale half-dead connection the server hasn't
// yet noticed; the newest authenticated SVID for the org owns the channel. (The
// alternative — reject-the-second — risks a wedged org after an ungraceful drop.)
type substrateHub struct {
	mu    sync.Mutex
	conns map[string]*substrateConn
}

func newSubstrateHub() *substrateHub {
	return &substrateHub{conns: map[string]*substrateConn{}}
}

// substrateConn is one live channel to an org's substrate-agent.
type substrateConn struct {
	org      string
	ws       *websocket.Conn
	send     chan channel.Down
	ctx      context.Context
	cancel   context.CancelFunc
	connated time.Time
	// lastSeen is the unix-nanos timestamp of the most recent frame read from the
	// substrate (heartbeat or status). It bounds liveness staleness for the
	// dashboard: the reader's per-frame deadline (channelReadTimeout) already drops
	// a silent channel — so `connected` is bounded — and lastSeen surfaces "how
	// fresh". Atomic: written by the reader goroutine, read by the status handler.
	lastSeen atomic.Int64
}

// register installs conn as org's current connection, closing+cancelling any
// prior one (newest-wins). Returns the caller's cancel-bound context.
func (h *substrateHub) register(conn *substrateConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev := h.conns[conn.org]; prev != nil {
		log.Printf("substrate channel: org %s reconnected — replacing prior connection (newest-wins)", conn.org)
		if prev.cancel != nil {
			prev.cancel()
		}
		if prev.ws != nil {
			_ = prev.ws.Close(websocket.StatusPolicyViolation, "replaced by newer connection")
		}
	}
	h.conns[conn.org] = conn
}

// unregister removes conn IFF it is still the current connection for its org (a
// newest-wins replacement must not have the old conn's teardown evict the new
// one).
func (h *substrateHub) unregister(conn *substrateConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[conn.org] == conn {
		delete(h.conns, conn.org)
	}
}

// connected reports whether org currently holds a live channel.
func (h *substrateHub) connected(org string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[org] != nil
}

// liveness reports the org's substrate channel state for the dashboard: online
// (a live connection is registered) and the last time a frame was seen. This is
// PURE in-memory connection state — reading it (or a disconnect that clears it)
// NEVER mutates a registry record (spec AC 8: no terminal sweep on disconnect).
// Bounded staleness comes from the reader's heartbeat/read timeout: a substrate
// that stops heartbeating is unregistered within channelReadTimeout, so `online`
// can never report a silently-dead channel as live for longer than that window.
func (h *substrateHub) liveness(org string) (online bool, lastSeen time.Time) {
	h.mu.Lock()
	conn := h.conns[org]
	h.mu.Unlock()
	if conn == nil {
		return false, time.Time{}
	}
	if ns := conn.lastSeen.Load(); ns > 0 {
		lastSeen = time.Unix(0, ns).UTC()
	}
	return true, lastSeen
}

// push enqueues a Down frame for org's substrate. It returns false when the org
// has no live channel (offline ⇒ the caller leaves the record pending;
// reconcile-on-connect delivers it) OR when the connection's buffer is full (a
// slow/stuck substrate ⇒ drop the connection so it reconnects and reconciles
// from scratch — the durable backstop). It never blocks the spawn path.
func (h *substrateHub) push(org string, d channel.Down) bool {
	h.mu.Lock()
	conn := h.conns[org]
	h.mu.Unlock()
	if conn == nil {
		return false
	}
	select {
	case conn.send <- d:
		return true
	default:
		log.Printf("substrate channel: org %s send buffer full — dropping connection (reconcile-on-reconnect recovers)", org)
		conn.cancel()
		return false
	}
}

// ── WebSocket endpoint ──────────────────────────────────────────────────────

// channelHandler is GET /v1/substrate/channel: the substrate-agent's persistent,
// client-initiated dispatch channel. It is authenticated by the caller's
// org-scoped substrate-agent SVID (the Phase-1 verifier via svidAuth: trust-
// domain + org + require SubstrateAgent identity), and the connection is bound
// SERVER-SIDE to that org for its whole lifetime — orgId is NEVER read from a
// frame. The substrate record is the authoritative gate (a disconnected org
// cannot open a channel even with a stale bootstrap SVID).
func (api *substrateAPI) channelHandler(w http.ResponseWriter, r *http.Request) {
	if api.hub == nil || api.svidAuth == nil {
		http.Error(w, "substrate channel is not configured", http.StatusServiceUnavailable)
		return
	}
	// authEntryCaller does the full gate: configured, SVID authenticated to a
	// substrate-agent identity, and the org still has a CONNECTED substrate record.
	// Reusing it keeps the channel and the Entry API on exactly one authorization
	// path (own-org-only, fail-closed on a registry outage).
	org, ok := api.authEntryCaller(w, r)
	if !ok {
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		// Accept already wrote a response.
		log.Printf("substrate channel (org=%s): accept: %v", org, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn := &substrateConn{
		org:      org,
		ws:       c,
		send:     make(chan channel.Down, 256),
		ctx:      ctx,
		cancel:   cancel,
		connated: time.Now().UTC(),
	}
	conn.lastSeen.Store(time.Now().UnixNano()) // "seen" at connect until the first frame
	api.hub.register(conn)
	defer api.hub.unregister(conn)
	defer cancel()

	// Flip the substrate record to active on first connect (idempotent). Liveness
	// (online/offline display) is Phase-5 in-memory state; the record's active
	// latch marks "this org routes to its substrate" and survives a disconnect —
	// disconnect NEVER reverts it (spec AC 8: no record mutation on drop).
	api.activateSubstrate(ctx, org)

	log.Printf("substrate channel (org=%s): connected", org)
	defer log.Printf("substrate channel (org=%s): disconnected", org)

	// Writer: drain the send queue to the socket.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case d := <-conn.send:
				wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
				err := wsjson.Write(wctx, c, d)
				wcancel()
				if err != nil {
					log.Printf("substrate channel (org=%s): write: %v", org, err)
					cancel()
					return
				}
			}
		}
	}()

	// Reader: consume UP frames (status/heartbeat). A read error (client gone,
	// idle timeout) ends the connection. Every frame is validated; a substrate can
	// only ever report about its OWN org (this connection is org-bound), so a
	// status report is advisory telemetry and never mutates another org's record.
	c.SetReadLimit(1 << 20)
	for {
		rctx, rcancel := context.WithTimeout(ctx, api.channelReadTimeout())
		var up channel.Up
		err := wsjson.Read(rctx, c, &up)
		rcancel()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("substrate channel (org=%s): read: %v", org, err)
			}
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
		if verr := up.Validate(); verr != nil {
			log.Printf("substrate channel (org=%s): dropping malformed up frame: %v", org, verr)
			continue
		}
		// Every valid frame (heartbeat or status) refreshes liveness — bounded
		// staleness for the dashboard's online display.
		conn.lastSeen.Store(time.Now().UnixNano())
		switch up.Type {
		case channel.TypeHeartbeat:
			// liveness ping; the successful read already refreshed the deadline.
		case channel.TypeStatus:
			log.Printf("substrate channel (org=%s): status agent=%s phase=%s %s",
				org, up.Status.AgentID, up.Status.Phase, up.Status.Message)
		}
	}
}

// channelReadTimeout bounds how long the server waits for the next UP frame
// (heartbeat or status) before treating the channel as dead. The substrate-agent
// heartbeats well inside this window.
func (api *substrateAPI) channelReadTimeout() time.Duration {
	if api.readTimeout > 0 {
		return api.readTimeout
	}
	return 90 * time.Second
}

// ── substrate-scoped agent list (reconcile-on-connect source) ───────────────

// listAgents is GET /v1/substrate/agents: the substrate-agent's reconcile source.
// Authenticated by the caller's substrate-agent SVID, it returns ONLY that org's
// substrate-placed records — org derived from the SVID, NEVER a query param, so a
// substrate can only ever see its OWN org's desired work. The substrate compares
// this desired set to its local CRs on (re)connect: pending records with no CR ⇒
// create; local CRs whose record is gone/terminal ⇒ tear down.
func (api *substrateAPI) listAgents(w http.ResponseWriter, r *http.Request) {
	callerOrg, ok := api.authEntryCaller(w, r)
	if !ok {
		return
	}
	agents, err := api.reg.ListAgents(r.Context())
	if err != nil {
		log.Printf("substrate list (org=%s): list agents: %v", callerOrg, err)
		http.Error(w, "registry unavailable", http.StatusBadGateway)
		return
	}
	// Server-side org filter (the isolation cut): only substrate-placed records
	// owned by the caller org are ever returned.
	normCaller, nerr := devca.NormalizeOrgID(callerOrg)
	if nerr != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	out := make([]registry.AgentRecord, 0, len(agents))
	for _, a := range agents {
		if a.Placement != registry.PlacementSubstrate {
			continue
		}
		norm, err := devca.NormalizeOrgID(a.OrgID)
		if err != nil || norm != normCaller {
			continue
		}
		out = append(out, a)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// activateSubstrate flips the org's substrate record to `active` on first channel
// connect (POST /v1/orgs/{id}/substrate/activate over the control-plane bearer).
// Idempotent and best-effort: a failure is logged, not fatal — the channel is
// already live, and the next connect retries. Routing (skip-local-CR) keys off
// this persisted `active` latch, so it must survive a disconnect.
func (api *substrateAPI) activateSubstrate(ctx context.Context, org string) {
	normOrg, err := devca.NormalizeOrgID(org)
	if err != nil {
		return
	}
	// Bound this call: it runs synchronously in the handler BEFORE the reader loop
	// starts, so an unbounded request to a hung registry would wedge the channel —
	// the connection is already registered (spawns push to it) but nothing drains
	// the buffer and the death-detecting read never begins. A timeout caps that.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", api.registryURL+"/v1/orgs/"+normOrg+"/substrate/activate", nil)
	if err != nil {
		return
	}
	if t := api.cpBearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("substrate activate (org=%s): %v", normOrg, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		log.Printf("substrate activate (org=%s): registry returned %d", normOrg, resp.StatusCode)
	}
}
