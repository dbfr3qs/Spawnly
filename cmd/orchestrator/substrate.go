package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
	"github.com/spawnly/platform/internal/substrate"
	"github.com/spawnly/platform/internal/substrate/devca"
	"github.com/spawnly/platform/internal/substrate/kmsca"
	"github.com/spawnly/platform/internal/substrate/spirebroker"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

// substrateAPI hosts the remote-substrate connect surface (org-admin-issued
// x509 identity) and the CP-mediated Entry API (substrate-SVID-authenticated),
// mirroring the anchor/remote-agent surfaces. Everything is server-authoritative:
// the org for a connect comes from the requireOrgAdmin-bound path {id}, and the
// org for an Entry API call comes from the caller's VERIFIED substrate-agent
// SVID — never a request body.
type substrateAPI struct {
	registryURL string
	cpBearer    func() string
	trustDomain string

	// reg is the registry client used by the substrate-scoped agent list
	// (reconcile-on-connect source). It reads ALL records; listAgents filters to
	// the caller's own org server-side.
	reg registry.Client
	// hub holds the live dispatch-channel connections (Phase 4). nil ⇒ the channel
	// endpoint returns 503. In-memory, one connection per org (newest-wins).
	hub *substrateHub
	// readTimeout bounds how long the channel waits for the next UP frame before
	// treating it as dead (0 ⇒ the 90s default). Overridable in tests.
	readTimeout time.Duration

	// issuer mints the org-bound x509 identity behind the CA-agnostic LeafIssuer
	// seam — the dev CA locally, or the KMS-backed CA on the edge. nil when no CA
	// is configured (feature off) ⇒ connect returns 503, never insecure.
	issuer substrate.LeafIssuer
	// entries authorizes+brokers CP-mediated SPIRE entries and provisions the
	// substrate-agent bootstrap entry. nil when no entry broker is configured ⇒
	// the Entry API returns 503 and connect provisions no bootstrap entry (Phase 4
	// reconcile re-drives it once the broker is wired).
	//
	// On the EDGE (gRPC broker) this is populated ASYNCHRONOUSLY: the orchestrator's
	// own SVID may not exist yet at startup (the spire-controller-manager mints its
	// entry per-POD, so every rollout re-runs the race, and the SPIRE agent then has
	// to sync it). A one-shot init would latch the Entry API off until someone
	// noticed and restarted the pod — a transient startup delay permanently
	// disabling a feature. entriesMu guards the late publish; read via entrySvc().
	entriesMu sync.RWMutex
	entries   *substrate.EntryService
	// initCancel stops the background edge initializers and initWG waits for them
	// to exit. Without these newSubstrateAPI would leak goroutines that retry
	// forever and outlive their owner (they also read package-level seams, which
	// the race detector rightly flags once a test restores them underneath).
	initCancel context.CancelFunc
	initWG     sync.WaitGroup
	// svidAuth authenticates an Entry API caller's substrate-agent SVID and
	// returns its org. nil ⇒ Entry API 503. Published under entriesMu together with
	// entries (the Entry API requires both), read via svidAuthenticator().
	svidAuth substrateSVIDAuthenticator

	// bootstrap selectors for the substrate-agent's own entry (the bundle's known
	// namespace + service account; pod-name is unpredictable for a Deployment).
	agentNamespace string
	agentSA        string
	// leafTTL bounds the issued identity's validity (reported to the admin).
	leafTTL time.Duration

	// Optional REAL-endpoint install hints (Phase 5, edge). When set (AWS overlay),
	// the connect response's Install map tells the org admin exactly where their
	// substrate must dial: the SPIRE node-attestation endpoint (host:port, e.g.
	// "spire.spawnly.run:443") and the public substrate base URL (the WS dispatch
	// channel + Entry API origin, e.g. "https://substrate.spawnly.run"). Empty ⇒
	// the keys are ABSENT — local kind behavior byte-identical. Informational
	// only: nothing here is an authz input, so the hints are config, not contract.
	spireEndpointHint string
	publicURLHint     string
}

// substrateSVIDAuthenticator verifies an Entry API caller's substrate-agent SVID
// and returns its org segment (server-authoritative). Any failure returns a
// non-nil error the handler renders as an oracle-free 401.
type substrateSVIDAuthenticator interface {
	authenticate(ctx context.Context, r *http.Request) (callerOrg string, err error)
}

// registerSubstrateRoutes wires the connect surface (org-endpoints family) and
// the Entry API onto mux. Connect/rotate/disconnect are org-admin-gated; status
// is org-member-gated (rides the org record); the Entry API is authenticated by
// the caller's substrate SVID, NOT a human token.
func registerSubstrateRoutes(mux *http.ServeMux, api *substrateAPI, v tokenvalidator.TokenValidator, audience, readScope, writeScope string, sdb spicedb.Client) {
	// POST /v1/orgs/{id}/substrate — connect (or ROTATE) the org's substrate:
	// issue the org-bound x509 identity, provision the substrate-agent bootstrap
	// entry, record the substrate `pending`, and return the identity ONCE.
	// refuseIfOrgDisabled sits INSIDE requireOrgAdmin so a stale admin token can't
	// mint a fresh org-bound credential for a SUSPENDED org (issuance is more
	// consequential than recording an anchor).
	mux.HandleFunc("POST /v1/orgs/{id}/substrate", requireOrgAdmin(v, audience, writeScope, sdb,
		refuseIfOrgDisabled(api.registryURL, api.cpBearer, api.connect)))
	// GET /v1/orgs/{id}/substrate — substrate status (pending/active + fingerprint).
	mux.HandleFunc("GET /v1/orgs/{id}/substrate", requireOrgMember(v, audience, readScope, sdb, api.status))
	// DELETE /v1/orgs/{id}/substrate — disconnect/revoke the org's substrate.
	mux.HandleFunc("DELETE /v1/orgs/{id}/substrate", requireOrgAdmin(v, audience, writeScope, sdb, api.disconnect))

	// CP-mediated Entry API — authenticated by the caller's substrate SVID, not a
	// human token. Creates/deletes SPIRE entries ONLY under the caller's own org.
	mux.HandleFunc("POST /v1/substrate/entries", api.createEntry)
	mux.HandleFunc("DELETE /v1/substrate/entries", api.deleteEntry)

	// Dispatch channel + reconcile source (Phase 4) — both substrate-SVID
	// authenticated, own-org-only. The channel is the persistent WebSocket the
	// substrate-agent holds; the list is its desired-state pull on (re)connect.
	mux.HandleFunc("GET /v1/substrate/channel", api.channelHandler)
	mux.HandleFunc("GET /v1/substrate/agents", api.listAgents)
}

