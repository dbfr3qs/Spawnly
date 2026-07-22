package main

import (
	"context"
	"testing"

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
	for _, k := range []string{"SPIRE_ENTRY_API_ADDR", "SPIRE_SERVER_SOCKET", "SPIRE_WORKLOAD_API_SOCKET", "SPIRE_JWKS_URL", "SUBSTRATE_KMS_KEY_ARN", "SUBSTRATE_CA_CERT_FILE", "SUBSTRATE_CA_KEY_FILE"} {
		t.Setenv(k, "")
	}
}

func newBrokerAPIForTest(t *testing.T) (*substrateAPI, error) {
	t.Helper()
	return newSubstrateAPI(registry.NewMock(nil), "http://registry.invalid", func() string { return "" }, nil)
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
	if api.entries == nil {
		t.Fatal("gRPC path: EntryService must be wired")
	}
	if _, ok := api.entries.Broker.(sentinelBroker); !ok {
		t.Fatalf("expected the gRPC (sentinel) broker, got %T", api.entries.Broker)
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
