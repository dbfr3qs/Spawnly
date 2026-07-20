package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

func TestSubstrate_ConnectLandsPending(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	rec := doReq(t, mux, "PUT", "/v1/orgs/acme/substrate", map[string]string{
		"certFingerprint": "deadbeef", "certSerial": "01",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	org, _, _ := s.GetOrg(context.Background(), "acme")
	if org.Substrate == nil || org.Substrate.Status != registry.SubstratePending {
		t.Fatalf("substrate = %+v, want pending", org.Substrate)
	}
	if org.Substrate.CertFingerprint != "deadbeef" || org.Substrate.CertSerial != "01" {
		t.Fatalf("substrate fields = %+v", org.Substrate)
	}
	if org.Substrate.IssuedAt.IsZero() {
		t.Fatal("issuedAt must be stamped")
	}
}

func TestSubstrate_RequiresFingerprint(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	rec := doReq(t, mux, "PUT", "/v1/orgs/acme/substrate", map[string]string{"certSerial": "01"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fingerprint status = %d, want 400", rec.Code)
	}
}

func TestSubstrate_UnknownOrg404(t *testing.T) {
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	rec := doReq(t, mux, "PUT", "/v1/orgs/ghost/substrate", map[string]string{"certFingerprint": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown org status = %d, want 404", rec.Code)
	}
}

// ONE substrate per org is structural: a re-PUT (rotate) overwrites rather than
// appending — the record is a single field, so the second fingerprint wins.
func TestSubstrate_OnePerOrg_RotateOverwrites(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	_ = doReq(t, mux, "PUT", "/v1/orgs/acme/substrate", map[string]string{"certFingerprint": "first"})
	_ = doReq(t, mux, "PUT", "/v1/orgs/acme/substrate", map[string]string{"certFingerprint": "second"})

	org, _, _ := s.GetOrg(context.Background(), "acme")
	if org.Substrate == nil || org.Substrate.CertFingerprint != "second" {
		t.Fatalf("rotate did not overwrite: %+v", org.Substrate)
	}
}

func TestSubstrate_Disconnect(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	_ = doReq(t, mux, "PUT", "/v1/orgs/acme/substrate", map[string]string{"certFingerprint": "x"})
	rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/substrate", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	org, _, _ := s.GetOrg(context.Background(), "acme")
	if org.Substrate != nil {
		t.Fatalf("substrate should be cleared, got %+v", org.Substrate)
	}
}
