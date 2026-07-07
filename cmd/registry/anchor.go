package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/registry"
)

// awsAccountID is a 12-digit AWS account number.
var awsAccountID = regexp.MustCompile(`^[0-9]{12}$`)

// defaultAnchorSTSClient is the HTTP client the anchor activate handler uses to
// replay the presigned GetCallerIdentity. A short timeout bounds the outbound
// call (the caller-supplied endpoint is already constrained to the trusted STS
// endpoint by the shared verifier's SSRF guard).
var defaultAnchorSTSClient = &http.Client{Timeout: 10 * time.Second}

// registerAnchorRoutes wires the per-org attestation-anchor lifecycle onto mux
// behind the control-plane gate (cp), the org-endpoints family (Phase 8). These
// give the anchor records that landed in the contract in Phase 1 a lifecycle:
// register (pending) -> prove control -> active. NOTHING verifies a runtime
// credential against an anchor yet — that is Phase 9. Org-admin authorization is
// enforced one tier up (the orchestrator's requireOrgAdmin); the registry is the
// single dual-write choke point and trusts the cp caller, exactly like the rest
// of registerOrgRoutes.
func registerAnchorRoutes(mux *http.ServeMux, s registry.Store, cp func(http.HandlerFunc) http.HandlerFunc, stsClient *http.Client) {
	if stsClient == nil {
		stsClient = defaultAnchorSTSClient
	}

	// PUT /v1/orgs/{id}/anchor — register (or re-register) the org's STS trust
	// anchor. It always lands `pending`: registration only records the claimed
	// account + role pattern; the org must then PROVE control (activate) before
	// anything trusts it. Re-registering deliberately resets an active anchor back
	// to pending (the account/role changed => the old proof no longer holds).
	mux.HandleFunc("PUT /v1/orgs/{id}/anchor", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		var body struct {
			Type           string `json:"type"`
			AWSAccountID   string `json:"awsAccountId"`
			RoleArnPattern string `json:"roleArnPattern"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Type != "sts" {
			http.Error(w, "unsupported anchor type", http.StatusBadRequest)
			return
		}
		if !awsAccountID.MatchString(body.AWSAccountID) {
			http.Error(w, "invalid awsAccountId", http.StatusBadRequest)
			return
		}
		// Require a real ARN pattern, not a bare "*": the pattern is one half of
		// proof-of-control (the account is the other), so a catch-all would let any
		// role in the account activate the anchor.
		if !strings.HasPrefix(body.RoleArnPattern, "arn:") {
			http.Error(w, "invalid roleArnPattern", http.StatusBadRequest)
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
		org.Anchor = &registry.AttestationAnchor{
			Type:           "sts",
			AWSAccountID:   body.AWSAccountID,
			RoleArnPattern: body.RoleArnPattern,
			Status:         registry.AnchorPending,
		}
		if err := s.PutOrg(ctx, org); storeErr(w, err) {
			return
		}
		writeJSON(w, org)
	}))

	// DELETE /v1/orgs/{id}/anchor — clear the org's anchor. The org's agents fall
	// back to the platform default attestor (in-cluster SPIRE), exactly as an
	// unanchored org.
	mux.HandleFunc("DELETE /v1/orgs/{id}/anchor", cp(func(w http.ResponseWriter, r *http.Request) {
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
		org.Anchor = nil
		if err := s.PutOrg(ctx, org); storeErr(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// POST /v1/orgs/{id}/anchor/activate — prove control of the AWS account. The
	// body carries a presigned STS GetCallerIdentity request; the registry replays
	// it (AWS is the attestor) and activates the anchor IFF the attested account ==
	// the anchor's awsAccountId AND the attested assumed-role ARN matches the
	// anchor's roleArnPattern. This is exactly the credential the Phase-9 runtime
	// path will present, so a successful activation proves the org can mint under
	// this anchor.
	//
	// EVERY failure returns a generic 400 with no detail (oracle-free): a caller
	// must not be able to distinguish "wrong account" from "no anchor" from "bad
	// signature" — that would leak the anchor's expected account/role or its
	// existence. Details are logged server-side only.
	mux.HandleFunc("POST /v1/orgs/{id}/anchor/activate", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		var body struct {
			Credential string `json:"credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "activation failed", http.StatusBadRequest)
			return
		}
		org, status, err := activateAnchor(ctx, s, stsClient, id, body.Credential)
		if err != nil {
			// Generic message regardless of the underlying reason (oracle-free).
			log.Printf("anchor activate %s: %v", id, err)
			http.Error(w, "activation failed", status)
			return
		}
		writeJSON(w, org)
	}))
}

// activateAnchor performs the proof-of-control check and, on success, flips the
// org's anchor to active. It returns the updated org on success; on ANY failure
// it returns a non-nil error and the HTTP status the handler should send — but
// the caller must NOT surface the error text (oracle-free). A store failure is a
// 500; every proof failure (missing/invalid anchor, replay failure, account or
// role mismatch) is a 400. Split out from the handler so it is unit-testable
// against a local stub STS server.
func activateAnchor(ctx context.Context, s registry.Store, stsClient *http.Client, id, credential string) (registry.Organization, int, error) {
	org, ok, err := s.GetOrg(ctx, id)
	if err != nil {
		return registry.Organization{}, http.StatusInternalServerError, err
	}
	if !ok {
		return registry.Organization{}, http.StatusBadRequest, errAnchorProof("org not found")
	}
	if org.Anchor == nil || org.Anchor.Type != "sts" {
		return registry.Organization{}, http.StatusBadRequest, errAnchorProof("org has no sts anchor")
	}
	if credential == "" {
		return registry.Organization{}, http.StatusBadRequest, errAnchorProof("missing credential")
	}

	// Replay the presigned request against STS (or the configured trusted
	// endpoint). The shared verifier enforces the SSRF host guard.
	identity, err := attestor.VerifyCallerIdentity(ctx, stsClient, credential)
	if err != nil {
		return registry.Organization{}, http.StatusBadRequest, err
	}
	if identity.Account != org.Anchor.AWSAccountID {
		return registry.Organization{}, http.StatusBadRequest, errAnchorProof("account mismatch")
	}
	if !attestor.MatchRoleArnPattern(org.Anchor.RoleArnPattern, identity.Arn) {
		return registry.Organization{}, http.StatusBadRequest, errAnchorProof("role ARN does not match pattern")
	}

	now := time.Now().UTC()
	org.Anchor.Status = registry.AnchorActive
	org.Anchor.ActivatedAt = &now
	if err := s.PutOrg(ctx, org); err != nil {
		return registry.Organization{}, http.StatusInternalServerError, err
	}
	return org, http.StatusOK, nil
}

// errAnchorProof labels a proof-of-control failure reason for server-side logs.
// The handler NEVER echoes it to the caller (oracle-free 400).
func errAnchorProof(reason string) error { return errors.New("anchor proof failed: " + reason) }
