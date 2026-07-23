package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate"
)

// sentinelBroker is a no-op EntryBroker used to prove which broker the selector
// wired without touching a real SPIRE server.
type sentinelBroker struct{}

func (sentinelBroker) CreateEntry(context.Context, substrate.EntrySpec) error { return nil }
func (sentinelBroker) DeleteEntry(context.Context, string) error              { return nil }

// clearBrokerEnv zeroes every broker-selection var so each case starts clean
// regardless of ordering.
func clearBrokerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SPIRE_ENTRY_API_ADDR", "SPIRE_SERVER_SOCKET", "SPIRE_WORKLOAD_API_SOCKET", "SPIRE_JWKS_URL", "SUBSTRATE_KMS_KEY_ARN", "SUBSTRATE_CA_CERT_FILE", "SUBSTRATE_CA_KEY_FILE", "SUBSTRATE_PUBLIC_ADDR"} {
		t.Setenv(k, "")
	}
}

func newBrokerAPIForTest(t *testing.T) (*substrateAPI, error) {
	t.Helper()
	api, err := newSubstrateAPI(registry.NewMock(nil), "http://registry.invalid", func() string { return "" }, nil)
	if api != nil {
		// Registered AFTER the caller's seam-restoring cleanups, so LIFO ordering
		// stops the background initializers FIRST — otherwise they keep reading the
		// package-level seams as the cleanups restore them (a real data race).
		t.Cleanup(api.stopInit)
	}
	return api, err
}

