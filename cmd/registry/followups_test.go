// cmd/registry/followups_test.go
//
// Handler-level tests for the aws-persistence follow-ups that live in the
// registry's neutral HTTP layer (so they apply to BOTH the in-memory and the
// DynamoDB store):
//   - the read-time consent-sweep backstop (item 1),
//   - transient store-read errors surfacing as 5xx (item 3),
//   - the userId charset guard at ingest (item 5),
//   - authentication on POST /v1/agents/{id}/events (item 6).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

// readFailStore wraps a registry.Store and makes GetAgent return a transient
// error from the failFrom-th call onward (0 = fail immediately, -1 = never),
// standing in for a durable backend hiccup so we can assert the handlers fail
// loud (5xx) rather than swallowing the error.
type readFailStore struct {
	registry.Store
	mu       sync.Mutex
	calls    int
	failFrom int
}

func (f *readFailStore) GetAgent(ctx context.Context, id string) (registry.AgentRecord, error) {
	f.mu.Lock()
	n := f.calls
	f.calls++
	f.mu.Unlock()
	if f.failFrom >= 0 && n >= f.failFrom {
		return registry.AgentRecord{}, errors.New("simulated transient read error")
	}
	return f.Store.GetAgent(ctx, id)
}

func allowAllMux(s registry.Store) http.Handler {
	return buildMux(s, spicedb.NewMock(),
		registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll(), testDir())
}

// --- Item 1: read-time consent-sweep backstop -------------------------------

func TestConsentRequestReadBackstop(t *testing.T) {
	s := newStore()
	ctx := context.Background()

	// A covering, unexpired grant already exists for the edge...
	if _, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		UserID: "u1", ParentType: "p", ChildType: "c", Scopes: []string{"x"}, GrantedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	// ...but a same-edge request is still PENDING, as if the approve-sweep's
	// eventually-consistent index had missed it (created directly at the store,
	// bypassing the handler's short-circuit).
	cr, _, err := s.CreateConsentRequest(ctx, registry.ConsentRequest{
		UserID: "u1", ParentType: "p", ChildType: "c", AgentID: "ag1", Scopes: []string{"x"},
	})
	if err != nil {
		t.Fatalf("seed pending request: %v", err)
	}
	if cr.Status != registry.ConsentPending {
		t.Fatalf("seed status = %q, want pending", cr.Status)
	}

	mux := allowAllMux(s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/consent-requests/"+cr.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var got registry.ConsentRequest
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The read resolved the missed request against the covering grant.
	if got.Status != registry.ConsentApproved {
		t.Errorf("backstop response status = %q, want approved", got.Status)
	}
	// ...and the transition is PERSISTED, not just reflected in the response.
	if persisted, _, _ := s.GetConsentRequest(ctx, cr.ID); persisted.Status != registry.ConsentApproved {
		t.Errorf("persisted status = %q, want approved", persisted.Status)
	}
}

func TestConsentRequestReadBackstop_NoGrantStaysPending(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	// No covering grant → the request must remain pending after a read.
	cr, _, err := s.CreateConsentRequest(ctx, registry.ConsentRequest{
		UserID: "u2", ParentType: "p", ChildType: "c", AgentID: "ag2", Scopes: []string{"x"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := allowAllMux(s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/consent-requests/"+cr.ID, nil))
	var got registry.ConsentRequest
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.Status != registry.ConsentPending {
		t.Errorf("status = %q, want still pending (no covering grant)", got.Status)
	}
}

// --- Item 3: transient read error → 5xx -------------------------------------

func TestReadErrorSurfacesAsServerError(t *testing.T) {
	// Revoke: agentOwnedBy's GetAgent errors on the first call → 500, not 404.
	t.Run("revoke", func(t *testing.T) {
		base := newStore()
		base.registerAgent(registry.AgentRecord{AgentID: "a1", UserID: "u1", Status: "active"})
		s := &readFailStore{Store: base, failFrom: 0}
		mux := allowAllMux(s)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/a1/revoke?userId=u1", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("revoke on read error = %d, want 500", rec.Code)
		}
	})

	// Spawn-policy depth walk: the handler's own GetAgent succeeds, then depth's
	// GetAgent errors → 500 rather than an undercounted (fail-open) depth.
	t.Run("spawn-policy depth", func(t *testing.T) {
		base := newStore()
		base.registerAgent(registry.AgentRecord{AgentID: "parent", AgentType: "worker", Status: "active"})
		base.putTemplate(registry.AgentTemplate{
			AgentType: "worker",
			Delegation: registry.DelegationPolicy{
				AllowedChildTypes: []string{"child"},
				MaxDepth:          2,
			},
		})
		s := &readFailStore{Store: base, failFrom: 1} // handler rec ok, depth walk fails
		mux := allowAllMux(s)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/spawn-policy?parentId=parent&childType=child", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("spawn-policy on depth read error = %d, want 500", rec.Code)
		}
	})
}

// --- Item 5: userId charset guard -------------------------------------------

func TestUserIDCharsetGuard(t *testing.T) {
	mux := allowAllMux(newStore())

	// Query-param path (middleware): a separator-bearing userId is rejected.
	for _, bad := range []string{"a|b", "a#b"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/consents?userId="+bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET consents userId=%q = %d, want 400", bad, rec.Code)
		}
	}

	// A real Keycloak UUID subject and an empty (unscoped) userId both pass.
	for _, ok := range []string{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", ""} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/consents?userId="+ok, nil))
		if rec.Code == http.StatusBadRequest {
			t.Errorf("GET consents userId=%q = 400, want accepted", ok)
		}
	}

	// Body path: POST /v1/consents with a corrupt userId is rejected before storage.
	body, _ := json.Marshal(map[string]any{"userId": "a|b", "parentType": "p", "childType": "c"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST consents bad userId = %d, want 400", rec.Code)
	}
}