// substratePublicMux builds a mux serving ONLY the substrate surface — the exact
// registerSubstrateRoutes set, with the SAME handlers and guards. It backs the
// dedicated internet-facing listener (SUBSTRATE_PUBLIC_ADDR): the ALB host-rule
// (substrate.spawnly.run, Phase 5) targets this port so ONLY the substrate routes
// (channel WS, Entry API, substrate agents list, org-admin connect endpoints) are
// reachable from the internet — every other orchestrator route (/spawn, /v1/agents,
// the admin family, …) is absent here and 404s, mirroring the mobile-gateway
// public/internal two-port split. Auth is UNCHANGED: the routes authenticate
// exactly as on the main mux (SVID for channel/entries/agents; OAuth org-admin/
// member for connect). The api is the SAME instance the main mux uses (shared hub),
// so a spawn pushed on :8080 reaches the channel a substrate holds on this port.
func substratePublicMux(api *substrateAPI, v tokenvalidator.TokenValidator, audience, readScope, writeScope string, sdb spicedb.Client) *http.ServeMux {
	m := http.NewServeMux()
	registerSubstrateRoutes(m, api, v, audience, readScope, writeScope, sdb)
	return m
}

// startSubstratePublicListener starts the dedicated internet-facing substrate
// listener on addr in a goroutine, serving handler (the substrate-only mux). It is
// invoked ONLY when SUBSTRATE_PUBLIC_ADDR is set — unset on local kind, so that
// path keeps a single :8080 listener unchanged. Explicit read/idle timeouts harden
// the internet-facing server (Slowloris defense); no WriteTimeout, because the
// dispatch channel is a long-lived WebSocket (same stance as the main server). A
// bind/serve failure is fatal, matching the mobile-gateway internal-listener
// precedent — a substrate edge that cannot come up must not fail silently.
func startSubstratePublicListener(addr string, handler http.Handler) {
	go func() {
		log.Printf("orchestrator substrate public listener on %s", addr)
		srv := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		log.Fatal(srv.ListenAndServe())
	}()
}

// connectResponse is the one-time connect payload. identity (cert+key) is
// returned exactly ONCE and never persisted server-side beyond its fingerprint.
type connectResponse struct {
	OrgID           string            `json:"orgId"`
	TrustDomain     string            `json:"trustDomain"`
	Status          string            `json:"status"`
	CertFingerprint string            `json:"certFingerprint"`
	CertSerial      string            `json:"certSerial"`
	NodeID          string            `json:"nodeId"`
	ExpiresAt       time.Time         `json:"expiresAt"`
	Identity        connectIdentity   `json:"identity"`
	Install         map[string]string `json:"install"`
}

type connectIdentity struct {
	CertPEM string `json:"certPem"`
	KeyPEM  string `json:"keyPem"`
	// TrustBundlePEM is the dev CA cert (never the key) the substrate SPIRE agent
	// uses to verify the hosted server's x509pop challenge — part of the one-time
	// bootstrap material.
	TrustBundlePEM string `json:"trustBundlePem"`
}

