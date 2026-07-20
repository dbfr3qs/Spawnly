package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	fakeclient "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// ── reaper unit: kill routing per placement (B1 push + B2 terminal marking) ────

// fakePusher records kills per org and reports per-org connectivity (so a test
// can model an OFFLINE substrate — push returns false, but the record is still
// dismissed, proving reconcile is the backstop).
type fakePusher struct {
	kills  map[string][]string
	online map[string]bool
}

func newFakePusher(onlineOrgs ...string) *fakePusher {
	p := &fakePusher{kills: map[string][]string{}, online: map[string]bool{}}
	for _, o := range onlineOrgs {
		p.online[o] = true
	}
	return p
}

func (p *fakePusher) push(org string, d channel.Down) bool {
	if d.Type == channel.TypeKill && d.Kill != nil {
		p.kills[org] = append(p.kills[org], d.Kill.AgentID)
	}
	return p.online[org]
}

func subRec(id, org string) registry.AgentRecord {
	return registry.AgentRecord{AgentID: id, OrgID: org, Placement: registry.PlacementSubstrate, Status: "active"}
}

// A live substrate: reapSubtree dismisses EVERY subtree node (terminal ⇒ reconcile
// backstop, B2) AND pushes a kill for each down the org's own channel (B1).
func TestReapSubtree_LiveDismissesAndPushes(t *testing.T) {
	reg := registry.NewMock(nil)
	reg.Agents = []registry.AgentRecord{subRec("root", "acme"), subRec("a", "acme"), subRec("b", "acme")}
	reg.Subtrees = map[string][]string{"root": {"root", "a", "b"}}
	pusher := newFakePusher("acme")
	rp := &substrateReaper{reg: reg, hub: pusher}

	dismissed, failed := rp.reapSubtree(context.Background(), "root", "acme", "alice")
	if dismissed != 3 || len(failed) != 0 {
		t.Fatalf("dismissed=%d failed=%v, want 3/none", dismissed, failed)
	}
	sort.Strings(reg.Dismissed)
	if !reflect.DeepEqual(reg.Dismissed, []string{"a", "b", "root"}) {
		t.Fatalf("registry Dismissed=%v, want [a b root] (every node terminal — B2)", reg.Dismissed)
	}
	sort.Strings(pusher.kills["acme"])
	if !reflect.DeepEqual(pusher.kills["acme"], []string{"a", "b", "root"}) {
		t.Fatalf("kills=%v, want [a b root] pushed to acme (B1)", pusher.kills["acme"])
	}
	// Own-org only: nothing pushed to any other org.
	if len(pusher.kills) != 1 {
		t.Fatalf("kills leaked to other orgs: %v", pusher.kills)
	}
}

// An OFFLINE substrate (no live channel): push returns false, but every record is
// STILL marked terminal, so reconcile-on-connect tears it down — no revocation/
// lifecycle bypass just because the channel was down when DELETE ran.
func TestReapSubtree_OfflineStillMarksTerminal(t *testing.T) {
	reg := registry.NewMock(nil)
	reg.Agents = []registry.AgentRecord{subRec("root", "acme")}
	reg.Subtrees = map[string][]string{"root": {"root"}}
	pusher := newFakePusher() // acme NOT online

	rp := &substrateReaper{reg: reg, hub: pusher}
	dismissed, failed := rp.reapSubtree(context.Background(), "root", "acme", "alice")
	if dismissed != 1 || len(failed) != 0 {
		t.Fatalf("dismissed=%d failed=%v, want 1/none even offline", dismissed, failed)
	}
	if !reflect.DeepEqual(reg.Dismissed, []string{"root"}) {
		t.Fatalf("offline substrate: record must still be dismissed (reconcile backstop), got %v", reg.Dismissed)
	}
	// A push was attempted (harmless) but not delivered — that's fine.
	if got := pusher.kills["acme"]; !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("kill push attempt=%v, want [root]", got)
	}
}

// A substrate chain agent can self-spawn children mid-teardown: the settle-loop
// re-reads the subtree and dismisses the newcomers until it converges.
type growingSubtreeReg struct {
	*registry.Mock
	passes [][]string
	call   int
}