// --- Item 6: authenticated events endpoint ----------------------------------

func TestEventsEndpointAuth(t *testing.T) {
	seed := func() *store {
		s := newStore()
		// child's record names its parent, so the child→parent post is allowed.
		s.registerAgent(registry.AgentRecord{AgentID: "child", UserID: "u1", Status: "active", ParentID: "parent"})
		s.registerAgent(registry.AgentRecord{AgentID: "parent", UserID: "u1", Status: "active"})
		return s
	}
	verifier := registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{SpiffeID: "spiffe://example.org/child"})
	body := func() *bytes.Reader {
		b, _ := json.Marshal(map[string]string{"source": "agent", "type": "unit-test"})
		return bytes.NewReader(b)
	}

	post := func(mux http.Handler, target, authz string) int {
		req := httptest.NewRequest("POST", target, body())
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("gated tier (shared-secret)", func(t *testing.T) {
		mux := buildMux(seed(), spicedb.NewMock(), verifier, controlplane.NewSharedSecret("sekrit"), testDir())

		if c := post(mux, "/v1/agents/child/events", ""); c != http.StatusUnauthorized {
			t.Errorf("no credential = %d, want 401", c)
		}
		if c := post(mux, "/v1/agents/child/events", "Bearer sekrit"); c != http.StatusCreated {
			t.Errorf("control-plane bearer = %d, want 201", c)
		}
		// SVID identifies "child": may post to itself and to its direct parent.
		if c := post(mux, "/v1/agents/child/events", "Bearer any-svid"); c != http.StatusCreated {
			t.Errorf("SVID→self = %d, want 201", c)
		}
		if c := post(mux, "/v1/agents/parent/events", "Bearer any-svid"); c != http.StatusCreated {
			t.Errorf("SVID→parent = %d, want 201", c)
		}
		// ...but not to an unrelated agent.
		if c := post(mux, "/v1/agents/stranger/events", "Bearer any-svid"); c != http.StatusUnauthorized {
			t.Errorf("SVID→unrelated = %d, want 401", c)
		}
	})

	t.Run("open tier (kind, auth=none) accepts unauthenticated posts", func(t *testing.T) {
		mux := allowAllMux(seed())
		if c := post(mux, "/v1/agents/child/events", ""); c != http.StatusCreated {
			t.Errorf("AllowAll unauthenticated = %d, want 201 (kind unchanged)", c)
		}
	})
}

// --- B1: the events-forge holes are closed ----------------------------------

// Preregister sets a record's ParentID, which the events gate trusts — so it must
// require control-plane auth on a gated tier (its only caller, the orchestrator,
// holds the bearer). Under AllowAll it stays open (kind).
func TestPreregisterRequiresControlPlaneAuth(t *testing.T) {
	verifier := registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{})
	body, _ := json.Marshal(registry.AgentRecord{AgentID: "ag", AgentType: "worker", UserID: "u1"})

	mux := buildMux(newStore(), spicedb.NewMock(), verifier, controlplane.NewSharedSecret("sekrit"), testDir())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/preregister", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated preregister = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/agents/preregister", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sekrit")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("control-plane preregister = %d, want 201", rec.Code)
	}
}

// A self-registering agent (SVID pins only its own id) must NOT be able to point
// its ParentID at a victim and then forge audit events onto that victim.
func TestSelfRegisterCannotForgeParent(t *testing.T) {
	s := newStore()
	s.putTemplate(registry.AgentTemplate{AgentType: "worker", Version: "1.0.0", Status: registry.TemplateStatusActive})
	// The agent's record already exists (as a preregister/first-register would
	// leave it) with NO parent.
	s.registerAgent(registry.AgentRecord{AgentID: "attacker", AgentType: "worker", UserID: "u1", Status: "pending"})

	verifier := registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{SpiffeID: "spiffe://example.org/attacker"})
	mux := buildMux(s, spicedb.NewMock(), verifier, controlplane.NewSharedSecret("sekrit"), testDir())

	// Self-register (SVID) trying to smuggle parentId=victim in the body.
	regBody, _ := json.Marshal(map[string]string{"agentType": "worker", "parentId": "victim"})
	req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(regBody))
	req.Header.Set("Authorization", "Bearer attacker-svid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("self-register = %d, want 201", rec.Code)
	}
	// The smuggled ParentID was ignored — the record's lineage is unchanged.
	if got, _ := s.GetAgent(context.Background(), "attacker"); got.ParentID != "" {
		t.Fatalf("ParentID = %q, want empty (immutable, smuggle ignored)", got.ParentID)
	}
	// ...so the forge post to the victim is refused by the events gate.
	eventBody, _ := json.Marshal(map[string]string{"source": "agent", "type": "forged"})
	fr := httptest.NewRequest("POST", "/v1/agents/victim/events", bytes.NewReader(eventBody))
	fr.Header.Set("Authorization", "Bearer attacker-svid")
	frec := httptest.NewRecorder()
	mux.ServeHTTP(frec, fr)
	if frec.Code != http.StatusUnauthorized {
		t.Errorf("forged event post to victim = %d, want 401", frec.Code)
	}
}
