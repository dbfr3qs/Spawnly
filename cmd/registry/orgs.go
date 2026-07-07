package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/spawnly/platform/internal/orgdirectory"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// validOrgID is the org alias/slug charset: a DNS label (lowercase alphanumerics
// and hyphens, no leading/trailing hyphen, 1–63 chars). This one identifier is
// the Keycloak org alias == registry OrgID == SpiceDB object id, and it is
// constrained now so it doubles as the subdomain slug in the follow-up feature.
// (Reserved-word rejection — www/api/... — lands with self-serve signup in
// Phase 7; platform-admin onboarding here needs only the charset guard.)
var validOrgID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// orgTuple builds the SpiceDB relationship coordinates for an org membership:
// resource organization:<id>, relation member|admin, subject user:<userId>.
//
// Both object ids are normalized with spicedb.EncodeObjectID so they match the
// agent owner/org tuples (cmd/registry/main.go) and the Phase-4 CheckPermission
// subject/resource (agentAllowed, orchestrator canView/canManage) — the same
// encoding on every side. orgID is DNS-label constrained (already safe, so the
// encoding is the identity); userID may carry '. : @' (ValidUserID admits them)
// which SpiceDB's object-id charset rejects raw, so encoding is load-bearing
// there. This is the single chokepoint for every membership tuple.
func orgTuple(orgID, role, userID string) (resource, relation, subject string) {
	return "organization:" + spicedb.EncodeObjectID(orgID), role, "user:" + spicedb.EncodeObjectID(userID)
}

