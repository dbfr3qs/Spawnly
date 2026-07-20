package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/spawnly/platform/internal/registry"
)

// registerSubstrateRoutes wires the per-org substrate registration lifecycle onto
// mux behind the control-plane gate (cp), the org-endpoints family
// (remote-substrate Phase 3). These persist the substrate RECORD (status +
// issued-cert fingerprint/serial + timestamps); the org-bound x509 identity is
// ISSUED one tier up in the orchestrator (which holds the dev CA) and only its
// fingerprint/serial ever reach this store — never key material. Org-admin
// authorization is enforced in the orchestrator's requireOrgAdmin; the registry
// is the single dual-write choke point and trusts the cp caller, exactly like
// registerAnchorRoutes.
//
// EXACTLY ONE substrate per org is structural: the record is a single field on
// the org (registry.Organization.Substrate), so PUT upserts/rotates rather than
// appending — a second connect replaces the first.
func registerSubstrateRoutes(mux *http.ServeMux, s registry.Store, cp func(http.HandlerFunc) http.HandlerFunc) {
	// PUT /v1/orgs/{id}/substrate — record (or rotate) the org's substrate. It
	// always lands `pending`: the record only notes that an identity was issued
	// (its fingerprint/serial) and when; the substrate flips to `active` when its
	// channel first connects (Phase 4). A rotate (re-PUT) overwrites the prior
	// fingerprint/serial, logically invalidating the old credential.
	mux.HandleFunc("PUT /v1/orgs/{id}/substrate", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		var body struct {
			CertFingerprint string `json:"certFingerprint"`
			CertSerial      string `json:"certSerial"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The fingerprint is the only cryptographic material retained; require it so
		// a rotate always supersedes a prior credential with a concrete new one.
		if body.CertFingerprint == "" {
			http.Error(w, "certFingerprint is required", http.StatusBadRequest)
			return
		}
		org, ok, err := s.GetOrg(ctx, id)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		org.Substrate = &registry.Substrate{
			Status:          registry.SubstratePending,
			CertFingerprint: body.CertFingerprint,
			CertSerial:      body.CertSerial,
			IssuedAt:        time.Now().UTC(),
		}
		if err := s.PutOrg(ctx, org); storeErr(w, err) {
			return
		}
		writeJSON(w, org)
	}))

	// POST /v1/orgs/{id}/substrate/activate — flip the org's substrate to `active`
	// (remote-substrate Phase 4). Called by the orchestrator (control-plane bearer)
	// the first time the substrate's dispatch channel connects. Idempotent: an
	// already-active substrate is a no-op re-write. Never re-creates a cleared
	// record (a disconnected org whose record was DELETEd stays gone — 404) so a
	// stale channel cannot resurrect a revoked substrate. `active` is the routing
	// latch: it survives a later channel disconnect (spec AC 8 — no record mutation
	// on drop), while online/offline liveness is separate in-memory channel state.
	mux.HandleFunc("POST /v1/orgs/{id}/substrate/activate", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		org, ok, err := s.GetOrg(ctx, id)
		if storeErr(w, err) {
			return
		}
		if !ok || org.Substrate == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if org.Substrate.Status != registry.SubstrateActive {
			now := time.Now().UTC()
			org.Substrate.Status = registry.SubstrateActive
			org.Substrate.ActivatedAt = &now
			if err := s.PutOrg(ctx, org); storeErr(w, err) {
				return
			}
		}
		writeJSON(w, org.Substrate)
	}))

	// DELETE /v1/orgs/{id}/substrate — disconnect/revoke the org's substrate. The
	// record is cleared (the issued credential's fingerprint no longer maps to an
	// active substrate); the org's agents fall back to platform-hosted, exactly as
	// an org that never connected one. Idempotent (a no-op when none is present).
	mux.HandleFunc("DELETE /v1/orgs/{id}/substrate", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		org, ok, err := s.GetOrg(ctx, id)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		org.Substrate = nil
		if err := s.PutOrg(ctx, org); storeErr(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
