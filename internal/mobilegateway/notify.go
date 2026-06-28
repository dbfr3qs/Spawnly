package mobilegateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Event is the minimal, secret-free notification the gateway delivers when a
// consent prompt needs a human. It carries only what the app needs to deep-link
// and label the prompt; the app re-fetches the authoritative request (scopes,
// binding message) over the authenticated channel. Scopes and binding message
// are deliberately NOT here — they must not ride a push payload.
type Event struct {
	Type             string    `json:"type"`
	ConsentRequestID string    `json:"consentRequestId"`
	AgentID          string    `json:"agentId,omitempty"`
	ParentType       string    `json:"parentType"`
	ChildType        string    `json:"childType"`
	At               time.Time `json:"at"`
}

// Transport is the pluggable background-push mechanism, selected by NOTIFIER and
// modelled on the platform's ATTESTOR selector. The dev transport is a no-op
// (the per-user SSE stream is the local delivery); the fcmapns transport (Phase
// 3) sends to APNs/FCM. Send is best-effort and per-device; the notify handler
// logs failures and moves on.
type Transport interface {
	Name() string
	Send(ctx context.Context, d Device, e Event) error
}

// NoopTransport is the dev transport: it sends no external push, leaving the
// authenticated SSE stream as the only delivery. This is what makes the local
// `make bootstrap` path work with zero Apple/Google credentials.
type NoopTransport struct{}

func (NoopTransport) Name() string                              { return "dev" }
func (NoopTransport) Send(context.Context, Device, Event) error { return nil }

// maxStreamsPerUser caps concurrent SSE connections per user. A valid token
// could otherwise open unbounded streams (each a goroutine + buffered channel
// for up to StreamTTL) — a self-inflicted resource drain. The cap bounds it
// without affecting the legitimate few-devices case.
const maxStreamsPerUser = 8

// Hub is a per-user fan-out of Events to connected SSE streams. A user may have
// several streams open (multiple devices / app instances); each gets every
// event. Subscriptions are keyed by userId so an event for one user can never
// reach another's stream.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan Event]struct{}{}}
}

// Subscribe registers a stream for userID and returns its channel, an
// unsubscribe func the caller must defer, and ok=false when the user is already
// at maxStreamsPerUser. The channel is buffered so a slow reader briefly lagging
// doesn't block Publish; an over-full channel drops the event (the app
// reconciles on its next list fetch / reconnect).
func (h *Hub) Subscribe(userID string) (<-chan Event, func(), bool) {
	ch := make(chan Event, 8)
	h.mu.Lock()
	// len on a nil map is 0, so the cap check is safe before the map exists —
	// and creating the map only after the check avoids orphaning an empty one.
	if len(h.subs[userID]) >= maxStreamsPerUser {
		h.mu.Unlock()
		return nil, func() {}, false
	}
	if h.subs[userID] == nil {
		h.subs[userID] = map[chan Event]struct{}{}
	}
	h.subs[userID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if set := h.subs[userID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, userID)
			}
		}
		h.mu.Unlock()
	}, true
}

// Publish delivers e to every stream subscribed for userID. Non-blocking: a full
// subscriber channel drops the event rather than stalling the fan-out.
func (h *Hub) Publish(userID string, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[userID] {
		select {
		case ch <- e:
		default:
		}
	}
}

// notify is the registry's NOTIFIER_WEBHOOK_URL target. It is a control-plane
// endpoint — authenticated by the shared control-plane authenticator, NOT a
// user token — because the caller is the registry (a platform service), and an
// unauthenticated caller could otherwise spam pushes at arbitrary users.
func (d Deps) notify(w http.ResponseWriter, r *http.Request) {
	if _, err := d.ControlPlane.Authenticate(r.Context(), r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var p struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		AgentID    string `json:"agentId"`
		User       string `json:"user"`
		ParentType string `json:"parentType"`
		ChildType  string `json:"childType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.User == "" || p.ID == "" {
		http.Error(w, "user and id are required", http.StatusBadRequest)
		return
	}
	// Defense-in-depth: bound the string fields. They key a map / device lookup
	// and (once a real push transport is configured) ride a notification, so a
	// pathological length has no legitimate use.
	for _, s := range []string{p.User, p.ID, p.AgentID, p.ParentType, p.ChildType} {
		if len(s) > 256 {
			http.Error(w, "field too long", http.StatusBadRequest)
			return
		}
	}
	eventType := p.Type
	if eventType == "" {
		eventType = "consent_pending"
	}
	e := Event{
		Type:             eventType,
		ConsentRequestID: p.ID,
		AgentID:          p.AgentID,
		ParentType:       p.ParentType,
		ChildType:        p.ChildType,
		At:               time.Now().UTC(),
	}
	// SSE stream first — the always-on, foreground delivery (the only delivery
	// under NOTIFIER=dev).
	d.Hub.Publish(p.User, e)
	// Background push to each registered device (no-op under the dev transport).
	devices, err := d.Devices.ListByUser(r.Context(), p.User)
	if err == nil {
		for _, dev := range devices {
			serr := d.Transport.Send(r.Context(), dev, e)
			if serr == nil {
				continue
			}
			// Prune tokens the provider says are dead so they don't accumulate;
			// other failures are transient and just logged (best-effort fan-out).
			if errors.Is(serr, ErrDeviceUnregistered) {
				d.Devices.Delete(r.Context(), p.User, dev.ID)
			}
			log.Printf("push send to device %s (%s) failed: %v", dev.ID, d.Transport.Name(), serr)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// stream is the per-user SSE endpoint (GET /me/stream). It is token-gated in
// BuildMux, so the connecting user is authenticated here. The stream is capped
// at StreamTTL: when it elapses the server closes it and the app reconnects with
// a fresh token — which is how a revoked/expired user stops receiving events
// (we validate at connect, and a revoked token fails the reconnect).
func (d Deps) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	userID := userIDFrom(r.Context())
	ch, unsubscribe, ok := d.Hub.Subscribe(userID)
	if !ok {
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		return
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Tell the client the stream is live before the first event.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	deadline := time.NewTimer(d.StreamTTL)
	defer deadline.Stop()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			// Force a reconnect so the token is re-validated (revocation teardown).
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e := <-ch:
			payload, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, payload)
			flusher.Flush()
		}
	}
}
