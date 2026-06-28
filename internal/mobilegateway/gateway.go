package mobilegateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

// ctxKey is the private type for gateway request-context values.
type ctxKey int

const ctxUserID ctxKey = iota

// userIDFrom returns the authenticated user id placed in the context by
// requireToken (the access token's sub, minus any "user:" prefix). Empty when
// the request was not token-authenticated, so device/stream scoping denies by
// construction — an empty userId owns no devices and matches no stream.
func userIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxUserID).(string)
	return s
}

// requireToken gates a mobile-facing handler behind a valid delegated access
// token, mirroring the orchestrator's middleware (cmd/orchestrator/main.go):
// a Bearer token the validator accepts, aud containing audience, not a
// delegation-only token, carrying the required scope. The authenticated user is
// stashed in the context for the handler to scope on — the gateway never trusts
// a client-supplied userId (the confused-deputy lesson from the dashboard OAuth
// work). The mobile token uses aud=orchestrator so the same token the gateway
// validates here is the one it forwards to the orchestrator's consent endpoints.
func requireToken(v tokenvalidator.TokenValidator, audience, scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims, err := v.ValidateAccessToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
		if err != nil {
			log.Printf("mobile auth: token validation failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if claims.TokenUse == "delegation" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !claims.HasAudience(audience) {
			log.Printf("mobile auth: aud %v does not contain %q", claims.Audience, audience)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !claims.HasScope(scope) {
			log.Printf("mobile auth: missing scope %q (have %v)", scope, claims.Scopes)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// The mobile edge is human-only: a token carrying an actor chain (act) is
		// an agent's delegated token, not a person answering a prompt. Reject it
		// explicitly so "human-only" is not merely implicit in the scope config.
		if len(claims.Chain) > 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		userID := strings.TrimPrefix(claims.User, "user:")
		if userID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUserID, userID)))
	}
}

// Deps are the gateway's wired dependencies.
type Deps struct {
	Validator       tokenvalidator.TokenValidator
	Devices         DeviceStore
	OrchestratorURL string
	Audience        string
	ReadScope       string
	WriteScope      string
	// Client is the HTTP client used for the orchestrator hop. Always set a
	// timeout; a hung orchestrator socket must not pin a mobile request open.
	Client *http.Client
	// ControlPlane authenticates the registry's /internal/notify webhook call.
	ControlPlane controlplane.Authenticator
	// Hub fans consent events out to connected /me/stream SSE subscribers.
	Hub *Hub
	// Transport is the background-push mechanism (dev no-op | fcmapns).
	Transport Transport
	// StreamTTL caps an SSE connection so the app reconnects with a fresh token
	// (the revocation/expiry teardown). Defaulted in BuildMux when zero.
	StreamTTL time.Duration
}