func (api *substrateAPI) connect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := r.PathValue("id") // requireOrgAdmin bound this to the caller's org.

	if api.issuer == nil {
		http.Error(w, "substrate connect is not configured", http.StatusServiceUnavailable)
		return
	}
	// Normalize+validate through the single org-id source so the org that reaches
	// the cert/SPIFFE paths matches the lowercase Keycloak slug.
	normOrg, err := devca.NormalizeOrgID(orgID)
	if err != nil {
		http.Error(w, "invalid org id", http.StatusBadRequest)
		return
	}

	// 1. Issue the org-bound identity (returned once; only the fingerprint is kept).
	issued, err := api.issuer.IssueOrgIdentity(normOrg)
	if err != nil {
		log.Printf("substrate connect %s: issue identity: %v", normOrg, err)
		http.Error(w, "failed to issue substrate identity", http.StatusInternalServerError)
		return
	}
	// Defense in depth: the issued leaf's CommonName MUST be the requested org.
	if leafOrg, lerr := api.issuer.OrgID(issued.Leaf.CertPEM); lerr != nil || leafOrg != normOrg {
		log.Printf("substrate connect %s: issued leaf org mismatch (%q, err=%v)", normOrg, leafOrg, lerr)
		http.Error(w, "failed to issue substrate identity", http.StatusInternalServerError)
		return
	}

	// 2. Record the substrate `pending` (fingerprint/serial only) via the registry.
	//    Persist BEFORE provisioning the entry so a record-write failure can't leave
	//    an orphaned bootstrap entry — and so the recorded fingerprint is the one
	//    that will gate the Entry API (below). A rotate overwrites the prior record.
	if err := api.putSubstrateRecord(ctx, normOrg, issued.CertFingerprint, issued.CertSerial); err != nil {
		log.Printf("substrate connect %s: persist record: %v", normOrg, err)
		http.Error(w, "failed to record substrate", http.StatusBadGateway)
		return
	}

	// 3. Provision the substrate-agent's OWN bootstrap entry (parented to the org's
	//    x509pop node, which we can pre-author because we just computed the node
	//    fingerprint from the leaf). On a ROTATE the prior leaf's node has a
	//    DIFFERENT fingerprint, so its bootstrap entry (same SPIFFE ID, old parent)
	//    would survive and keep attesting the superseded credential — delete ALL
	//    existing substrate-agent entries for the org FIRST (idempotent no-op on a
	//    fresh connect), then create the new one. Skipped when no broker is
	//    configured (Phase-4 reconcile re-drives it); a provisioning ERROR with a
	//    configured broker fails the connect so it isn't silently half-wired.
	if entries := api.entrySvc(); entries != nil {
		// Rotate hygiene: sweep EVERY entry under the org's x509pop node prefix
		// (the superseded bootstrap entry AND any workload entries the substrate
		// authored under the OLD node), so a replaced/compromised prior leaf can't
		// keep issuing SVIDs. On a fresh connect this is a no-op. The substrate
		// re-authors its workload entries under the NEW node on reconcile-on-connect.
		if derr := entries.InvalidateOrgNodeEntries(ctx, normOrg); derr != nil {
			// Fall back to at least clearing the bootstrap entry if the broker can't
			// enumerate (keeps older brokers working); a hard failure fails the connect.
			log.Printf("substrate connect %s: sweep prior org-node entries: %v (falling back to bootstrap-only)", normOrg, derr)
			if agentID, berr := spiffe.BuildSubstrateAgentPath(api.trustDomain, normOrg); berr == nil {
				if berr := entries.Broker.DeleteEntry(ctx, agentID); berr != nil {
					log.Printf("substrate connect %s: clear prior bootstrap entry: %v", normOrg, berr)
					http.Error(w, "failed to provision substrate identity", http.StatusBadGateway)
					return
				}
			}
		}
		// Node-side rotate hygiene: evict the org's SUPERSEDED x509pop nodes (every
		// attested node under the org prefix but the one this new leaf will attest
		// as). Their entries are gone above, so a stale node issues nothing — but it
		// is live attestation state for a replaced credential, and without this an
		// org accumulates one attested node per connect. Best-effort: a broker that
		// cannot enumerate agents, or a transient eviction failure, must not fail a
		// connect whose security-relevant work (the entry sweep) already succeeded.
		if n, eerr := entries.EvictSupersededOrgNodes(ctx, normOrg, issued.NodeID); eerr != nil {
			log.Printf("substrate connect %s: evict superseded org nodes: %v (continuing)", normOrg, eerr)
		} else {
			log.Printf("substrate connect %s: evicted %d superseded x509pop node(s)", normOrg, n)
		}
		selectors := []string{"k8s:ns:" + api.agentNamespace, "k8s:sa:" + api.agentSA}
		if err := entries.ProvisionSubstrateAgentEntry(ctx, normOrg, issued.NodeID, selectors); err != nil {
			log.Printf("substrate connect %s: provision bootstrap entry: %v", normOrg, err)
			http.Error(w, "failed to provision substrate identity", http.StatusBadGateway)
			return
		}
	} else {
		log.Printf("substrate connect %s: no entry broker configured — bootstrap entry deferred to Phase-4 reconcile", normOrg)
	}

	log.Printf("substrate connect %s: issued identity (fp=%s serial=%s node=%s)", normOrg, issued.CertFingerprint, issued.CertSerial, issued.NodeID)
	install := map[string]string{
		"namespace":      api.agentNamespace,
		"serviceAccount": api.agentSA,
		"note":           "Install the substrate bundle (deploy/substrate) with this identity as the SPIRE x509pop credential. The identity is shown ONCE — store it as a Secret in your cluster; Spawnly retains only its fingerprint.",
	}
	// Real-endpoint hints (edge only — see the field docs). Absent when unset so
	// the local-kind response is unchanged byte-for-byte.
	if api.spireEndpointHint != "" {
		install["spireServerEndpoint"] = api.spireEndpointHint
	}
	if api.publicURLHint != "" {
		install["substrateURL"] = api.publicURLHint
	}
	w.Header().Set("Content-Type", "application/json")
	// One-time secret material — never cache it at any hop.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(connectResponse{
		OrgID:           normOrg,
		TrustDomain:     api.trustDomain,
		Status:          registry.SubstratePending,
		CertFingerprint: issued.CertFingerprint,
		CertSerial:      issued.CertSerial,
		NodeID:          issued.NodeID,
		ExpiresAt:       issued.ExpiresAt,
		Identity: connectIdentity{
			CertPEM:        string(issued.Leaf.CertPEM),
			KeyPEM:         string(issued.Leaf.KeyPEM),
			TrustBundlePEM: string(api.issuer.TrustBundlePEM()),
		},
		Install: install,
	})
}