// requireOrg reports whether org id exists, writing the correct error and
// returning false otherwise: 500 on a store failure (never a spurious 404 on
// top), 404 when the org is unknown. Callers `if !requireOrg(...) { return }`.
func requireOrg(w http.ResponseWriter, ctx context.Context, s registry.Store, id string) bool {
	_, ok, err := s.GetOrg(ctx, id)
	if storeErr(w, err) {
		return false
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// registerOrgRoutes wires the org onboarding + membership control-plane API onto
// mux behind the control-plane gate (cp). ALL org/membership mutation flows
// through here so Keycloak (membership, via dir) and SpiceDB (authorization
// tuples) are written together with cleanup on partial failure — org membership
// is NEVER edited directly in the Keycloak admin console (see
// deploy/keycloak/README.md). These endpoints are platform-admin only in Phase 3
// (the orchestrator gates them with requireAdmin); org-admin self-service arrives
// in Phase 6.
func registerOrgRoutes(mux *http.ServeMux, s registry.Store, sdb spicedb.Client, dir orgdirectory.Directory, cp func(http.HandlerFunc) http.HandlerFunc) {
	// POST /v1/orgs — create an org as ONE logical transaction, ordered
	// Keycloak-org -> store PutOrg -> (SpiceDB admin tuple + Keycloak first-admin
	// membership). On any later step failing, the earlier writes are unwound
	// best-effort and a loud 500 is returned. Every step is idempotent
	// (create-if-absent / TOUCH upsert), so a retry after a partial failure
	// converges rather than double-writing.
	mux.HandleFunc("POST /v1/orgs", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var body struct {
			OrgID     string `json:"orgId"`
			Name      string `json:"name"`
			AdminUser string `json:"adminUser"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !validOrgID.MatchString(body.OrgID) {
			http.Error(w, "invalid orgId", http.StatusBadRequest)
			return
		}
		if !registry.ValidUserID(body.AdminUser) {
			http.Error(w, "invalid adminUser", http.StatusBadRequest)
			return
		}

		// Preserve an existing record's CreatedAt so a converging retry doesn't
		// reset it; existed also decides whether an unwind may delete the org.
		prev, existed, err := s.GetOrg(ctx, body.OrgID)
		if storeErr(w, err) {
			return
		}

		// Step 1: Keycloak organization (idempotent).
		if err := dir.CreateOrg(ctx, body.OrgID, body.Name); err != nil {
			log.Printf("org create: keycloak CreateOrg %s failed: %v", body.OrgID, err)
			http.Error(w, "org create failed", http.StatusInternalServerError)
			return
		}

		// Step 2: store record.
		org := registry.Organization{OrgID: body.OrgID, Name: body.Name, CreatedAt: time.Now().UTC()}
		if existed {
			org.CreatedAt = prev.CreatedAt
			org.RemovalDisposition = prev.RemovalDisposition
			org.Anchor = prev.Anchor
		}
		if err := s.PutOrg(ctx, org); err != nil {
			// Unwind step 1 (only if we just created it).
			if !existed {
				if derr := dir.DeleteOrg(context.WithoutCancel(ctx), body.OrgID); derr != nil {
					log.Printf("org create unwind: DeleteOrg %s: %v", body.OrgID, derr)
				}
			}
			storeErr(w, err)
			return
		}

		// Step 3: authorization (SpiceDB admin tuple) + Keycloak first-admin
		// membership. The first admin holds the `admin` relation (which grants
		// both view_agents and admin_agents in the schema), so no separate member
		// tuple is needed.
		res, rel, sub := orgTuple(body.OrgID, orgdirectory.RoleAdmin, body.AdminUser)
		tupleErr := sdb.WriteRelationship(ctx, res, rel, sub)
		var memberErr error
		if tupleErr == nil {
			memberErr = dir.AddMember(ctx, body.OrgID, body.AdminUser)
		}
		if tupleErr != nil || memberErr != nil {
			log.Printf("org create: admin wiring for %s failed (tuple=%v member=%v) — unwinding", body.OrgID, tupleErr, memberErr)
			uctx := context.WithoutCancel(ctx)
			if derr := sdb.DeleteRelationship(uctx, res, rel, sub); derr != nil {
				log.Printf("org create unwind: delete admin tuple: %v", derr)
			}
			if derr := dir.RemoveMember(uctx, body.OrgID, body.AdminUser); derr != nil {
				log.Printf("org create unwind: RemoveMember: %v", derr)
			}
			if !existed {
				if _, derr := s.DeleteOrg(uctx, body.OrgID); derr != nil {
					log.Printf("org create unwind: store DeleteOrg: %v", derr)
				}
				if derr := dir.DeleteOrg(uctx, body.OrgID); derr != nil {
					log.Printf("org create unwind: keycloak DeleteOrg: %v", derr)
				}
			}
			http.Error(w, "org create failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if existed {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		json.NewEncoder(w).Encode(org)
	}))

	// GET /v1/orgs — list all orgs, or ?member=<user> to list only the orgs that
	// user belongs to (source: Keycloak membership via dir; the org RECORDS come
	// from the store). Powers the Phase-5 org picker (via the orchestrator's
	// per-caller /v1/my/orgs) and admin listing.
	mux.HandleFunc("GET /v1/orgs", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if member := r.URL.Query().Get("member"); member != "" {
			if !registry.ValidUserID(member) {
				http.Error(w, "invalid member", http.StatusBadRequest)
				return
			}
			aliases, err := dir.ListUserOrgs(ctx, member)
			if err != nil {
				log.Printf("org list by member %s: %v", member, err)
				http.Error(w, "org list failed", http.StatusInternalServerError)
				return
			}
			orgs := make([]registry.Organization, 0, len(aliases))
			for _, a := range aliases {
				org, ok, err := s.GetOrg(ctx, a)
				if storeErr(w, err) {
					return
				}
				if ok {
					orgs = append(orgs, org)
				}
			}
			writeJSON(w, orgs)
			return
		}
		orgs, err := s.ListOrgs(ctx)
		if storeErr(w, err) {
			return
		}
		writeJSON(w, orgs)
	}))

	// GET /v1/orgs/{id} — one org record (settings + anchor).
	mux.HandleFunc("GET /v1/orgs/{id}", cp(func(w http.ResponseWriter, r *http.Request) {
		org, ok, err := s.GetOrg(r.Context(), r.PathValue("id"))
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, org)
	}))

	// PATCH /v1/orgs/{id} — org settings (currently: removal disposition).
	mux.HandleFunc("PATCH /v1/orgs/{id}", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		var body struct {
			RemovalDisposition string `json:"removalDisposition"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.RemovalDisposition != "" &&
			body.RemovalDisposition != registry.RemovalAutoRevoke &&
			body.RemovalDisposition != registry.RemovalOrphanToAdmin {
			http.Error(w, "invalid removalDisposition", http.StatusBadRequest)
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
		org.RemovalDisposition = body.RemovalDisposition
		if err := s.PutOrg(ctx, org); storeErr(w, err) {
			return
		}
		writeJSON(w, org)
	}))

	// DELETE /v1/orgs/{id} — remove an org. Best-effort tuple cleanup (drop each
	// current member's member+admin tuples) precedes the Keycloak + store delete
	// so the org's authorization relations don't dangle. Idempotent per step.
	mux.HandleFunc("DELETE /v1/orgs/{id}", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		_, ok, err := s.GetOrg(ctx, id)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Drop membership tuples for every current member (both relations —
		// TOUCH-delete is idempotent, so deleting a relation the member doesn't
		// hold is harmless). FAIL CLOSED if the member list can't be read: deleting
		// the store record while tuples survive would leave dangling authorization
		// that a later org-id reuse (the id doubles as the SpiceDB object id) would
		// resurrect. A 500 here is retryable and converges.
		members, merr := dir.ListMembers(ctx, id)
		if merr != nil {
			log.Printf("org delete: list members for %s: %v", id, merr)
			http.Error(w, "org delete failed", http.StatusInternalServerError)
			return
		}
		for _, m := range members {
			for _, role := range []string{orgdirectory.RoleMember, orgdirectory.RoleAdmin} {
				res, rel, sub := orgTuple(id, role, m.UserID)
				if derr := sdb.DeleteRelationship(ctx, res, rel, sub); derr != nil {
					log.Printf("org delete: drop %s tuple for %s: %v", role, m.UserID, derr)
				}
			}
		}
		if derr := dir.DeleteOrg(ctx, id); derr != nil {
			log.Printf("org delete: keycloak DeleteOrg %s: %v", id, derr)
			http.Error(w, "org delete failed", http.StatusInternalServerError)
			return
		}
		if _, derr := s.DeleteOrg(ctx, id); storeErr(w, derr) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// GET /v1/orgs/{id}/members — the org's members (from Keycloak). Role is
	// resolved from SpiceDB by the UI in Phase 6; the Keycloak adapter reports
	// membership only.
	mux.HandleFunc("GET /v1/orgs/{id}/members", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		if !requireOrg(w, ctx, s, id) {
			return
		}
		members, err := dir.ListMembers(ctx, id)
		if err != nil {
			log.Printf("org list members %s: %v", id, err)
			http.Error(w, "member list failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, members)
	}))

	// POST /v1/orgs/{id}/members — add a member. Dual-write: Keycloak membership
	// (dir.AddMember, creating an unknown user with a required password reset)
	// then the SpiceDB role tuple. If the tuple write fails, the Keycloak
	// membership is unwound (best-effort) and a loud 500 returned.
	mux.HandleFunc("POST /v1/orgs/{id}/members", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		var body struct {
			User string `json:"user"`
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !registry.ValidUserID(body.User) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		role := body.Role
		if role == "" {
			role = orgdirectory.RoleMember
		}
		if !orgdirectory.ValidRole(role) {
			http.Error(w, "invalid role", http.StatusBadRequest)
			return
		}
		if !requireOrg(w, ctx, s, id) {
			return
		}
		if err := dir.AddMember(ctx, id, body.User); err != nil {
			log.Printf("org add member %s/%s: %v", id, body.User, err)
			http.Error(w, "add member failed", http.StatusInternalServerError)
			return
		}
		res, rel, sub := orgTuple(id, role, body.User)
		if err := sdb.WriteRelationship(ctx, res, rel, sub); err != nil {
			log.Printf("org add member %s/%s: tuple write failed, unwinding: %v", id, body.User, err)
			if derr := dir.RemoveMember(context.WithoutCancel(ctx), id, body.User); derr != nil {
				log.Printf("org add member unwind: RemoveMember: %v", derr)
			}
			http.Error(w, "add member failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))

	// PATCH /v1/orgs/{id}/members/{user} — change a member's role. Writes the new
	// role tuple then drops the other, so the change is atomic in authz terms
	// (a momentary both-roles window only ever GRANTS, never denies).
	mux.HandleFunc("PATCH /v1/orgs/{id}/members/{user}", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, user := r.PathValue("id"), r.PathValue("user")
		var body struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !orgdirectory.ValidRole(body.Role) {
			http.Error(w, "invalid role", http.StatusBadRequest)
			return
		}
		if !registry.ValidUserID(user) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		if !requireOrg(w, ctx, s, id) {
			return
		}
		// A role change must target an existing MEMBER — writing a role tuple for
		// a non-member would grant authorization with no matching Keycloak
		// membership (the desync the directory doc warns against). Confirm
		// membership via the user's own org list (bounded by orgs-per-user).
		userOrgs, err := dir.ListUserOrgs(ctx, user)
		if err != nil {
			log.Printf("org set role %s/%s: membership check: %v", id, user, err)
			http.Error(w, "set role failed", http.StatusInternalServerError)
			return
		}
		if !containsStr(userOrgs, id) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := dir.SetRole(ctx, id, user, body.Role); err != nil {
			log.Printf("org set role %s/%s=%s: %v", id, user, body.Role, err)
			http.Error(w, "set role failed", http.StatusInternalServerError)
			return
		}
		other := orgdirectory.RoleMember
		if body.Role == orgdirectory.RoleMember {
			other = orgdirectory.RoleAdmin
		}
		res, rel, sub := orgTuple(id, body.Role, user)
		if err := sdb.WriteRelationship(ctx, res, rel, sub); err != nil {
			log.Printf("org set role %s/%s: write %s tuple: %v", id, user, body.Role, err)
			http.Error(w, "set role failed", http.StatusInternalServerError)
			return
		}
		ores, orel, osub := orgTuple(id, other, user)
		if err := sdb.DeleteRelationship(ctx, ores, orel, osub); err != nil {
			// The new role is already granted; a stale old-role tuple only ever
			// over-grants and is cleaned on a retry. Log and succeed.
			log.Printf("org set role %s/%s: drop stale %s tuple: %v", id, user, other, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// DELETE /v1/orgs/{id}/members/{user} — remove a member. SpiceDB tuples are
	// dropped FIRST (kills authorization immediately), then the Keycloak
	// membership — so a partial failure fails toward LESS access, and a retry
	// converges. Disposition of the ex-member's running agents (auto-revoke /
	// orphan-to-admin) is wired in Phase 6.
	mux.HandleFunc("DELETE /v1/orgs/{id}/members/{user}", cp(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, user := r.PathValue("id"), r.PathValue("user")
		if !registry.ValidUserID(user) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
		if !requireOrg(w, ctx, s, id) {
			return
		}
		for _, role := range []string{orgdirectory.RoleMember, orgdirectory.RoleAdmin} {
			res, rel, sub := orgTuple(id, role, user)
			if err := sdb.DeleteRelationship(ctx, res, rel, sub); err != nil {
				log.Printf("org remove member %s/%s: drop %s tuple: %v", id, user, role, err)
			}
		}
		if err := dir.RemoveMember(ctx, id, user); err != nil {
			log.Printf("org remove member %s/%s: keycloak RemoveMember: %v", id, user, err)
			http.Error(w, "remove member failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// writeJSON writes v as a JSON response with a 200 status.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// containsStr reports whether xs contains want.
func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
