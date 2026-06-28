package mobilegateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

func TestHub_PerUserIsolation(t *testing.T) {
	h := NewHub()
	alice, ca, okA := h.Subscribe("alice")
	defer ca()
	bob, cb, okB := h.Subscribe("bob")
	defer cb()
	if !okA || !okB {
		t.Fatal("first subscriptions should succeed")
	}

	h.Publish("alice", Event{ConsentRequestID: "req-1"})

	select {
	case e := <-alice:
		if e.ConsentRequestID != "req-1" {
			t.Fatalf("alice got %+v", e)
		}
	default:
		t.Fatal("alice did not receive her event")
	}
	select {
	case e := <-bob:
		t.Fatalf("bob received another user's event: %+v", e)
	default:
	}
}

func TestHub_PerUserStreamCap(t *testing.T) {
	h := NewHub()
	cancels := make([]func(), 0, maxStreamsPerUser)
	for i := 0; i < maxStreamsPerUser; i++ {
		_, c, ok := h.Subscribe("alice")
		if !ok {
			t.Fatalf("subscribe %d should succeed (under cap)", i)
		}
		cancels = append(cancels, c)
	}
	// One past the cap is refused.
	if _, _, ok := h.Subscribe("alice"); ok {
		t.Fatal("subscribe past the per-user cap should be refused")
	}
	// Freeing one slot lets a new subscribe in.
	cancels[0]()
	if _, _, ok := h.Subscribe("alice"); !ok {
		t.Fatal("subscribe should succeed after a slot frees")
	}
	// A different user is unaffected.
	if _, _, ok := h.Subscribe("bob"); !ok {
		t.Fatal("a different user must not be capped by alice's streams")
	}
}

// unregTransport always reports the device as unregistered, to drive pruning.
type unregTransport struct{}

func (unregTransport) Name() string                              { return "unreg" }
func (unregTransport) Send(context.Context, Device, Event) error { return ErrDeviceUnregistered }

func TestNotify_PrunesUnregisteredDevices(t *testing.T) {
	d := testDeps(nil, "http://unused")
	d.Transport = unregTransport{}
	d.Devices.Put(context.Background(), dev("d1", "alice", "dead-token"))
	mux := BuildInternalMux(d)

	w := do(t, mux, "POST", "/internal/notify", "",
		`{"type":"consent_pending","id":"req-1","user":"alice","parentType":"p","childType":"c"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d", w.Code)
	}
	left, _ := d.Devices.ListByUser(context.Background(), "alice")
	if len(left) != 0 {
		t.Fatalf("unregistered device not pruned: %+v", left)
	}
}

func TestEventPayload_CarriesNoSecrets(t *testing.T) {
	// The push/SSE payload must never carry scopes or the binding message — the
	// app re-fetches those over the authed channel. Guard the JSON shape.
	b, _ := json.Marshal(Event{ConsentRequestID: "r", ParentType: "p", ChildType: "c"})
	for _, banned := range []string{"scope", "binding"} {
		if strings.Contains(strings.ToLower(string(b)), banned) {
			t.Fatalf("event payload %s contains %q", b, banned)
		}
	}
}

func TestNotify_ControlPlaneAuth(t *testing.T) {
	d := testDeps(nil, "http://unused")
	d.ControlPlane = controlplane.NewSharedSecret("s3cret")
	mux := BuildInternalMux(d)

	body := `{"type":"consent_pending","id":"req-1","user":"alice","parentType":"p","childType":"c"}`

	// No / wrong secret → 401.
	w := do(t, mux, "POST", "/internal/notify", "", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no secret: got %d, want 401", w.Code)
	}
	w = do(t, mux, "POST", "/internal/notify", "wrong", body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: got %d, want 401", w.Code)
	}
	// Correct secret (presented as a bearer, matching the registry's hop) → 204.
	w = do(t, mux, "POST", "/internal/notify", "s3cret", body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("correct secret: got %d, want 204", w.Code)
	}
}

func TestNotify_BadInput(t *testing.T) {
	mux := BuildInternalMux(testDeps(nil, "http://unused")) // ControlPlane defaults to AllowAll
	for _, body := range []string{
		`{"type":"consent_pending","user":"alice"}`, // no id
		`{"type":"consent_pending","id":"r"}`,       // no user
		`not json`,
	} {
		w := do(t, mux, "POST", "/internal/notify", "", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400", body, w.Code)
		}
	}
}

// countingTransport records Send calls to prove per-device fan-out.
type countingTransport struct{ n int32 }

func (c *countingTransport) Name() string { return "counting" }
func (c *countingTransport) Send(context.Context, Device, Event) error {
	atomic.AddInt32(&c.n, 1)
	return nil
}

func TestNotify_FanOutToDevices(t *testing.T) {
	d := testDeps(nil, "http://unused")
	ct := &countingTransport{}
	d.Transport = ct
	// alice has two devices; bob has one — only alice's should be pushed.
	d.Devices.Put(context.Background(), dev("d1", "alice", "ta1"))
	d.Devices.Put(context.Background(), dev("d2", "alice", "ta2"))
	d.Devices.Put(context.Background(), dev("d3", "bob", "tb1"))
	mux := BuildInternalMux(d)

	w := do(t, mux, "POST", "/internal/notify", "",
		`{"type":"consent_pending","id":"req-1","user":"alice","parentType":"p","childType":"c"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d", w.Code)
	}
	if got := atomic.LoadInt32(&ct.n); got != 2 {
		t.Fatalf("transport sent %d times, want 2 (alice's devices only)", got)
	}
}

// TestStream_DeliversEvent exercises the real SSE wire across the two ports the
// gateway runs in production: open /me/stream on the public server, fire a
// notification through /internal/notify on the internal server, and (because
// both share one Deps and thus one Hub) read the event off the stream.
func TestStream_DeliversEvent(t *testing.T) {
	v := mapValidator{byToken: map[string]tokenvalidator.Claims{"alice": rwClaims("user:alice")}}
	d := testDeps(v, "http://unused")
	d.Hub = NewHub() // shared by both muxes below
	pub := httptest.NewServer(BuildMux(d))
	defer pub.Close()
	internal := httptest.NewServer(BuildInternalMux(d))
	defer internal.Close()

	// Open the stream on the public server.
	req, _ := http.NewRequest("GET", pub.URL+"/me/stream", nil)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream open: got %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	// Drain the ": connected" preamble so we know the handler has subscribed.
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read preamble: %v", err)
	}

	// Fire the notification on the internal server (control-plane AllowAll default).
	go http.Post(internal.URL+"/internal/notify", "application/json",
		strings.NewReader(`{"type":"consent_pending","id":"req-42","user":"alice","parentType":"p","childType":"c"}`))

	// Read lines until we see the data frame (or time out).
	done := make(chan string, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				done <- strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				return
			}
		}
	}()

	select {
	case data := <-done:
		var e Event
		if json.Unmarshal([]byte(data), &e) != nil || e.ConsentRequestID != "req-42" {
			t.Fatalf("stream data = %q, want event req-42", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}