// TestBrokerSelection_GRPC: SPIRE_ENTRY_API_ADDR set ⇒ the gRPC broker is wired via
// the seam (the exec broker is NOT used).
func TestBrokerSelection_GRPC(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("SPIRE_ENTRY_API_ADDR", "spire-server:8081")

	var gotAddr, gotSock, gotTD string
	orig := spireBrokerFromEnv
	t.Cleanup(func() { spireBrokerFromEnv = orig })
	spireBrokerFromEnv = func(_ context.Context, addr, sock, td string) (substrate.EntryBroker, error) {
		gotAddr, gotSock, gotTD = addr, sock, td
		return sentinelBroker{}, nil
	}

	api, err := newBrokerAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The EDGE path wires the broker ASYNCHRONOUSLY (the orchestrator's own SVID is
	// often not deliverable in the first seconds after a rollout), so wait for the
	// late publish rather than asserting it is already there.
	es := waitForEntrySvc(t, api)
	if _, ok := es.Broker.(sentinelBroker); !ok {
		t.Fatalf("expected the gRPC (sentinel) broker, got %T", es.Broker)
	}
	if gotAddr != "spire-server:8081" {
		t.Fatalf("broker seam got addr %q", gotAddr)
	}
	if gotSock != "" || gotTD == "" {
		t.Fatalf("broker seam got sock=%q td=%q", gotSock, gotTD)
	}
}

// TestBrokerSelection_Exec: SPIRE_SERVER_SOCKET set (and no gRPC addr) ⇒ the
// unchanged local-sim exec broker.
func TestBrokerSelection_Exec(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("SPIRE_SERVER_SOCKET", "/tmp/spire.sock")

	orig := spireBrokerFromEnv
	t.Cleanup(func() { spireBrokerFromEnv = orig })
	spireBrokerFromEnv = func(context.Context, string, string, string) (substrate.EntryBroker, error) {
		t.Fatal("gRPC seam must NOT be consulted on the exec path")
		return nil, nil
	}

	api, err := newBrokerAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.entries == nil {
		t.Fatal("exec path: EntryService must be wired")
	}
	if _, ok := api.entries.Broker.(*execEntryBroker); !ok {
		t.Fatalf("expected *execEntryBroker, got %T", api.entries.Broker)
	}
}

// TestBrokerSelection_BothSetHardError: gRPC addr + exec socket ⇒ hard startup
// error, and the gRPC seam is never consulted.
func TestBrokerSelection_BothSetHardError(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("SPIRE_ENTRY_API_ADDR", "spire-server:8081")
	t.Setenv("SPIRE_SERVER_SOCKET", "/tmp/spire.sock")

	orig := spireBrokerFromEnv
	t.Cleanup(func() { spireBrokerFromEnv = orig })
	called := false
	spireBrokerFromEnv = func(context.Context, string, string, string) (substrate.EntryBroker, error) {
		called = true
		return sentinelBroker{}, nil
	}

	if _, err := newBrokerAPIForTest(t); err == nil {
		t.Fatal("both brokers configured must be a hard startup error")
	}
	if called {
		t.Fatal("both-set must be rejected before building any broker")
	}
}

// TestBrokerSelection_GRPCFailsClosed: a gRPC broker init failure ⇒ nil EntryService
// (Entry API 503), NOT a hard error and NOT a fallback to the exec broker.
func TestBrokerSelection_GRPCFailsClosed(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("SPIRE_ENTRY_API_ADDR", "spire-server:8081")

	orig := spireBrokerFromEnv
	t.Cleanup(func() { spireBrokerFromEnv = orig })
	spireBrokerFromEnv = func(context.Context, string, string, string) (substrate.EntryBroker, error) {
		return nil, context.DeadlineExceeded
	}

	api, err := newBrokerAPIForTest(t)
	if err != nil {
		t.Fatalf("gRPC init failure must NOT be a hard error, got %v", err)
	}
	if api.entries != nil {
		t.Fatal("gRPC init failure must leave the Entry API disabled (nil EntryService)")
	}
}

// TestBrokerSelection_NoneConfigured: neither var ⇒ Entry API disabled.
func TestBrokerSelection_NoneConfigured(t *testing.T) {
	clearBrokerEnv(t)
	api, err := newBrokerAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.entries != nil {
		t.Fatal("no broker configured must leave the Entry API disabled")
	}
}

// waitForEntrySvc blocks until the (asynchronously published) EntryService appears.
func waitForEntrySvc(t *testing.T, api *substrateAPI) *substrate.EntryService {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if es := api.entrySvc(); es != nil {
			return es
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("EntryService was never published")
	return nil
}

// TestBrokerSelection_GRPC_RetriesUntilSVIDArrives is the regression test for the
// startup race that repeatedly disabled the edge Entry API: the orchestrator's SVID
// is not deliverable for the first N attempts (the spire-controller-manager mints
// its entry per-POD, so every rollout re-runs the race). The broker MUST keep
// retrying and enable itself with no restart — a one-shot init latched the Entry
// API off at 503 until a human intervened.
func TestBrokerSelection_GRPC_RetriesUntilSVIDArrives(t *testing.T) {
	clearBrokerEnv(t)
	t.Setenv("SPIRE_ENTRY_API_ADDR", "spire-server:8081")

	origInterval := entryAPIRetryInterval
	entryAPIRetryInterval = time.Millisecond
	t.Cleanup(func() { entryAPIRetryInterval = origInterval })

	var mu sync.Mutex
	attempts := 0
	orig := spireBrokerFromEnv
	t.Cleanup(func() { spireBrokerFromEnv = orig })
	spireBrokerFromEnv = func(context.Context, string, string, string) (substrate.EntryBroker, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			// What the real failure looks like: no SVID inside the init window.
			return nil, errors.New("obtain orchestrator SVID from workload API: context deadline exceeded")
		}
		return sentinelBroker{}, nil
	}

	api, err := newBrokerAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	es := waitForEntrySvc(t, api)
	if _, ok := es.Broker.(sentinelBroker); !ok {
		t.Fatalf("expected the sentinel broker after retry, got %T", es.Broker)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 3 {
		t.Fatalf("expected the broker seam to be retried, got %d attempt(s)", attempts)
	}
}
