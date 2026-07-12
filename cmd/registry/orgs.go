package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spawnly/platform/internal/orgdirectory"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// validOrgID is the org alias/slug charset: a DNS label (lowercase alphanumerics
// and hyphens, no leading/trailing hyphen, 1–63 chars). This one identifier is
// the Keycloak org alias == registry OrgID == SpiceDB object id, and it is
// constrained now so it doubles as the subdomain slug in the follow-up feature.
var validOrgID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedOrgIDs are slugs no org may claim (Phase 7). They collide with the
// platform's own subdomains in the deferred subdomain-per-org model (the org
// alias doubles as the subdomain slug), so an org named `auth`/`api`/... could
// otherwise shadow a platform host. Rejected for EVERY caller (platform-admin
// onboarding and self-serve signup alike) — reserving a name is not a privilege
// a platform admin should be able to override into a collision. validOrgID
// already forces lowercase, so a plain lookup is exhaustive.
var reservedOrgIDs = map[string]bool{
	"www": true, "api": true, "auth": true, "admin": true, "mobile": true, "docs": true,
}

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
// maxOrgs is the global self-serve org ceiling (signup-lifecycle Phase 2): 0
// means unlimited; a positive value rejects a self-serve create once the
// platform holds that many orgs OR MORE. The platform-admin path is never gated.
func registerOrgRoutes(mux *http.ServeMux, s registry.Store, sdb spicedb.Client, dir orgdirectory.Directory, apexDomain string, maxOrgs int, cp func(http.HandlerFunc) http.HandlerFunc) {
	// Per-org attestation-anchor lifecycle (Phase 8): PUT/DELETE .../anchor and
	// POST .../anchor/activate. Same org-endpoints family, same cp gate; the
	// anchor is also visible read-only in GET /v1/orgs/{id} below.
	registerAnchorRoutes(mux, s, cp, nil)

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
		body.Name = strings.TrimSpace(body.Name)
		// orgId is OPTIONAL. User-facing creation (self-serve signup, the create-org
		// form) omits it: the registry mints an opaque GUID so a user never has to
		// invent a permanent slug — they supply a display Name (which is what the org
		// switcher shows). With no slug to fall back to, that Name is REQUIRED here.
		// Internal seed/admin tooling MAY still pin a readable, validated orgId for
		// stable references (e.g. the demo acme/globex), in which case Name stays
		// optional (the UI falls back to the id).
		if body.OrgID == "" {
			if body.Name == "" {
				http.Error(w, "organization name is required", http.StatusBadRequest)
				return
			}
			body.OrgID = uuid.NewString()
		} else {
			if !validOrgID.MatchString(body.OrgID) {
				http.Error(w, "invalid orgId", http.StatusBadRequest)
				return
			}
			if reservedOrgIDs[body.OrgID] {
				http.Error(w, "orgId is reserved", http.StatusBadRequest)
				return
			}
		}
		if !registry.ValidUserID(body.AdminUser) {
			http.Error(w, "invalid adminUser", http.StatusBadRequest)
			return
		}

		// Self-serve signup (Phase 7): the orchestrator's requireToken-gated
		// /v1/signup/org sets ?selfServe=1 and makes the CALLER the adminUser. Unlike
		// platform-admin onboarding this path is un-privileged, so it carries two
		// extra guards the admin path deliberately does NOT (an admin may legitimately
		// re-assert an org or run many orgs):
		//   (1) no HIJACK — a self-serve create of an ALREADY-EXISTING slug is refused
		//       (409), never the idempotent add-caller-as-admin below; otherwise a
		//       signup could adopt admin over someone else's org by guessing its slug.
		//   (2) one org per user — a caller who already belongs to any org is refused
		//       (409), the light per-user rate limit the beta calls for.
		// Both are 409 with a clear message (this is a cooperative self-service form,
		// not an oracle-sensitive resource — the caller already authenticated).
		selfServe := r.URL.Query().Get("selfServe") != ""

		// Preserve an existing record's CreatedAt so a converging retry doesn't
		// reset it; existed also decides whether an unwind may delete the org.
		prev, existed, err := s.GetOrg(ctx, body.OrgID)
		if storeErr(w, err) {
			return
		}
		if selfServe {
			// Global self-serve ceiling (signup-lifecycle Phase 2): reject once the
			// platform already holds maxOrgs orgs OR MORE. 0 = unlimited. Only the
			// self-serve path reaches here (the admin onboarding path has
			// selfServe=false), so a master admin's POST /v1/orgs always bypasses the
			// cap. A friendly 403 JSON body (not the plain-text guards below) so the
			// signup UX can surface it distinctly; the count is a live ListOrgs read.
			if maxOrgs > 0 {
				orgs, lerr := s.ListOrgs(ctx)
				if storeErr(w, lerr) {
					return
				}
				if len(orgs) >= maxOrgs {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(w).Encode(map[string]string{
						"error": "organization creation is currently at capacity",
					})
					return
				}
			}
			// NOTE the deliberate asymmetry with the admin path: an admin re-POST of an
			// existing slug idempotently converges (adds the caller as admin), but a
			// self-serve re-POST is a hard 409 — we must never adopt a caller onto an
			// org they didn't create. The rare lost-response retry (registry committed,
			// response dropped) therefore 409s here, but it self-heals: the next
			// dashboard load resolves the now-committed membership via /v1/my/orgs and
			// lands the user in their org rather than back on the create form.
			if existed {
				http.Error(w, "that organization ID is already taken", http.StatusConflict)
				return
			}
			userOrgs, lerr := dir.ListUserOrgs(ctx, body.AdminUser)
			if lerr != nil {
				log.Printf("org self-serve create %s: list caller orgs for %s: %v", body.OrgID, body.AdminUser, lerr)
				http.Error(w, "org create failed", http.StatusInternalServerError)
				return
			}
			if len(userOrgs) > 0 {
				http.Error(w, "you already belong to an organization", http.StatusConflict)
				return
			}
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
		// unwindCreate reverses steps 1–3 best-effort (admin tuple + first-admin
		// membership, and — only when we created it this call — the store record and
		// Keycloak org). Shared by the step-3 and step-4 failure paths; every step is
		// idempotent so a retry converges rather than double-writing.
		unwindCreate := func() {
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
		}

		tupleErr := sdb.WriteRelationship(ctx, res, rel, sub)
		var memberErr error
		if tupleErr == nil {
			memberErr = dir.AddMember(ctx, body.OrgID, body.AdminUser)
		}
		if tupleErr != nil || memberErr != nil {
			log.Printf("org create: admin wiring for %s failed (tuple=%v member=%v) — unwinding", body.OrgID, tupleErr, memberErr)
			unwindCreate()
			http.Error(w, "org create failed", http.StatusInternalServerError)
			return
		}

		// Step 4 (org-subdomains Phase 2): register the org's satellite callback +
		// post-logout origin on the dashboard client so a fresh login at
		// https://<org>.<apex>/ can complete its own OIDC redirect. Feature-gated on
		// ORG_APEX_DOMAIN; the registry OWNS the URL shape (the adapter is host-blind).
		// A failure unwinds steps 1–3 exactly like the admin-wiring failure above.
		if apexDomain != "" {
			redirectURI := "https://" + body.OrgID + "." + apexDomain + "/callback"
			postLogoutURI := "https://" + body.OrgID + "." + apexDomain + "/"
			if err := dir.RegisterOrgRedirectURIs(ctx, redirectURI, postLogoutURI); err != nil {
				log.Printf("org create: register satellite redirect URIs for %s failed: %v — unwinding", body.OrgID, err)
				// The PUT may have LANDED before the response was lost (timeout), so
				// best-effort remove the URIs too — completing the "unwind reverses
				// everything" contract. Idempotent: a no-op when nothing was written.
				if derr := dir.RemoveOrgRedirectURIs(context.WithoutCancel(ctx), redirectURI, postLogoutURI); derr != nil {
					log.Printf("org create unwind: RemoveOrgRedirectURIs: %v", derr)
				}
				unwindCreate()
				http.Error(w, "org create failed", http.StatusInternalServerError)
				return
			}
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
		// Remove the org's satellite redirect URIs from the dashboard client
		// (org-subdomains Phase 2). Best-effort, mirroring the tuple cleanup above:
		// a stale redirect URI for a deleted org is harmless (no org means no
		// satellite to redirect to), so it must not block the delete.
		if apexDomain != "" {
			redirectURI := "https://" + id + "." + apexDomain + "/callback"
			postLogoutURI := "https://" + id + "." + apexDomain + "/"
			if derr := dir.RemoveOrgRedirectURIs(ctx, redirectURI, postLogoutURI); derr != nil {
				log.Printf("org delete: remove satellite redirect URIs for %s: %v", id, derr)
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
		// Enrich each member with their role, which is SpiceDB-authoritative (Phase 3:
		// Keycloak has no native member/admin concept). admin_agents holds iff the
		// member has the `admin` relation; otherwise they are a plain member. A
		// SpiceDB hiccup degrades a single row to "member" (never spuriously grants
		// admin) rather than failing the whole list.
		enc := spicedb.EncodeObjectID
		type memberView struct {
			UserID   string `json:"userId"`
			Username string `json:"username"`
			Email    string `json:"email,omitempty"`
			Role     string `json:"role"`
		}
		out := make([]memberView, 0, len(members))
		for _, m := range members {
			role := orgdirectory.RoleMember
			isAdmin, cerr := sdb.CheckPermission(ctx, "organization:"+enc(id), "admin_agents", "user:"+enc(m.UserID))
			if cerr != nil {
				log.Printf("org list members %s: role check for %s failed (defaulting member): %v", id, m.UserID, cerr)
			} else if isAdmin {
				role = orgdirectory.RoleAdmin
			}
			out = append(out, memberView{UserID: m.UserID, Username: m.Username, Email: m.Email, Role: role})
		}
		writeJSON(w, out)
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
		if err := removeMemberFromOrg(ctx, s, sdb, dir, id, user); err != nil {
			log.Printf("org remove member %s/%s: %v", id, user, err)
			http.Error(w, "remove member failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// removeMemberFromOrg removes user's membership in org id and settles the agents
// they OWNED there per the org's removal disposition — the shared core of the
// DELETE members endpoint and the master-admin user delete (users.go), which
// removes a user from EVERY org they belong to before deleting the identity.
// Reusing one path keeps every membership teardown flowing through the single
// dual-write choke point (SpiceDB tuples + Keycloak membership).
//
// Order: SpiceDB tuples drop FIRST (kills authorization immediately), then the
// Keycloak membership, then the disposition — so a partial failure fails toward
// LESS access and a retry converges (every step is idempotent). A Keycloak or
// store error is returned so the caller fails loud (5xx) rather than proceeding
// on a half-removed member.
func removeMemberFromOrg(ctx context.Context, s registry.Store, sdb spicedb.Client, dir orgdirectory.Directory, id, user string) error {
	for _, role := range []string{orgdirectory.RoleMember, orgdirectory.RoleAdmin} {
		res, rel, sub := orgTuple(id, role, user)
		if err := sdb.DeleteRelationship(ctx, res, rel, sub); err != nil {
			log.Printf("org remove member %s/%s: drop %s tuple: %v", id, user, role, err)
		}
	}
	if err := dir.RemoveMember(ctx, id, user); err != nil {
		return fmt.Errorf("keycloak RemoveMember: %w", err)
	}
	// Removed-member disposition: the ex-member has now lost all access (membership
	// tuples + Keycloak membership gone above). Settle the agents they OWNED in this
	// org per the org's per-org setting — auto-revoke the cascade, or orphan-to-admin
	// (default). A store hiccup fails loud (the whole remove is retryable, every step
	// idempotent).
	disposition := registry.RemovalOrphanToAdmin
	if org, ok, gerr := s.GetOrg(ctx, id); gerr != nil {
		return fmt.Errorf("load org for disposition: %w", gerr)
	} else if ok && org.RemovalDisposition != "" {
		disposition = org.RemovalDisposition
	}
	if err := applyRemovalDisposition(ctx, s, sdb, id, user, disposition); err != nil {
		return fmt.Errorf("apply disposition %s: %w", disposition, err)
	}
	return nil
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