func (g *growingSubtreeReg) Subtree(_ context.Context, _, _ string) ([]string, error) {
	i := g.call
	if i >= len(g.passes) {
		i = len(g.passes) - 1
	}
	g.call++
	return g.passes[i], nil
}

func TestReapSubtree_SettleLoopCatchesLateChildren(t *testing.T) {
	base := registry.NewMock(nil)
	reg := &growingSubtreeReg{Mock: base, passes: [][]string{{"root"}, {"root", "late"}}}
	pusher := newFakePusher("acme")
	rp := &substrateReaper{reg: reg, hub: pusher, settle: 0} // no sleep in tests

	dismissed, failed := rp.reapSubtree(context.Background(), "root", "acme", "alice")
	if dismissed != 2 || len(failed) != 0 {
		t.Fatalf("dismissed=%d failed=%v, want 2/none (late child caught)", dismissed, failed)
	}
	sort.Strings(base.Dismissed)
	if !reflect.DeepEqual(base.Dismissed, []string{"late", "root"}) {
		t.Fatalf("Dismissed=%v, want [late root]", base.Dismissed)
	}
}

func TestPushKill_OwnOrgDelivery(t *testing.T) {
	pusher := newFakePusher("acme")
	rp := &substrateReaper{hub: pusher}
	if !rp.pushKill("acme", "x") {
		t.Fatal("push to connected org must report delivered")
	}
	if rp.pushKill("globex", "y") {
		t.Fatal("push to a disconnected org must report not-delivered (offline)")
	}
	if got := pusher.kills["acme"]; !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("acme kills=%v, want [x]", got)
	}
}

func TestIsSubstratePlaced(t *testing.T) {
	if !isSubstratePlaced(registry.AgentRecord{Placement: registry.PlacementSubstrate}) {
		t.Fatal("substrate placement not recognized")
	}
	for _, p := range []string{"", registry.PlacementK8s, registry.PlacementRemote} {
		if isSubstratePlaced(registry.AgentRecord{Placement: p}) {
			t.Fatalf("placement %q wrongly treated as substrate", p)
		}
	}
}

// ── integration: DELETE of a substrate agent marks terminal (B2) via buildMux ──

