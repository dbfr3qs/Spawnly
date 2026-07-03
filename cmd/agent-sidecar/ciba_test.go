// cmd/agent-sidecar/ciba_test.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/attestor"
)

// fakeIS is a minimal Keycloak double for the two CIBA endpoints (the realm's
// OIDC token endpoint and the ext/ciba/auth backchannel endpoint). Its verdict
// field scripts what the token endpoint answers for pending requests.
type fakeIS struct {
	mu        sync.Mutex
	verdict   string // "pending" | "granted" | "denied" | "expired"
	requests  int    // backchannel requests seen
	polls     int    // token polls seen
	expiresIn int
}

func (f *fakeIS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/spawnly/protocol/openid-connect/ext/ciba/auth", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		expires := f.expiresIn
		if expires == 0 {
			expires = 300
		}
		json.NewEncoder(w).Encode(map[string]any{
			"auth_req_id": fmt.Sprintf("req-%d", f.requests),
			"expires_in":  expires,
			"interval":    1,
		})
	})
	mux.HandleFunc("POST /realms/spawnly/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls++
		switch f.verdict {
		case "granted":
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "user-bound-token", "expires_in": 120,
			})
		case "denied":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
		case "expired":
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "expired_token"})
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		}
	})
	return mux
}

func testSource(t *testing.T, f *fakeIS) *cibaTokenSource {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cs := newCibaTokenSource(config{
		agentType:     "currency-converter",
		userID:        "alice",
		isTokenURL:    srv.URL + "/realms/spawnly/protocol/openid-connect/token",
		consentScopes: "openid sample-api-b:read",
	})
	cs.source = &attestor.MockSource{Cred: attestor.Credential{
		Value:         "fake-svid",
		AssertionType: attestor.JWTBearerAssertionType,
	}}
	return cs
}

func TestWaitForGrant_AutoApprovedOnFirstPoll(t *testing.T) {
	f := &fakeIS{verdict: "granted"}
	cs := testSource(t, f)
	if err := cs.waitForGrant(context.Background()); err != nil {
		t.Fatalf("waitForGrant: %v", err)
	}
	tok, expiresIn, err := cs.get(context.Background())
	if err != nil || tok != "user-bound-token" || expiresIn <= 0 {
		t.Fatalf("get after grant: tok=%q expiresIn=%d err=%v", tok, expiresIn, err)
	}
	if f.polls != 1 {
		t.Fatalf("auto-approve should resolve on the first poll, got %d", f.polls)
	}
}

func TestWaitForGrant_PendingThenApproved(t *testing.T) {
	f := &fakeIS{verdict: "pending"}
	cs := testSource(t, f)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		f.mu.Lock()
		f.verdict = "granted"
		f.mu.Unlock()
	}()
	if err := cs.waitForGrant(context.Background()); err != nil {
		t.Fatalf("waitForGrant: %v", err)
	}
	if f.polls < 2 {
		t.Fatalf("expected at least one pending poll before grant, got %d", f.polls)
	}
}

func TestWaitForGrant_Denied(t *testing.T) {
	f := &fakeIS{verdict: "denied"}
	cs := testSource(t, f)
	if err := cs.waitForGrant(context.Background()); !errors.Is(err, errConsentDenied) {
		t.Fatalf("want errConsentDenied, got %v", err)
	}
	if _, _, err := cs.get(context.Background()); !errors.Is(err, errConsentDenied) {
		t.Fatalf("get after denial should stay denied, got %v", err)
	}
}

func TestWaitForGrant_Expired(t *testing.T) {
	f := &fakeIS{verdict: "expired"}
	cs := testSource(t, f)
	if err := cs.waitForGrant(context.Background()); !errors.Is(err, errConsentExpired) {
		t.Fatalf("want errConsentExpired, got %v", err)
	}
}

// A renewal after token expiry opens a fresh backchannel request; while the
// stored consent stands (fake grants immediately) the agent never notices.
func TestGet_RenewsViaFreshBackchannelRequest(t *testing.T) {
	f := &fakeIS{verdict: "granted"}
	cs := testSource(t, f)
	if err := cs.waitForGrant(context.Background()); err != nil {
		t.Fatalf("waitForGrant: %v", err)
	}
	cs.expiry = time.Now() // force renewal
	tok, _, err := cs.get(context.Background())
	if err != nil || tok != "user-bound-token" {
		t.Fatalf("renewal: tok=%q err=%v", tok, err)
	}
	if f.requests != 2 {
		t.Fatalf("renewal should open a second backchannel request, got %d", f.requests)
	}
}

