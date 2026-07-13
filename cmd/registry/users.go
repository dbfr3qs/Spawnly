package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/spawnly/platform/internal/orgdirectory"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// registerUserRoutes wires the master-admin USER lifecycle control-plane API
// (signup-lifecycle Phase 3) onto mux behind the control-plane gate (cp), just
// like the org handlers. The registry is the single dual-write choke point over
// Keycloak (the identity/enabled state, via dir) and SpiceDB/store (agent
// authorization) — the orchestrator/dashboard only forward here, and every route
// is additionally requireAdmin-gated at the orchestrator + BFF (defense in
// depth). masterAdmin is the platform master-admin username the root lockout
// guard refuses to disable/delete (env MASTER_ADMIN_USERNAME, default "root").
func registerUserRoutes(mux *http.ServeMux, s registry.Store, sdb spicedb.Client, dir orgdirectory.Directory, masterAdmin string, cp func(http.HandlerFunc) http.HandlerFunc) {
	// GET /v1/users — the flat platform user directory (username, email, enabled,
	// disabledByOrg). Membership/role are NOT resolved here; this is the master
	// admin's global account list.
	mux.HandleFunc("GET /v1/users", cp(func(w http.ResponseWriter, r *http.Request) {
		users, err := dir.ListUsers(r.Context())
		if err != nil {
			log.Printf("list users: %v", err)
			http.Error(w, "user list failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, users)
	}))

	// POST /v1/users/{username}/disable — block login + revoke sessions, and
	// (when suspendAgents) suspend the user's agents via the revoke cascade. Direct
	// admin ban, so DisableUser is called with byOrg="" — it sets the separate
	// disabledByAdmin marker (NOT the org-suspension set) so an org-enable can never
	// launder the platform ban away. The root lockout guard refuses to disable the
	// master-admin account.
	mux.HandleFunc("POST /v1/users/{username}/disable", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		username := r.PathValue("username")
		if !registry.ValidUserID(username) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		if isMasterAdmin(masterAdmin, username) {
			http.Error(w, "the master-admin account cannot be disabled", http.StatusForbidden)
			return
		}
		var body struct {
			SuspendAgents bool `json:"suspendAgents"`
		}
		// Body is OPTIONAL (a bare disable). An empty body (EOF) defaults
		// suspendAgents=false; a present-but-malformed body is a 400.
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := dir.DisableUser(ctx, username, ""); err != nil {
			log.Printf("disable user %s: %v", username, err)
			http.Error(w, "disable failed", http.StatusInternalServerError)
			return
		}
		if body.SuspendAgents {
			if err := suspendUserAgents(ctx, s, sdb, username); err != nil {
				log.Printf("disable user %s: suspend agents: %v", username, err)
				http.Error(w, "disable failed", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// POST /v1/users/{username}/enable — lift the direct admin ban. Agents suspended
	// at disable time are DELIBERATELY not resumed (asymmetric by design — the
	// admin/user resumes them manually). onlyIfDisabledByOrg="" clears the
	// disabledByAdmin marker and restores login ONLY when no org still suspends the
	// account (a user an org also disabled stays disabled until that org enables).
	// Enabling the master admin is harmless, so no root guard here.
	mux.HandleFunc("POST /v1/users/{username}/enable", cp(func(w http.ResponseWriter, r *http.Request) {
		username := r.PathValue("username")
		if !registry.ValidUserID(username) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		if _, err := dir.EnableUser(r.Context(), username, ""); err != nil {
			log.Printf("enable user %s: %v", username, err)
			http.Error(w, "enable failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// DELETE /v1/users/{username} — remove the account. For EVERY org the user
	// belongs to, apply that org's removal disposition to the agents they owned
	// there and tear down the membership (the shared removeMemberFromOrg path), so
	// no org agent is orphaned and no authorization dangles; only then hard-delete
	// the Keycloak identity. The root lockout guard refuses to delete the
	// master-admin account.
	mux.HandleFunc("DELETE /v1/users/{username}", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		username := r.PathValue("username")
		if !registry.ValidUserID(username) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		if isMasterAdmin(masterAdmin, username) {
			http.Error(w, "the master-admin account cannot be deleted", http.StatusForbidden)
			return
		}
		// Settle the user's OWN agents + consents up front so a delete leaves
		// nothing ACTING under the freed username (self-registration is open since
		// Phase 1, so the username can be re-registered):
		//   * ORG-LESS (global) agents authorize by userId EQUALITY, not a SpiceDB
		//     owner tuple (cmd/registry/main.go agentAllowed), so the per-org
		//     disposition below never touches them — revoke them here so they stop
		//     acting. (Org agents' owner tuples ARE dropped by the disposition.)
		//   * CIBA consent grants are keyed on userId; purge them so an inherited
		//     username can't ride the deleted user's auto-approvals.
		// This REDUCES the blast radius rather than fully closing it: because
		// org-less authority is pure userId equality (not status-gated), a
		// re-registered username could still RESUME a revoked org-less agent — that
		// residual only truly closes with immutable subject ids (an out-of-scope
		// follow-up). Both steps run before the membership teardown and are
		// idempotent, so a retry after a partial failure re-converges.
		if err := revokeOwnedRoots(ctx, s, sdb, username, func(a registry.AgentRecord) bool { return a.OrgID == "" }); err != nil {
			log.Printf("delete user %s: revoke org-less agents: %v", username, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		if err := purgeUserConsents(ctx, s, username); err != nil {
			log.Printf("delete user %s: purge consents: %v", username, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		orgs, err := dir.ListUserOrgs(ctx, username)
		if err != nil {
			log.Printf("delete user %s: list orgs: %v", username, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		for _, org := range orgs {
			if err := removeMemberFromOrg(ctx, s, sdb, dir, org, username); err != nil {
				log.Printf("delete user %s: remove from org %s: %v", username, org, err)
				http.Error(w, "delete failed", http.StatusInternalServerError)
				return
			}
		}
		if err := dir.DeleteUser(ctx, username); err != nil {
			log.Printf("delete user %s: %v", username, err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// isMasterAdmin reports whether username is the protected platform master-admin
// account (the root lockout guard). The compare is case-insensitive because
// Keycloak canonicalizes usernames to lowercase while MASTER_ADMIN_USERNAME is a
// free-form env value — a "Root"/"root" mismatch must not silently unprotect the
// real admin. An empty masterAdmin disables the guard (never happens — it
// defaults to "root").
func isMasterAdmin(masterAdmin, username string) bool {
	return masterAdmin != "" && strings.EqualFold(masterAdmin, username)
}

// suspendUserAgents revokes every agent the user OWNS and everything those agents
// spawned — the optional suspend-agents action on a user disable. It spans ALL
// the user's agents regardless of org and does NOT drop owner tuples: a disable
// is reversible (enable restores login and the user resumes their own agents).
func suspendUserAgents(ctx context.Context, s registry.Store, sdb spicedb.Client, user string) error {
	return revokeOwnedRoots(ctx, s, sdb, user, func(registry.AgentRecord) bool { return true })
}

// revokeOwnedRoots runs the shared revoke cascade (revokeCascadeRoots) over every
// ROOT agent the user owns for which include(agent) is true — a root being an
// owned agent whose parent is not ALSO owned by the user (a child of one of their
// own agents is already covered by the parent's cascade; revokeNode is idempotent
// regardless). The scope predicate is ALL the user's agents (owned) so the
// parent-owned skip is correct even when include narrows the starting roots more
// tightly (e.g. org-less-only on delete): the root predicate is owned && include,
// but the skip still sees the full owned set. A backend read/write error is
// returned so the caller fails loud rather than leaving some agents un-revoked.
func revokeOwnedRoots(ctx context.Context, s registry.Store, sdb spicedb.Client, user string, include func(registry.AgentRecord) bool) error {
	owned := func(a registry.AgentRecord) bool { return a.UserID == user }
	root := func(a registry.AgentRecord) bool { return owned(a) && include(a) }
	return revokeCascadeRoots(ctx, s, sdb, owned, root)
}

// purgeUserConsents revokes every CIBA consent grant the user holds, so a deleted
// account leaves no auto-approve grants a re-registered username could inherit.
// Idempotent (RevokeConsent on an already-revoked grant is a no-op success).
func purgeUserConsents(ctx context.Context, s registry.Store, user string) error {
	consents, err := s.ListConsents(ctx, user)
	if err != nil {
		return err
	}
	for _, rec := range consents {
		if _, err := s.RevokeConsent(ctx, rec.ID, user); err != nil {
			return err
		}
	}
	return nil
}