// substrateLifecycleReg serves the registry surface the DELETE/dismiss handlers
// touch for a SUBSTRATE-placed agent: GET record (owned by alice, org acme,
// placement substrate), subtree, and dismiss capture.
func substrateLifecycleReg(t *testing.T, subtrees map[string][]string, dismissed *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dismiss"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/dismiss")
			if dismissed != nil {
				*dismissed = append(*dismissed, id)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/subtree"):
			if r.URL.Query().Get("userId") == "" {
				http.NotFound(w, r)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/subtree")
			nodes, ok := subtrees[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string][]string{"subtree": nodes})
		case r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") == 3:
			id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(registry.AgentRecord{
				AgentID: id, AgentType: "worker", UserID: "alice", OrgID: "acme",
				Placement: registry.PlacementSubstrate, Status: "active",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func substrateLifecycleMux(t *testing.T, mockReg string) http.Handler {
	t.Helper()
	sdb := spicedb.NewMock()
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	// Realm-admin token: canManage passes via the admin bypass, and manageUserID
	// resolves the forwarded userId to the record's owner (alice).
	return buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg,
		fakeValidator{claims: humanAdminClaims("root", "orchestrator:write")},
		"orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")
}

// DELETE of a substrate agent must NOT be a silent 204 no-op (B2): it marks every
// subtree record terminal (dismiss) so reconcile-on-connect tears them down even
// if the substrate was OFFLINE at delete time.
func TestDeleteSubstrateAgent_MarksSubtreeTerminal(t *testing.T) {
	cascadeSettleDelay = 0
	var dismissed []string
	mockReg := substrateLifecycleReg(t, map[string][]string{"root": {"root", "child"}}, &dismissed)
	defer mockReg.Close()

	mux := substrateLifecycleMux(t, mockReg.URL)
	req := httptest.NewRequest("DELETE", "/v1/agents/root", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	sort.Strings(dismissed)
	if !reflect.DeepEqual(dismissed, []string{"child", "root"}) {
		t.Fatalf("dismissed=%v, want [child root] — a substrate DELETE must mark terminal, not no-op", dismissed)
	}
}

// Dismiss of a substrate agent forwards to the registry (marking it terminal) and
// succeeds — the live kill push is best-effort on top (unit-covered in the reaper).
func TestDismissSubstrateAgent_Forwards(t *testing.T) {
	var dismissed []string
	mockReg := substrateLifecycleReg(t, nil, &dismissed)
	defer mockReg.Close()

	mux := substrateLifecycleMux(t, mockReg.URL)
	req := httptest.NewRequest("POST", "/v1/agents/root/dismiss", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(dismissed, []string{"root"}) {
		t.Fatalf("dismissed=%v, want [root]", dismissed)
	}
}

// Revoke/resume parity: revoke of a substrate agent forwards to the registry's
// revoke (mint-time enforcement) and must NEVER mark the record terminal — the pod
// keeps running (revoke bites at the next mint, resume restores it). A kill push on
// revoke would break resume, so it must not happen; here we prove no dismiss.
func TestRevokeSubstrateAgent_DoesNotMarkTerminal(t *testing.T) {
	var dismissed []string
	var revoked []string
	mockReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/revoke"):
			revoked = append(revoked, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/revoke"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dismiss"):
			dismissed = append(dismissed, "x")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") == 3:
			id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(registry.AgentRecord{AgentID: id, UserID: "alice", OrgID: "acme", Placement: registry.PlacementSubstrate, Status: "active"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockReg.Close()

	mux := substrateLifecycleMux(t, mockReg.URL)
	req := httptest.NewRequest("POST", "/v1/agents/root/revoke", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke got %d want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(revoked, []string{"root"}) {
		t.Fatalf("revoked=%v, want [root] forwarded to registry", revoked)
	}
	if len(dismissed) != 0 {
		t.Fatalf("revoke must NOT mark the substrate record terminal (parity: mint-time only), dismissed=%v", dismissed)
	}
}

// ── liveness: hub state + status endpoint (AC 8: no record mutation) ───────────

func TestHub_LivenessTracksConnection(t *testing.T) {
	h := newSubstrateHub()
	if on, _ := h.liveness("acme"); on {
		t.Fatal("no connection ⇒ offline")
	}
	c := newTestConn("acme")
	c.lastSeen.Store(time.Now().UnixNano())
	h.register(c)
	on, ls := h.liveness("acme")
	if !on {
		t.Fatal("registered connection ⇒ online")
	}
	if ls.IsZero() {
		t.Fatal("lastSeen should be surfaced")
	}
	// A disconnect flips liveness offline — pure in-memory, no record touched.
	h.unregister(c)
	if on, _ := h.liveness("acme"); on {
		t.Fatal("unregistered ⇒ offline")
	}
}

func TestSubstrateStatus_LivenessFromHub(t *testing.T) {
	mockReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/orgs/acme" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(registry.Organization{OrgID: "acme", Substrate: &registry.Substrate{Status: registry.SubstrateActive, CertFingerprint: "fp"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockReg.Close()

	hub := newSubstrateHub()
	api := &substrateAPI{registryURL: mockReg.URL, cpBearer: func() string { return "" }, trustDomain: "cluster.local", hub: hub}

	call := func() map[string]any {
		r := httptest.NewRequest("GET", "/v1/orgs/acme/substrate", nil)
		r.SetPathValue("id", "acme")
		w := httptest.NewRecorder()
		api.status(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status got %d want 200 (body %s)", w.Code, w.Body.String())
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	// Offline: record present (status active) but online=false — the record's
	// active LATCH is orthogonal to live reachability.
	if out := call(); out["online"] != false || out["status"] != "active" {
		t.Fatalf("offline: online=%v status=%v, want false/active", out["online"], out["status"])
	}
	// Connect ⇒ online=true, record unchanged.
	hub.register(newTestConn("acme"))
	if out := call(); out["online"] != true {
		t.Fatalf("connected: online=%v, want true", out["online"])
	}
}