// A revoked consent leaves the renewal pending: get surfaces errConsentPending
// (503 to the agent) and keeps polling the same outstanding request.
func TestGet_RevokedConsentLeavesRenewalPending(t *testing.T) {
	f := &fakeIS{verdict: "granted"}
	cs := testSource(t, f)
	if err := cs.waitForGrant(context.Background()); err != nil {
		t.Fatalf("waitForGrant: %v", err)
	}
	f.mu.Lock()
	f.verdict = "pending" // consent revoked server-side: renewals stop auto-approving
	f.mu.Unlock()
	cs.expiry = time.Now()
	if _, _, err := cs.get(context.Background()); !errors.Is(err, errConsentPending) {
		t.Fatalf("want errConsentPending, got %v", err)
	}
	requestsAfterFirst := f.requests
	cs.nextPoll = time.Now() // skip the poll interval
	if _, _, err := cs.get(context.Background()); !errors.Is(err, errConsentPending) {
		t.Fatalf("want errConsentPending on re-poll, got %v", err)
	}
	if f.requests != requestsAfterFirst {
		t.Fatal("re-poll must reuse the outstanding request, not open a new one")
	}
	// User re-approves on the dashboard.
	f.mu.Lock()
	f.verdict = "granted"
	f.mu.Unlock()
	cs.nextPoll = time.Now()
	if tok, _, err := cs.get(context.Background()); err != nil || tok != "user-bound-token" {
		t.Fatalf("after re-approval: tok=%q err=%v", tok, err)
	}
}

// TestCibaAuthURL verifies the CIBA backchannel endpoint is derived as
// Keycloak's .../ext/ciba/auth from the realm token endpoint.
func TestCibaAuthURL(t *testing.T) {
	got := cibaAuthURL("http://keycloak:8080/realms/spawnly/protocol/openid-connect/token")
	want := "http://keycloak:8080/realms/spawnly/protocol/openid-connect/ext/ciba/auth"
	if got != want {
		t.Fatalf("cibaAuthURL = %q, want %q", got, want)
	}
}

// TestBindingMessage verifies the binding_message is space-free and ≤50 chars,
// which Keycloak requires (it rejects spaces and caps the length).
func TestBindingMessage(t *testing.T) {
	cs := &cibaTokenSource{cfg: config{agentType: "flight-search"}}
	msg := cs.bindingMessage()
	if len(msg) > 50 {
		t.Fatalf("binding_message too long: %d chars (%q)", len(msg), msg)
	}
	// Keycloak's default CIBA validation allows ONLY basic alphanumerics — no
	// spaces AND no punctuation (so the ':' and '-' an earlier attempt used are
	// both rejected). "flight-search" must yield "spawnflightsearch".
	for _, r := range msg {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			t.Fatalf("binding_message contains a non-alphanumeric character %q: %q", r, msg)
		}
	}
	if msg != "spawnflightsearch" {
		t.Fatalf("binding_message = %q, want %q", msg, "spawnflightsearch")
	}

	// A pathologically long agentType is truncated to 50 chars.
	long := &cibaTokenSource{cfg: config{agentType: "a-very-long-agent-type-name-that-blows-past-the-fifty-char-limit"}}
	if got := long.bindingMessage(); len(got) != 50 {
		t.Fatalf("long agentType binding_message not truncated to 50: %d (%q)", len(got), got)
	}
}

func TestCovered_ScopeSubset(t *testing.T) {
	cs := &cibaTokenSource{cfg: config{consentScopes: "openid sample-api-b:read"}}
	for req, want := range map[string]bool{
		"":                                 true,
		"sample-api-b:read":                true,
		"openid sample-api-b:read":         true,
		"sample-api-a:write":               false,
		"sample-api-b:read sample-api-a:x": false,
	} {
		if got := cs.covered(req); got != want {
			t.Errorf("covered(%q) = %v, want %v", req, got, want)
		}
	}
}

// TestUpdateStatus_AttachesBearer verifies updateStatus fetches a fresh registry
// credential and presents it as `Authorization: Bearer <svid>` — the SVID-self
// auth the registry's PATCH /v1/agents/{id} now requires. A startup token could
// be stale by the time the post-consent active/failed PATCH fires, so the
// credential is fetched per call.
func TestUpdateStatus_AttachesBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config{
		registryURL: srv.URL,
		source:      &attestor.MockSource{Cred: attestor.Credential{Value: "fresh-svid"}},
	}
	if err := updateStatus(context.Background(), cfg, "agent-x", "active"); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}
	if gotAuth != "Bearer fresh-svid" {
		t.Fatalf("Authorization: got %q, want %q", gotAuth, "Bearer fresh-svid")
	}
}