// status returns the org's substrate record (pending/active + fingerprint), or
// 404 when no substrate is connected. Members may read it (requireOrgMember).
func (api *substrateAPI) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID, err := devca.NormalizeOrgID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid org id", http.StatusBadRequest)
		return
	}
	org, err := fetchOrg(ctx, api.registryURL, api.cpBearer, orgID)
	if err != nil {
		log.Printf("substrate status %s: fetch org: %v", orgID, err)
		http.Error(w, "registry unavailable", http.StatusBadGateway)
		return
	}
	if org.Substrate == nil {
		http.Error(w, "no substrate connected", http.StatusNotFound)
		return
	}
	// Liveness (remote-substrate Phase 5) is layered on from the orchestrator's
	// in-memory channel state — NOT persisted on the record. `online` is true iff
	// the org currently holds a live dispatch channel; a disconnect flips it back
	// without ever mutating org.Substrate (spec AC 8). The record's `active` status
	// (the routing latch) persists across a disconnect; `online` is the separate
	// "is it reachable right now" signal the dashboard renders as online/offline.
	resp := substrateStatusResponse{Substrate: org.Substrate}
	if api.hub != nil {
		online, lastSeen := api.hub.liveness(orgID)
		resp.Online = online
		if !lastSeen.IsZero() {
			resp.LastSeen = &lastSeen
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// substrateStatusResponse is the GET status payload: the persisted substrate
// record (fields flattened via the embedded pointer) plus the live-only liveness
// signal. Online/LastSeen come from the in-memory channel hub, so they reflect
// "reachable right now", independent of the record's persisted status.
type substrateStatusResponse struct {
	*registry.Substrate
	Online   bool       `json:"online"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// disconnect clears the org's substrate record (revoke) and best-effort removes
// the substrate-agent bootstrap entry.
func (api *substrateAPI) disconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := r.PathValue("id")
	normOrg, err := devca.NormalizeOrgID(orgID)
	if err != nil {
		http.Error(w, "invalid org id", http.StatusBadRequest)
		return
	}
	if err := api.deleteSubstrateRecord(ctx, normOrg); err != nil {
		log.Printf("substrate disconnect %s: delete record: %v", normOrg, err)
		http.Error(w, "failed to disconnect substrate", http.StatusBadGateway)
		return
	}
	// Best-effort: sweep EVERY entry under the org's x509pop node (the bootstrap
	// entry AND any workload entries) so a disconnected org can't keep issuing
	// SVIDs. A failure here is logged, not fatal (the record — the authoritative
	// gate — is already gone; a stale entry issues an SVID that authenticates
	// nothing without a record). Falls back to bootstrap-only if enumeration is
	// unsupported.
	if entries := api.entrySvc(); entries != nil {
		if derr := entries.InvalidateOrgNodeEntries(ctx, normOrg); derr != nil {
			log.Printf("substrate disconnect %s: sweep org-node entries: %v", normOrg, derr)
			if id, berr := spiffe.BuildSubstrateAgentPath(api.trustDomain, normOrg); berr == nil {
				if berr := entries.Broker.DeleteEntry(ctx, id); berr != nil {
					log.Printf("substrate disconnect %s: delete bootstrap entry: %v", normOrg, berr)
				}
			}
		}
		// And evict the org's attested x509pop nodes — ALL of them (no keep): unlike
		// a rotate, a disconnect revokes the credential outright, so no node of this
		// org should remain attested. Same best-effort stance as the entry sweep.
		if n, eerr := entries.EvictSupersededOrgNodes(ctx, normOrg, ""); eerr != nil {
			log.Printf("substrate disconnect %s: evict org nodes: %v", normOrg, eerr)
		} else {
			log.Printf("substrate disconnect %s: evicted %d x509pop node(s)", normOrg, n)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// createEntry is the CP-mediated Entry API: authenticate the caller's substrate
// SVID, then authorize+create the requested workload entry under the caller's own
// org (every field validated in substrate.EntryService — fail closed).
func (api *substrateAPI) createEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerOrg, entries, ok := api.authEntryCaller(w, r)
	if !ok {
		return
	}
	var body struct {
		SPIFFEID  string   `json:"spiffeId"`
		ParentID  string   `json:"parentId"`
		Selectors []string `json:"selectors"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	spec := substrate.EntrySpec{SPIFFEID: body.SPIFFEID, ParentID: body.ParentID, Selectors: body.Selectors}
	if err := entries.CreateWorkloadEntry(ctx, callerOrg, spec); err != nil {
		api.writeEntryError(w, "create", callerOrg, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// deleteEntry tears down a workload entry the caller owns (same org isolation).
func (api *substrateAPI) deleteEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	callerOrg, entries, ok := api.authEntryCaller(w, r)
	if !ok {
		return
	}
	spiffeID := r.URL.Query().Get("spiffeId")
	if spiffeID == "" {
		http.Error(w, "spiffeId is required", http.StatusBadRequest)
		return
	}
	if err := entries.DeleteWorkloadEntry(ctx, callerOrg, spiffeID); err != nil {
		api.writeEntryError(w, "delete", callerOrg, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authEntryCaller checks the Entry API is configured, authenticates the caller's
// substrate SVID, and confirms the caller org still has a CONNECTED substrate
// record — the authoritative gate. On any failure it writes the response and
// returns ok=false.
func (api *substrateAPI) authEntryCaller(w http.ResponseWriter, r *http.Request) (string, *substrate.EntryService, bool) {
	// Read both halves ONCE, under the lock, and hand the service back to the
	// caller: on the edge they are published late (initEntryAPIWithRetry), so
	// re-reading api.entries afterwards would be an unsynchronized read.
	entries, auth := api.entrySvc(), api.svidAuthenticator()
	if entries == nil || auth == nil {
		http.Error(w, "substrate entry API is not configured", http.StatusServiceUnavailable)
		return "", nil, false
	}
	callerOrg, err := auth.authenticate(r.Context(), r)
	if err != nil {
		// Oracle-free: a bad/absent/non-substrate SVID is one 401.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", nil, false
	}
	// The substrate record is the authoritative gate: a DISCONNECTED org (record
	// cleared) must not be able to author entries even if a stale bootstrap SVID
	// still attests. Fetch the org over the control-plane bearer and require a
	// substrate record. A registry outage fails CLOSED (502) — never fail-open.
	org, ferr := fetchOrg(r.Context(), api.registryURL, api.cpBearer, callerOrg)
	if ferr != nil {
		log.Printf("substrate entry (org=%s): fetch org: %v", callerOrg, ferr)
		http.Error(w, "registry unavailable", http.StatusBadGateway)
		return "", nil, false
	}
	if org.Substrate == nil {
		log.Printf("substrate entry (org=%s): no connected substrate — refusing", callerOrg)
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", nil, false
	}
	return callerOrg, entries, true
}

// writeEntryError renders an EntryService error: an authorization failure is an
// oracle-free 403; a resolver/broker outage is a 502. Details are logged only.
func (api *substrateAPI) writeEntryError(w http.ResponseWriter, op, callerOrg string, err error) {
	if errors.Is(err, substrate.ErrEntryUnauthorized) {
		log.Printf("substrate entry %s (org=%s): unauthorized", op, callerOrg)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	log.Printf("substrate entry %s (org=%s): %v", op, callerOrg, err)
	http.Error(w, "entry operation failed", http.StatusBadGateway)
}

// ── registry substrate record helpers (over the control-plane bearer) ──────────

func (api *substrateAPI) putSubstrateRecord(ctx context.Context, orgID, fingerprint, serial string) error {
	payload, _ := json.Marshal(map[string]string{"certFingerprint": fingerprint, "certSerial": serial})
	req, err := http.NewRequestWithContext(ctx, "PUT", api.registryURL+"/v1/orgs/"+orgID+"/substrate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := api.cpBearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry PUT substrate returned %d", resp.StatusCode)
	}
	return nil
}

func (api *substrateAPI) deleteSubstrateRecord(ctx context.Context, orgID string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", api.registryURL+"/v1/orgs/"+orgID+"/substrate", nil)
	if err != nil {
		return err
	}
	if t := api.cpBearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("registry DELETE substrate returned %d", resp.StatusCode)
	}
	return nil
}

// ── substrate SVID authenticator (JWKS-backed) ─────────────────────────────────

// spiffeSubstrateAuth authenticates an Entry API caller by validating its
// JWT-SVID against the SPIRE OIDC JWKS and requiring it to be the well-known
// substrate-agent identity for some org — trust-domain-pinned, org surfaced from
// the signed path. A workload SVID (not SubstrateAgent) is refused, so only the
// substrate-agent may drive the Entry API (least privilege).
type spiffeSubstrateAuth struct {
	validator   spiffe.SVIDValidator
	trustDomain string
	audience    string
}

func (a *spiffeSubstrateAuth) authenticate(ctx context.Context, r *http.Request) (string, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return "", errors.New("missing SVID")
	}
	spiffeID, err := a.validator.Validate(ctx, tok, a.audience)
	if err != nil {
		return "", err
	}
	parsed, err := spiffe.ParsePath(spiffeID)
	if err != nil {
		return "", err
	}
	if parsed.TrustDomain != a.trustDomain {
		return "", errors.New("SVID trust domain not permitted")
	}
	if !parsed.SubstrateAgent || parsed.OrgID == "" {
		return "", errors.New("caller is not a substrate-agent SVID")
	}
	return parsed.OrgID, nil
}

// ── registry record-org resolver (for Entry API authz) ─────────────────────────

// registryRecordOrgResolver resolves an agent record's org via the registry
// client. A backend error is surfaced (fail closed at the authz layer); an
// absent record is (found=false).
type registryRecordOrgResolver struct{ reg registry.Client }

func (r registryRecordOrgResolver) RecordOrg(ctx context.Context, agentID string) (string, bool, error) {
	rec, err := r.reg.GetAgent(ctx, agentID)
	if err != nil {
		// An unknown agent is an expected outcome (found=false), NOT an outage:
		// GetAgent returns ErrAgentNotFound on a 404. Distinguishing them is
		// load-bearing — the EntryService renders "unknown" as the oracle-free
		// 403 refuse and a genuine outage as a 502 (fail closed).
		if errors.Is(err, registry.ErrAgentNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if rec.AgentID == "" {
		return "", false, nil
	}
	return rec.OrgID, true, nil
}

// ── the exec entry broker (delegated-admin spire-server CLI) ───────────────────

// execEntryBroker is the LOCAL-SIM entry broker: it drives SPIRE registration
// entries by invoking the `spire-server entry` CLI as a delegated admin against
// the server's admin socket. It is deliberately behind the substrate.EntryBroker
// seam so the mechanism can be swapped for the SPIRE server entry gRPC admin API
// (the infra-track upgrade) WITHOUT touching the authorization in
// substrate.EntryService — the authz runs entirely before any broker call.
//
// No authorization lives here: by the time a spec reaches this broker it has
// already been validated org-correct. The broker only translates a spec into CLI
// args, so it never needs to understand orgs.
type execEntryBroker struct {
	bin        string
	socketPath string
	// runCmd is the exec seam (overridable in tests); returns combined output.
	runCmd func(ctx context.Context, bin string, args ...string) (string, error)
}

func newExecEntryBroker(bin, socketPath string) *execEntryBroker {
	if bin == "" {
		bin = "spire-server"
	}
	return &execEntryBroker{bin: bin, socketPath: socketPath, runCmd: defaultRunCmd}
}

func defaultRunCmd(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func (b *execEntryBroker) baseArgs() []string {
	if b.socketPath == "" {
		return nil
	}
	return []string{"-socketPath", b.socketPath}
}

func (b *execEntryBroker) CreateEntry(ctx context.Context, spec substrate.EntrySpec) error {
	args := append([]string{"entry", "create"}, b.baseArgs()...)
	args = append(args, "-parentID", spec.ParentID, "-spiffeID", spec.SPIFFEID)
	for _, sel := range spec.Selectors {
		args = append(args, "-selector", sel)
	}
	out, err := b.runCmd(ctx, b.bin, args...)
	if err != nil {
		// Idempotent: tolerate an already-existing identical entry.
		if strings.Contains(strings.ToLower(out), "already exists") {
			return nil
		}
		return fmt.Errorf("spire-server entry create: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (b *execEntryBroker) DeleteEntry(ctx context.Context, spiffeID string) error {
	// Resolve ALL entry ids for this SPIFFE ID, then delete each. One SPIFFE ID can
	// have several entries (e.g. different parents — exactly what a rotate leaves
	// behind), so deleting only the first would strand the rest. A missing entry is
	// a no-op success (idempotent teardown).
	showArgs := append([]string{"entry", "show"}, b.baseArgs()...)
	showArgs = append(showArgs, "-spiffeID", spiffeID)
	out, err := b.runCmd(ctx, b.bin, showArgs...)
	if err != nil {
		return fmt.Errorf("spire-server entry show: %v: %s", err, strings.TrimSpace(out))
	}
	for _, entryID := range parseEntryIDs(out) {
		delArgs := append([]string{"entry", "delete"}, b.baseArgs()...)
		delArgs = append(delArgs, "-entryID", entryID)
		if delOut, derr := b.runCmd(ctx, b.bin, delArgs...); derr != nil {
			if strings.Contains(strings.ToLower(delOut), "not found") {
				continue
			}
			return fmt.Errorf("spire-server entry delete: %v: %s", derr, strings.TrimSpace(delOut))
		}
	}
	return nil
}

// DeleteEntriesUnderParent implements substrate.ParentPrefixDeleter: list ALL
// entries and delete each whose Parent ID begins with parentPrefix. Backs the
// rotate hygiene (EntryService.InvalidateOrgNodeEntries) — sweeping an org's
// stale node entries so a superseded x509pop node can't keep issuing SVIDs. A
// missing entry on delete is a no-op success (idempotent).
func (b *execEntryBroker) DeleteEntriesUnderParent(ctx context.Context, parentPrefix string) error {
	if parentPrefix == "" {
		return fmt.Errorf("empty parent prefix")
	}
	showArgs := append([]string{"entry", "show"}, b.baseArgs()...)
	out, err := b.runCmd(ctx, b.bin, showArgs...)
	if err != nil {
		return fmt.Errorf("spire-server entry show: %v: %s", err, strings.TrimSpace(out))
	}
	for _, e := range parseEntriesWithParent(out) {
		if !strings.HasPrefix(e.parent, parentPrefix) {
			continue
		}
		delArgs := append([]string{"entry", "delete"}, b.baseArgs()...)
		delArgs = append(delArgs, "-entryID", e.id)
		if delOut, derr := b.runCmd(ctx, b.bin, delArgs...); derr != nil {
			if strings.Contains(strings.ToLower(delOut), "not found") {
				continue
			}
			return fmt.Errorf("spire-server entry delete: %v: %s", derr, strings.TrimSpace(delOut))
		}
	}
	return nil
}

// EvictNodesUnderPrefix implements substrate.NodeEvicter: list the attested
// agents (nodes) and evict each whose SPIFFE ID begins with prefix, except
// keepNodeID. Backs the rotate hygiene (EntryService.EvictSupersededOrgNodes) —
// the org's superseded x509pop nodes are attestation state for a leaf that has
// been replaced, and would otherwise accumulate one per connect. The prefix is
// CP-derived from the caller's own org, so this can only reach that org's nodes.
// An already-absent node is a no-op success (idempotent).
func (b *execEntryBroker) EvictNodesUnderPrefix(ctx context.Context, prefix, keepNodeID string) (int, error) {
	if prefix == "" {
		return 0, fmt.Errorf("empty node prefix")
	}
	listArgs := append([]string{"agent", "list"}, b.baseArgs()...)
	out, err := b.runCmd(ctx, b.bin, listArgs...)
	if err != nil {
		return 0, fmt.Errorf("spire-server agent list: %v: %s", err, strings.TrimSpace(out))
	}
	evicted := 0
	for _, id := range parseAgentSpiffeIDs(out) {
		if id == keepNodeID || !strings.HasPrefix(id, prefix) {
			continue
		}
		evictArgs := append([]string{"agent", "evict"}, b.baseArgs()...)
		evictArgs = append(evictArgs, "-spiffeID", id)
		if evictOut, eerr := b.runCmd(ctx, b.bin, evictArgs...); eerr != nil {
			if strings.Contains(strings.ToLower(evictOut), "not found") {
				continue
			}
			return evicted, fmt.Errorf("spire-server agent evict: %v: %s", eerr, strings.TrimSpace(evictOut))
		}
		evicted++
	}
	return evicted, nil
}

// parseAgentSpiffeIDs extracts every "SPIFFE ID : <id>" from
// `spire-server agent list` output (one block per attested node).
func parseAgentSpiffeIDs(out string) []string {
	var ids []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "SPIFFE ID") {
			if id := valueAfterColon(line); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// parsedEntry is one entry's id + parent id from `spire-server entry show`.
type parsedEntry struct{ id, parent string }

// parseEntriesWithParent extracts (Entry ID, Parent ID) pairs from
// `spire-server entry show` output. Each entry block starts with "Entry ID" and
// carries a "Parent ID"; the two are paired within a block.
func parseEntriesWithParent(out string) []parsedEntry {
	var entries []parsedEntry
	var cur parsedEntry
	flush := func() {
		if cur.id != "" {
			entries = append(entries, cur)
		}
		cur = parsedEntry{}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Entry ID"):
			flush()
			cur.id = valueAfterColon(line)
		case strings.HasPrefix(line, "Parent ID"):
			cur.parent = valueAfterColon(line)
		}
	}
	flush()
	return entries
}

func valueAfterColon(line string) string {
	if i := strings.Index(line, ":"); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return ""
}

// parseEntryIDs extracts every "Entry ID : <id>" from `spire-server entry show`
// output.
func parseEntryIDs(out string) []string {
	var ids []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Entry ID") {
			if i := strings.Index(line, ":"); i >= 0 {
				if id := strings.TrimSpace(line[i+1:]); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// ── construction ──────────────────────────────────────────────────────────────

// newSubstrateAPI builds the substrate surface from env. It is fail-safe: any
// component that is not configured (no dev CA mounted, no entry broker) is left
// nil and its routes return 503 rather than behaving insecurely. Wiring the real
// broker + JWKS auth lands with the dispatch channel (Phase 4).
func newSubstrateAPI(reg registry.Client, registryURL string, cpBearer func() string, hub *substrateHub) (*substrateAPI, error) {
	td := getEnv("SPIRE_TRUST_DOMAIN", "cluster.local")
	api := &substrateAPI{
		registryURL:    registryURL,
		cpBearer:       cpBearer,
		trustDomain:    td,
		reg:            reg,
		hub:            hub,
		agentNamespace: getEnv("SUBSTRATE_AGENT_NAMESPACE", "substrate-sim"),
		agentSA:        getEnv("SUBSTRATE_AGENT_SA", "substrate-agent"),
		leafTTL:        90 * 24 * time.Hour,
		// Optional real-endpoint install hints (edge overlay only; see the field
		// docs). Unset ⇒ empty ⇒ the Install keys are absent (kind unchanged).
		spireEndpointHint: os.Getenv("SUBSTRATE_SPIRE_ENDPOINT_HINT"),
		publicURLHint:     os.Getenv("SUBSTRATE_PUBLIC_URL_HINT"),
	}

	// Select the CA behind the LeafIssuer seam. Exactly one CA may be configured:
	//   - SUBSTRATE_KMS_KEY_ARN  ⇒ the edge KMS CA (CA cert-only, no key file), or
	//   - SUBSTRATE_CA_KEY_FILE  ⇒ the LOCAL-SIM dev CA (cert + key), unchanged.
	// Setting BOTH is a HARD startup error (no ambiguity about which CA is live).
	// Any misconfig within a selected path leaves issuer nil ⇒ connect 503
	// (fail-safe), never a weaker/insecure CA.
	kmsARN := os.Getenv("SUBSTRATE_KMS_KEY_ARN")
	caCertFile := os.Getenv("SUBSTRATE_CA_CERT_FILE")
	caKeyFile := os.Getenv("SUBSTRATE_CA_KEY_FILE")
	switch {
	case kmsARN != "" && caKeyFile != "":
		return nil, fmt.Errorf("substrate: both SUBSTRATE_KMS_KEY_ARN and SUBSTRATE_CA_KEY_FILE are set — configure exactly one CA")
	case kmsARN != "":
		// Edge KMS CA: the CA cert is public (cert-only mount); the private key is
		// in KMS. A missing/unreadable cert or a mismatched key leaves issuer nil.
		if caCertFile == "" {
			log.Printf("substrate: SUBSTRATE_KMS_KEY_ARN set but no SUBSTRATE_CA_CERT_FILE — connect disabled")
		} else if certPEM, cerr := os.ReadFile(caCertFile); cerr != nil {
			log.Printf("substrate: KMS CA cert unreadable (%v) — connect disabled", cerr)
		} else if issuer, ierr := kmsIssuerFromEnv(context.Background(), kmsARN, certPEM, td, api.leafTTL); ierr != nil {
			log.Printf("substrate: KMS issuer init failed (%v) — connect disabled", ierr)
		} else {
			api.issuer = issuer
			log.Printf("substrate: KMS CA loaded — connect enabled (trust domain %s)", td)
		}
	case caCertFile != "" && caKeyFile != "":
		// Dev CA (mounted from the substrate-dev-ca Secret) → issuer. Unchanged.
		certPEM, cerr := os.ReadFile(caCertFile)
		keyPEM, kerr := os.ReadFile(caKeyFile)
		if cerr != nil || kerr != nil {
			log.Printf("substrate: dev CA files unreadable (%v/%v) — connect disabled", cerr, kerr)
		} else if ca, lerr := devca.LoadCA(certPEM, keyPEM); lerr != nil {
			log.Printf("substrate: dev CA load failed (%v) — connect disabled", lerr)
		} else if issuer, ierr := substrate.NewIssuer(ca, td, api.leafTTL); ierr != nil {
			log.Printf("substrate: issuer init failed (%v) — connect disabled", ierr)
		} else {
			api.issuer = issuer
			log.Printf("substrate: dev CA loaded — connect enabled (trust domain %s)", td)
		}
	default:
		log.Printf("substrate: no CA configured (SUBSTRATE_KMS_KEY_ARN or SUBSTRATE_CA_CERT_FILE/KEY_FILE) — connect disabled")
	}

	// Select the entry broker behind the substrate.EntryBroker seam. Exactly one
	// mechanism may be configured:
	//   - SPIRE_ENTRY_API_ADDR ⇒ the EDGE gRPC broker (SPIRE entry/agent admin API
	//     over mTLS with the orchestrator's own SVID), or
	//   - SPIRE_SERVER_SOCKET  ⇒ the LOCAL-SIM delegated-admin CLI broker, unchanged.
	// Setting BOTH is a HARD startup error (mirrors the CA both-set stance — no
	// ambiguity about which mechanism is live). A misconfig within a selected path
	// leaves the broker nil ⇒ Entry API 503 (fail-safe), never an insecure fallback.
	// Absent ⇒ Entry API unavailable (503) and connect defers the bootstrap entry.
	entryAddr := os.Getenv("SPIRE_ENTRY_API_ADDR")
	sock := os.Getenv("SPIRE_SERVER_SOCKET")
	var broker substrate.EntryBroker
	switch {
	case entryAddr != "" && sock != "":
		return nil, fmt.Errorf("substrate: both SPIRE_ENTRY_API_ADDR and SPIRE_SERVER_SOCKET are set — configure exactly one entry broker")
	case entryAddr != "":
		// EDGE: establish both halves in the BACKGROUND with retry. The orchestrator's
		// own SVID is frequently not deliverable in the first seconds after a rollout
		// (see initGRPCBrokerWithRetry), and the SPIRE OIDC discovery provider may not
		// be serving yet. Until they converge the Entry API answers 503 exactly as an
		// unconfigured broker would, then enables itself with no restart. `broker`
		// stays nil here on purpose — the sync path below is the LOCAL-SIM exec broker.
		log.Printf("substrate: gRPC entry broker configured (SPIRE entry API %s) — establishing in the background", entryAddr)
		initCtx, cancelInit := context.WithCancel(context.Background())
		api.initCancel = cancelInit
		api.initWG.Add(1)
		go func() {
			defer api.initWG.Done()
			api.initGRPCBrokerWithRetry(initCtx, entryAddr, os.Getenv("SPIRE_WORKLOAD_API_SOCKET"), td)
		}()
		if jwksURL := os.Getenv("SPIRE_JWKS_URL"); jwksURL != "" {
			api.initWG.Add(1)
			go func() { defer api.initWG.Done(); api.initSVIDAuthWithRetry(initCtx, jwksURL, td) }()
		} else {
			log.Printf("substrate: SPIRE_JWKS_URL is not set — Entry API stays disabled")
		}
	case sock != "":
		broker = newExecEntryBroker(os.Getenv("SPIRE_SERVER_BIN"), sock)
		log.Printf("substrate: exec entry broker configured (delegated-admin socket %s)", sock)
	default:
		log.Printf("substrate: no entry broker configured (SPIRE_ENTRY_API_ADDR or SPIRE_SERVER_SOCKET) — Entry API disabled")
	}

	// LOCAL-SIM (exec) path stays fully synchronous — the delegated-admin socket is
	// a local file and the JWKS endpoint is in-cluster, so there is no startup race
	// to absorb and kind behavior is unchanged.
	if broker != nil {
		api.entries = &substrate.EntryService{
			Broker:      broker,
			Resolver:    registryRecordOrgResolver{reg: reg},
			TrustDomain: td,
		}
		if jwksURL := os.Getenv("SPIRE_JWKS_URL"); jwksURL != "" {
			if validator, verr := spiffe.NewJWKSValidator(context.Background(), jwksURL); verr != nil {
				log.Printf("substrate: SVID JWKS validator init failed (%v) — Entry API disabled", verr)
			} else {
				api.svidAuth = &spiffeSubstrateAuth{
					validator:   validator,
					trustDomain: td,
					audience:    getEnv("SUBSTRATE_SVID_AUDIENCE", "orchestrator"),
				}
				log.Printf("substrate: Entry API enabled")
			}
		}
	}

	return api, nil
}

// entrySvc returns the Entry API service, or nil when the entry broker is not (yet)
// wired. Callers MUST use this rather than reading api.entries directly: on the edge
// the service is published late, from the background initializer.
func (api *substrateAPI) entrySvc() *substrate.EntryService {
	api.entriesMu.RLock()
	defer api.entriesMu.RUnlock()
	return api.entries
}

// svidAuthenticator returns the Entry API's SVID authenticator, or nil when not
// (yet) wired. Same late-publish rule as entrySvc.
func (api *substrateAPI) svidAuthenticator() substrateSVIDAuthenticator {
	api.entriesMu.RLock()
	defer api.entriesMu.RUnlock()
	return api.svidAuth
}

// stopInit cancels the background edge initializers and waits for them to exit.
// Safe to call when none were started. Callers that replace the package-level
// seams (tests) MUST call this before restoring them.
func (api *substrateAPI) stopInit() {
	if api.initCancel != nil {
		api.initCancel()
	}
	api.initWG.Wait()
}

// setEntrySvc / setSVIDAuth publish the two halves of the Entry API INDEPENDENTLY.
// They are deliberately not coupled: entries also backs connect/disconnect entry
// sweeps and bootstrap provisioning, which need no SVID authenticator, so a JWKS
// problem must not withhold them (authEntryCaller still requires both, so the Entry
// API itself stays 503 until each half is up).
func (api *substrateAPI) setEntrySvc(es *substrate.EntryService) {
	api.entriesMu.Lock()
	defer api.entriesMu.Unlock()
	api.entries = es
}

func (api *substrateAPI) setSVIDAuth(auth substrateSVIDAuthenticator) {
	api.entriesMu.Lock()
	defer api.entriesMu.Unlock()
	api.svidAuth = auth
}

// entryAPIRetryInterval paces the background edge initializers. Package var so the
// unit tests can drive them fast.
var entryAPIRetryInterval = 10 * time.Second

// initGRPCBrokerWithRetry establishes the EDGE gRPC entry broker in the background,
// retrying until it succeeds or ctx is cancelled.
//
// Why retry rather than fail once: the orchestrator authenticates to the SPIRE
// entry/agent admin API with its OWN SVID, which arrives over the Workload API only
// after the spire-controller-manager has minted THIS POD's entry and the node's
// SPIRE agent has synced it. That is inherently racy at startup and re-runs on every
// rollout (the entry carries a k8s:pod-uid selector, so a new pod needs a new
// entry). A one-shot init turned a few seconds of unlucky timing into an Entry API
// that stayed 503 until a human restarted the pod. Failing CLOSED is right; failing
// closed FOREVER is not — the surface stays 503 while this converges and enables
// itself the moment the SVID lands. No insecure fallback is ever taken.
func (api *substrateAPI) initGRPCBrokerWithRetry(ctx context.Context, entryAddr, workloadSocket, td string) {
	for attempt := 1; ; attempt++ {
		broker, err := spireBrokerFromEnv(ctx, entryAddr, workloadSocket, td)
		if err == nil {
			api.setEntrySvc(&substrate.EntryService{
				Broker:      broker,
				Resolver:    registryRecordOrgResolver{reg: api.reg},
				TrustDomain: td,
			})
			log.Printf("substrate: gRPC entry broker enabled (SPIRE entry API %s, attempt %d)", entryAddr, attempt)
			return
		}
		log.Printf("substrate: gRPC entry broker init attempt %d failed (%v) — Entry API 503, retrying in %s", attempt, err, entryAPIRetryInterval)
		select {
		case <-ctx.Done():
			log.Printf("substrate: gRPC entry broker initializer stopped (%v)", ctx.Err())
			return
		case <-time.After(entryAPIRetryInterval):
		}
	}
}

// initSVIDAuthWithRetry establishes the Entry API's SVID authenticator in the
// background. Same rationale as the broker: the JWKS endpoint is the SPIRE OIDC
// discovery provider, which may not be serving yet when the orchestrator starts.
func (api *substrateAPI) initSVIDAuthWithRetry(ctx context.Context, jwksURL, td string) {
	for attempt := 1; ; attempt++ {
		validator, err := spiffe.NewJWKSValidator(ctx, jwksURL)
		if err == nil {
			api.setSVIDAuth(&spiffeSubstrateAuth{
				validator:   validator,
				trustDomain: td,
				audience:    getEnv("SUBSTRATE_SVID_AUDIENCE", "orchestrator"),
			})
			log.Printf("substrate: SVID JWKS validator enabled (%s, attempt %d)", jwksURL, attempt)
			return
		}
		log.Printf("substrate: SVID JWKS validator init attempt %d failed (%v) — Entry API 503, retrying in %s", attempt, err, entryAPIRetryInterval)
		select {
		case <-ctx.Done():
			log.Printf("substrate: SVID JWKS validator initializer stopped (%v)", ctx.Err())
			return
		case <-time.After(entryAPIRetryInterval):
		}
	}
}

// spireBrokerFromEnv builds the edge gRPC EntryBroker against the SPIRE server's
// entry/agent admin API. It is a package var so the broker-selection unit test can
// exercise the env matrix with a fake — the real body constructs the mTLS gRPC
// broker from the orchestrator's own SVID (SPIFFE Workload API) and authorizes the
// SPIRE server as the mTLS peer. The dial is bounded so an unreachable Workload API
// fails closed at startup (nil broker ⇒ Entry API 503) rather than hanging the
// orchestrator; Phase 4 provides the orchestrator its SVID (CSI-mounted socket) and
// pins its SPIFFE id in the server admin_ids.
var spireBrokerFromEnv = func(ctx context.Context, entryAddr, workloadSocket, trustDomain string) (substrate.EntryBroker, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return spirebroker.New(dialCtx, spirebroker.Config{
		Addr:              entryAddr,
		WorkloadAPISocket: workloadSocket,
		TrustDomain:       trustDomain,
	})
}

// kmsIssuerFromEnv builds the edge KMS-backed LeafIssuer. It is a package var so
// the issuer-selection unit test can exercise the env matrix with a fake — the
// real body constructs a KMS client from the default credential chain (EKS Pod
// Identity) and hands it to the AWS-blind seam. Any error (unreachable KMS, a CA
// cert whose SPKI doesn't match the KMS key) is returned so the selector fails
// closed (nil issuer ⇒ connect 503), never falling back to a weaker CA.
var kmsIssuerFromEnv = func(ctx context.Context, keyARN string, caCertPEM []byte, trustDomain string, leafTTL time.Duration) (substrate.LeafIssuer, error) {
	api, err := kmsca.NewSigningClient(ctx)
	if err != nil {
		return nil, err
	}
	return kmsca.NewIssuer(ctx, api, keyARN, caCertPEM, trustDomain, leafTTL)
}