// BuildMux wires the gateway's routes. Every /me/* route is token-gated; the
// consent routes forward the user's own bearer token to the orchestrator, which
// re-validates it and enforces per-user ownership (the gateway adds no new
// authority). The device routes are served locally against the DeviceStore.
func BuildMux(d Deps) *http.ServeMux {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if d.Hub == nil {
		d.Hub = NewHub()
	}
	if d.Transport == nil {
		d.Transport = NoopTransport{}
	}
	if d.ControlPlane == nil {
		d.ControlPlane = controlplane.AllowAll()
	}
	if d.StreamTTL == 0 {
		d.StreamTTL = 5 * time.Minute
	}
	mux := http.NewServeMux()

	// --- Consent proxy: forward to the orchestrator's user-scoped endpoints ---
	// The orchestrator derives userId from the forwarded token and scopes the
	// registry call, so the gateway only needs to relay method, path, query,
	// body, and the Authorization header.
	mux.HandleFunc("GET /me/consent-requests", requireToken(d.Validator, d.Audience, d.ReadScope,
		d.proxy("GET", func(*http.Request) string { return "/v1/consent-requests" })))
	mux.HandleFunc("POST /me/consent-requests/{id}/approve", requireToken(d.Validator, d.Audience, d.WriteScope,
		d.proxy("POST", func(r *http.Request) string { return "/v1/consent-requests/" + r.PathValue("id") + "/approve" })))
	mux.HandleFunc("POST /me/consent-requests/{id}/deny", requireToken(d.Validator, d.Audience, d.WriteScope,
		d.proxy("POST", func(r *http.Request) string { return "/v1/consent-requests/" + r.PathValue("id") + "/deny" })))

	// Standing consents management (list / revoke).
	mux.HandleFunc("GET /me/consents", requireToken(d.Validator, d.Audience, d.ReadScope,
		d.proxy("GET", func(*http.Request) string { return "/v1/consents" })))
	mux.HandleFunc("POST /me/consents/{id}/revoke", requireToken(d.Validator, d.Audience, d.WriteScope,
		d.proxy("POST", func(r *http.Request) string { return "/v1/consents/" + r.PathValue("id") + "/revoke" })))

	// Single pending request, for the detail screen. The orchestrator exposes no
	// by-id route, so we fetch the user-scoped pending list and select — which
	// keeps ownership enforced (the list is already scoped to the token's user)
	// and means the app shows authoritative server state, never push payload.
	mux.HandleFunc("GET /me/consent-requests/{id}", requireToken(d.Validator, d.Audience, d.ReadScope, d.getConsentRequestByID))

	// --- Device registry (served locally) ------------------------------------
	mux.HandleFunc("GET /me/devices", requireToken(d.Validator, d.Audience, d.ReadScope, d.listDevices))
	mux.HandleFunc("POST /me/devices", requireToken(d.Validator, d.Audience, d.WriteScope, d.registerDevice))
	mux.HandleFunc("DELETE /me/devices/{id}", requireToken(d.Validator, d.Audience, d.WriteScope, d.deleteDevice))

	// --- Per-user event stream (SSE) -----------------------------------------
	mux.HandleFunc("GET /me/stream", requireToken(d.Validator, d.Audience, d.ReadScope, d.stream))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// BuildInternalMux wires the control-plane surface: the registry's
// NOTIFIER_WEBHOOK_URL target. It is served on a SEPARATE port from the
// user-facing BuildMux so a NetworkPolicy can lock /internal/notify down to the
// registry alone — the public surface (reached via the ALB / port-forward) stays
// open and token-gated, while the spoofable webhook is reachable only by the one
// legitimate caller, regardless of the control-plane auth tier.
func BuildInternalMux(d Deps) *http.ServeMux {
	if d.Hub == nil {
		d.Hub = NewHub()
	}
	if d.Transport == nil {
		d.Transport = NoopTransport{}
	}
	if d.ControlPlane == nil {
		d.ControlPlane = controlplane.AllowAll()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/notify", d.notify)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// proxy relays a request to the orchestrator, passing the caller's bearer token,
// query string, and body through unchanged, and copying back the orchestrator's
// status, Content-Type, and body. The orchestrator is the authorization
// authority for consent; this is a transparent relay.
func (d Deps) proxy(method string, pathFn func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		url := d.OrchestratorURL + pathFn(r)
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}
		// GET relays (the consent list) carry no body — pass nil so the upstream
		// GET doesn't get a spurious Content-Length/transfer-encoding.
		var body io.Reader
		if method != http.MethodGet {
			body = r.Body
		}
		req2, err := http.NewRequestWithContext(r.Context(), method, url, body)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Forward the user's own access token — the orchestrator scopes on it.
		req2.Header.Set("Authorization", r.Header.Get("Authorization"))
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req2.Header.Set("Content-Type", ct)
		}
		resp, err := d.Client.Do(req2)
		if err != nil {
			http.Error(w, "orchestrator unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

// consentRequest mirrors the fields the gateway needs from the orchestrator's
// consent-request JSON. It is intentionally partial — the gateway re-serializes
// the matched element verbatim, so unknown fields survive the round trip.
type consentRequest struct {
	ID string `json:"id"`
}

// getConsentRequestByID returns one pending consent request the authenticated
// user owns, by fetching the user-scoped pending list from the orchestrator and
// selecting the id. 404 when the id is not in the user's pending set (it never
// belonged to them, or it was already resolved).
func (d Deps) getConsentRequestByID(w http.ResponseWriter, r *http.Request) {
	// Make per-user isolation explicit here rather than load-bearing on the
	// orchestrator re-deriving userId: never fetch the pending list without an
	// authenticated user (requireToken guarantees this, but a future re-wire of
	// OrchestratorURL must not be able to widen this to every user's requests).
	if userIDFrom(r.Context()) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	url := d.OrchestratorURL + "/v1/consent-requests?status=pending"
	req2, err := http.NewRequestWithContext(r.Context(), "GET", url, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req2.Header.Set("Authorization", r.Header.Get("Authorization"))
	resp, err := d.Client.Do(req2)
	if err != nil {
		http.Error(w, "orchestrator unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Relay the orchestrator's verdict (e.g. 401/403) rather than masking it.
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	var list []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		http.Error(w, "bad upstream response", http.StatusBadGateway)
		return
	}
	for _, raw := range list {
		var cr consentRequest
		if json.Unmarshal(raw, &cr) == nil && cr.ID == id {
			w.Header().Set("Content-Type", "application/json")
			w.Write(raw)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (d Deps) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := d.Devices.ListByUser(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

func (d Deps) registerDevice(w http.ResponseWriter, r *http.Request) {
	// Cap the body — this is an internet-facing edge for untrusted mobile
	// clients; a device registration is a few hundred bytes.
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Platform  string `json:"platform"`
		PushToken string `json:"pushToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !ValidPlatform(body.Platform) {
		http.Error(w, "platform must be ios or android", http.StatusBadRequest)
		return
	}
	if body.PushToken == "" {
		http.Error(w, "pushToken is required", http.StatusBadRequest)
		return
	}
	// userId comes from the validated token, never the request body — a forged
	// userId in the payload has nowhere to land.
	dev := Device{
		ID:        newID(),
		UserID:    userIDFrom(r.Context()),
		Platform:  body.Platform,
		PushToken: body.PushToken,
		CreatedAt: time.Now().UTC(),
	}
	if err := d.Devices.Put(r.Context(), dev); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, dev)
}

func (d Deps) deleteDevice(w http.ResponseWriter, r *http.Request) {
	ok, err := d.Devices.Delete(r.Context(), userIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// newID returns a random 128-bit hex id for a device registration. A failed
// read (essentially impossible on Linux) must not yield an all-zero, colliding
// id, so it panics rather than mint a degenerate identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("mobilegateway: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
