// cmd/orchestrator/main.go
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

// cascadeSettleDelay is the pause between cascade sweeps, giving any child spawn
// that was in flight during a sweep time to register before the next snapshot.
// Tests set it to 0.
var cascadeSettleDelay = 300 * time.Millisecond

type SpawnRequest struct {
	AgentType string `json:"agentType"`
	UserID    string `json:"userId"`
	TenantID  string `json:"tenantId"`
	Task      string `json:"task,omitempty"`
	ParentID  string `json:"parentId,omitempty"`
	// OrgID is INTERNAL: the org the agent is spawned into, derived server-side
	// (agent path: inherited from the parent's registry record; human path: from
	// the token's organization claim, Phase 4 — org-less until then). The spec
	// binds org to the token/lineage, NEVER a request-body field, so it carries a
	// `json:"-"` tag: unlike userId (always overwritten from the token sub),
	// tenantId (a consumer primitive read from the body on the human path), and
	// parentId (rejected-if-present on the human path), OrgID must be structurally
	// impossible to set from the body — hence the tag rather than an after-decode
	// clear.
	OrgID string `json:"-"`
}

type SpawnResponse struct {
	WorkloadName string `json:"workloadName"`
}

func shortID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// logLine is a single parsed log entry.
type logLine struct {
	TS   string `json:"ts"`
	Text string `json:"text"`
}

// logsResponse is the JSON shape returned by GET /v1/agents/{id}/logs.
type logsResponse struct {
	PodName   string    `json:"podName"`
	Container string    `json:"container"`
	PodPhase  string    `json:"podPhase"`
	Lines     []logLine `json:"lines"`
	Complete  bool      `json:"complete"`
}

// parseLogLines parses raw log output produced with Timestamps:true. Each
// non-empty line has the form "<RFC3339Nano> <text>". When sinceTime is
// non-empty, only lines whose timestamp is strictly after it are returned.
// This filtering supplements the SinceTime PodLogOption, which only has
// second granularity and can re-deliver lines.
func parseLogLines(raw string, sinceTime string) []logLine {
	var since time.Time
	haveSince := false
	if sinceTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, sinceTime); err == nil {
			since = t
			haveSince = true
		}
	}

	lines := []logLine{}
	for _, l := range strings.Split(raw, "\n") {
		if l == "" {
			continue
		}
		ts := l
		text := ""
		if idx := strings.IndexByte(l, ' '); idx >= 0 {
			ts = l[:idx]
			text = l[idx+1:]
		}
		if haveSince {
			t, err := time.Parse(time.RFC3339Nano, ts)
			if err != nil || !t.After(since) {
				continue
			}
		}
		lines = append(lines, logLine{TS: ts, Text: text})
	}
	return lines
}

// isContainerNotReadyErr reports whether err indicates the container has not
// started yet (so logs are not available). These are expected transient
// conditions, not server errors.
func isContainerNotReadyErr(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"waiting to start", "ContainerCreating", "not found", "PodInitializing"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// SpiceDB agent permissions (schema.zed): view = read (events/logs/chain);
// manage = mutate (message/revoke/resume/dismiss/delete). Same names the
// registry checks — kept here so the orchestrator-tier gate matches.
const (
	actionView   = "view"
	actionManage = "manage"
)

// agentAllowed reports whether userId is allowed the given action on the agent —
// the orchestrator-tier mirror of the registry's agentAllowed (Phase-4 authz
// flip). Org-less agents keep today's userId-equality with NO SpiceDB round-trip
// (so existing org-less flows are byte-identical); org-scoped agents defer to
// SpiceDB. A SpiceDB error FAILS CLOSED (deny → the caller's 404), never opens
// access. An unknown/unavailable agent also denies (the read/interact handlers
// surface that as 404, never enumerable).
func agentAllowed(ctx context.Context, regClient registry.Client, sdb spicedb.Client, id, userId, action string) bool {
	if userId == "" {
		return false
	}
	rec, err := regClient.GetAgent(ctx, id)
	if err != nil || rec.AgentID == "" {
		return false
	}
	if rec.OrgID == "" {
		return rec.UserID == userId
	}
	ok, cerr := sdb.CheckPermission(ctx, "agent:"+id, action, "user:"+spicedb.EncodeObjectID(userId))
	if cerr != nil {
		log.Printf("agentAllowed: spicedb %s check for agent %s user %s failed (fail-closed deny): %v", action, id, userId, cerr)
		return false
	}
	return ok
}

// canView / canManage are the two authority levels the orchestrator gates on.
func canView(ctx context.Context, regClient registry.Client, sdb spicedb.Client, id, userId string) bool {
	return agentAllowed(ctx, regClient, sdb, id, userId, actionView)
}
func canManage(ctx context.Context, regClient registry.Client, sdb spicedb.Client, id, userId string) bool {
	return agentAllowed(ctx, regClient, sdb, id, userId, actionManage)
}

// orgMemberCheck reports whether userId is a member (or admin — admin ⊇ member in
// the schema) of org per SpiceDB, keyed by the SAME encoded subject the whole
// authz layer uses (user:<EncodeObjectID(sub)>). It is the human-spawn
// subject-consistency guard and the /v1/my/orgs active-org gate. Fail-closed: a
// SpiceDB error or a genuine non-member both return false.
func orgMemberCheck(ctx context.Context, sdb spicedb.Client, org, userId string) bool {
	ok, err := sdb.CheckPermission(ctx,
		"organization:"+spicedb.EncodeObjectID(org), "view_agents", "user:"+spicedb.EncodeObjectID(userId))
	if err != nil {
		log.Printf("orgMemberCheck: spicedb view_agents check for org %s user %s failed (fail-closed deny): %v", org, userId, err)
		return false
	}
	return ok
}

// orgAdminCheck reports whether userId holds admin_agents on org (org admin) per
// SpiceDB, encoded the same way as every other membership check. It is the single
// fail-closed chokepoint for the Phase-6 org-admin gates (requireOrgAdmin,
// requireTemplateManager, GET /v1/org/templates): a SpiceDB error or a genuine
// non-admin both return false.
func orgAdminCheck(ctx context.Context, sdb spicedb.Client, org, userId string) bool {
	ok, err := sdb.CheckPermission(ctx,
		"organization:"+spicedb.EncodeObjectID(org), "admin_agents", "user:"+spicedb.EncodeObjectID(userId))
	if err != nil {
		log.Printf("orgAdminCheck: spicedb admin_agents check for org %s user %s failed (fail-closed deny): %v", org, userId, err)
		return false
	}
	return ok
}

// appendOrgID appends a trusted, token-derived org id to a registry path as the
// ?orgId= query the registry stamps/filters on (Phase 6). Empty org → path
// unchanged. The org is ALWAYS server-derived (a token claim or ctxTemplateOrg),
// never a client value, so this can't widen a caller's org scope.
func appendOrgID(path, org string) string {
	if org == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "orgId=" + url.QueryEscape(org)
}

// ctxKey is the private type for orchestrator request-context values.
type ctxKey int

const (
	ctxUserID ctxKey = iota
	ctxRoles
	ctxOrgID
	// ctxTemplateOrg carries the template-management authority scope set by the
	// requireTemplateManager gate (Phase 6): "" for a realm admin (global
	// authority — may create global templates and manage any org's), or the
	// caller's org id for an org admin (scoped to that org's templates only). The
	// path builder turns a non-empty value into the trusted ?orgId= the registry
	// stamps/filters on — never a client-supplied body/query value.
	ctxTemplateOrg
)

// templateOrgFrom returns the template-management scope stashed by
// requireTemplateManager. "" means global authority (realm admin).
func templateOrgFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxTemplateOrg).(string)
	return s
}

// userIDFrom returns the authenticated user id placed in the context by
// requireToken (the dashboard token's sub). Empty if the request was not
// token-authenticated — ownership checks then deny by construction (empty userId
// never owns), so this never silently widens access.
func userIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxUserID).(string)
	return s
}

// orgIDFrom returns the active organization the token names (the parsed
// `organization` claim), placed in the context by requireToken. Empty when the
// token is org-less (global) — the caller then sees only its own org-less agents
// and never any org-scoped ones.
func orgIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxOrgID).(string)
	return s
}

// requireToken gates a dashboard-facing handler behind a valid delegated access
// token: it must be a Bearer token the validator accepts (signature/issuer/
// expiry), have `aud` containing audience, not be a delegation-only token, and
// carry the required scope. The authenticated user (the token `sub`, minus any
// "user:" prefix for parity with agent tokens) is stashed in the request context
// for the handler to scope on — the orchestrator never trusts a client-supplied
// userId. Missing/invalid/wrong-audience → 401; valid token missing scope → 403.
func requireToken(v tokenvalidator.TokenValidator, audience, scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims, err := v.ValidateAccessToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
		if err != nil {
			log.Printf("dashboard auth: token validation failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if claims.TokenUse == "delegation" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !claims.HasAudience(audience) {
			log.Printf("dashboard auth: aud %v does not contain %q", claims.Audience, audience)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !claims.HasScope(scope) {
			log.Printf("dashboard auth: missing scope %q (have %v)", scope, claims.Scopes)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		userID := strings.TrimPrefix(claims.User, "user:")
		if userID == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Validate the token-derived subject once, here at the point of extraction,
		// before it is forwarded to the registry as a userId that keys consent edges
		// and per-user scoping. A well-formed IdP subject always passes; a malformed
		// one is a bad token, so reject it as unauthorized rather than propagating a
		// key-corrupting value downstream (the registry re-checks at its own ingest).
		if !registry.ValidUserID(userID) {
			log.Printf("dashboard auth: token subject %q is not a valid userId", userID)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxRoles, claims.Roles)
		// Stash the active org (fail-closed "" when the token names none) so the
		// list handler scopes visibility to org agents without re-parsing the token.
		ctx = context.WithValue(ctx, ctxOrgID, claims.OrgID)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin gates a dashboard-facing handler behind an admin role in addition
// to a valid delegated token: it runs requireToken's token/audience/scope
// checks, then requires the token's `role` claim to include "admin" (else 403).
// The admin role is issued by Keycloak (the realm `admin` role) and rides in
// the orchestrator-audience access token via the realm's flat `role` mapper.
// This is the orchestrator-side enforcement; the dashboard BFF
// gates the same routes with requireAdmin too (defense in depth), so a
// non-admin is rejected at both tiers — UI hiding is cosmetic and never the
// only gate.
func requireAdmin(v tokenvalidator.TokenValidator, audience, scope string, next http.HandlerFunc) http.HandlerFunc {
	return requireToken(v, audience, scope, func(w http.ResponseWriter, r *http.Request) {
		if !hasRole(r.Context(), "admin") {
			// Attribute the attempted privilege escalation to the authenticated user
			// (the token sub) so a deny is investigable in logs.
			log.Printf("admin auth: user %s missing admin role (have %v)", userIDFrom(r.Context()), rolesFrom(r.Context()))
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// requireOrgAdmin gates an org self-service route (Phase 6) behind org-admin
// authority over the {id} org in the path. On top of requireToken's checks it
// requires EITHER the realm `admin` role (a platform admin bypasses for ANY org,
// matching the platform-admin /v1/admin/orgs family) OR that the caller is
// acting inside org {id} (their single-org token's claim.OrgID == {id}) AND holds
// SpiceDB `admin_agents` on it. Every denial is a 404 (never 403), so a caller
// can neither learn that org {id} exists nor that they lack admin on it —
// cross-org and non-admin are indistinguishable from "no such route/org". The
// SpiceDB subject is the token sub, encoded exactly like every other membership
// check (orgMemberCheck / the list handler); a SpiceDB error fails closed (404).
func requireOrgAdmin(v tokenvalidator.TokenValidator, audience, scope string, sdb spicedb.Client, next http.HandlerFunc) http.HandlerFunc {
	return requireToken(v, audience, scope, func(w http.ResponseWriter, r *http.Request) {
		if hasRole(r.Context(), "admin") {
			next(w, r)
			return
		}
		id := r.PathValue("id")
		// The token must be scoped to THIS org (single-org tokens): a claim naming
		// a different org (or none) can never manage org {id}. This is checked before
		// SpiceDB so a stale/cross-org token is rejected without a round-trip.
		if id == "" || orgIDFrom(r.Context()) != id {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if !orgAdminCheck(r.Context(), sdb, id, userIDFrom(r.Context())) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next(w, r)
	})
}

// requireOrgMember is requireOrgAdmin's read-only sibling: it admits an org
// MEMBER (or admin — admin ⊇ member) of org {id} in addition to a realm admin.
// Used for the members list an ordinary member may read. Same fail-closed 404 on
// cross-org / non-member / SpiceDB error, so it leaks no org existence.
func requireOrgMember(v tokenvalidator.TokenValidator, audience, scope string, sdb spicedb.Client, next http.HandlerFunc) http.HandlerFunc {
	return requireToken(v, audience, scope, func(w http.ResponseWriter, r *http.Request) {
		if hasRole(r.Context(), "admin") {
			next(w, r)
			return
		}
		id := r.PathValue("id")
		if id == "" || orgIDFrom(r.Context()) != id {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if !orgMemberCheck(r.Context(), sdb, id, userIDFrom(r.Context())) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next(w, r)
	})
}

// rolesFrom returns the role claims stashed in the request context by
// requireToken (the dashboard token's `role` claim). Empty for a non-admin —
// hasRole then denies by construction, so a token with no role claim can never
// widen access.
func rolesFrom(ctx context.Context) []string {
	r, _ := ctx.Value(ctxRoles).([]string)
	return r
}

// hasRole reports whether the context's roles include want.
func hasRole(ctx context.Context, want string) bool {
	for _, r := range rolesFrom(ctx) {
		if r == want {
			return true
		}
	}
	return false
}

// buildMux wires the orchestrator's routes. validator/audience/spawnScope gate
// the agent spawn path; readScope/writeScope gate the dashboard-facing routes
// (the human's delegated token, validated by the same validator).
func buildMux(k8s client.Client, clientset kubernetes.Interface, sdb spicedb.Client, registryURL string, validator tokenvalidator.TokenValidator, audience, spawnScope, readScope, writeScope string) *http.ServeMux {
	mux := http.NewServeMux()

	// controlPlaneBearer yields the bearer the orchestrator (a trusted
	// control-plane caller) presents to the registry. Its shape follows
	// CONTROL_PLANE_AUTH and is empty in the local demo (the registry then
	// enforces nothing). Built once; the oidc source auto-refreshes. It is used
	// both by the typed regClient (so control-plane-gated reads like the
	// single-template GET authorize) and by the forwardToRegistry passthroughs.
	controlPlaneBearer := controlPlaneBearerSource()

	regClient := registry.NewWithTokenSource(registryURL, controlPlaneBearer)
	// The events endpoint is now authenticated; the orchestrator posts to parent
	// and child agent ids, so it presents the control-plane bearer (empty/no-op
	// under auth=none, exactly like its registry client).
	evtClient := events.NewWithToken(registryURL, controlPlaneBearer)

	mux.HandleFunc("POST /spawn", func(w http.ResponseWriter, r *http.Request) {
		var req SpawnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// /spawn is authenticated by a Bearer token — the same validator, issuer,
		// and audience serve both the agent and human paths, but the paths differ
		// in required scope and how they derive authority:
		//
		//   Agent path (token has an actor chain `act`): the agent's ACT-AS
		//   access token. Requires orchestrator:spawn; userId from the token's sub;
		//   parentId from the outermost act.sub (ActingAgentName); tenantId from
		//   the PARENT's registry record. Body fields are ignored (can't forge).
		//
		//   Human path (token has NO actor chain — no `act` claim, or an empty
		//   one): the dashboard's delegated access token acting for the logged-in
		//   human. Requires orchestrator:write; userId from sub; parentId MUST be
		//   empty (top-level only).
		//
		// Both are rejected if token_use=delegation, or aud doesn't contain the
		// orchestrator audience.
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims, err := validator.ValidateAccessToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
		if err != nil {
			log.Printf("spawn: token validation failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if claims.TokenUse == "delegation" {
			log.Printf("spawn: rejecting token_use=delegation")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !claims.HasAudience(audience) {
			log.Printf("spawn: rejecting token: aud %v does not contain %q", claims.Audience, audience)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		userID := strings.TrimPrefix(claims.User, "user:")
		if userID == "" {
			log.Printf("spawn: token has no user subject")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Validate the token-derived subject at extraction, before it becomes the
		// spawned agent's userId (registry record owner + consent-edge key).
		if !registry.ValidUserID(userID) {
			log.Printf("spawn: token subject %q is not a valid userId", userID)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if len(claims.Chain) > 0 {
			// Agent path: the token carries an actor chain (the outermost is the
			// acting agent — the parent of this child).
			if !claims.HasScope(spawnScope) {
				log.Printf("spawn: rejecting token: missing scope %q (have %v)", spawnScope, claims.Scopes)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			parentID := claims.ActingAgentName
			if parentID == "" {
				log.Printf("spawn: token has no acting agent")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			parent, err := regClient.GetAgent(r.Context(), parentID)
			if err != nil {
				log.Printf("spawn: unknown parent agent %s: %v", parentID, err)
				http.Error(w, "unknown parent agent", http.StatusForbidden)
				return
			}
			req.UserID = userID
			req.ParentID = parentID
			req.TenantID = parent.TenantID
			// A child inherits its parent's org (the SaaS-account boundary), just
			// as it inherits the tenant primitive — both from the parent's trusted
			// registry record, never the body. Empty when the parent is org-less.
			req.OrgID = parent.OrgID
		} else {
			// Human path: the dashboard's delegated token, acting for the human.
			if !claims.HasScope(writeScope) {
				log.Printf("spawn: rejecting token: missing scope %q (have %v)", writeScope, claims.Scopes)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if req.ParentID != "" {
				http.Error(w, "parentId not allowed on dashboard spawn", http.StatusBadRequest)
				return
			}
			req.UserID = userID
			// Org derived from the token's organization claim, NEVER the body: a
			// token carrying an org claim always spawns into that org (admin or not).
			// Empty when the token is org-less.
			req.OrgID = claims.OrgID
			// A non-admin human MUST have an active organization to spawn: as of
			// Phase 5 the dashboard mints single-org tokens, so a non-admin with no
			// active org is refused. A realm admin keeps the org-less (global) spawn
			// path. tenantId (body primitive) + requiresTenant are unaffected below.
			if req.OrgID == "" && !claims.HasRole("admin") {
				log.Printf("spawn: non-admin user %s has no active organization", userID)
				http.Error(w, "no active organization", http.StatusForbidden)
				return
			}
			// Subject-consistency guard (defense in depth). Keycloak only issues an
			// organization:<alias> claim to an ACTUAL member, so an org claim here
			// means the caller is a member — and the SpiceDB membership tuples are
			// keyed by the same token sub (username, via the dashboard sub=username
			// mapper; agent tokens likewise carry sub=user:<username>). Re-check
			// membership against SpiceDB, keyed by <sub>, before spawning into the org:
			// if the claim says "member" but the check does NOT resolve, the sub no
			// longer matches the membership-tuple convention (e.g. a future login path
			// emitting a UUID sub) — fail LOUD rather than silently spawning into an
			// org whose authorization layer can't see this subject. Fail-closed on a
			// SpiceDB error (matches the list handler's stance).
			if req.OrgID != "" && !orgMemberCheck(r.Context(), sdb, req.OrgID, userID) {
				log.Printf("spawn: user %s carries org claim %q but is not a SpiceDB member (subject/tuple-key mismatch or removed member) — refusing", userID, req.OrgID)
				http.Error(w, "organization membership could not be verified", http.StatusForbidden)
				return
			}
		}

		if req.UserID == "" {
			http.Error(w, "userId is required", http.StatusBadRequest)
			return
		}
		if req.AgentType == "" {
			http.Error(w, "agentType is required", http.StatusBadRequest)
			return
		}

		tpl, err := regClient.GetTemplate(r.Context(), req.AgentType)
		if err != nil {
			log.Printf("get template %s: %v", req.AgentType, err)
			http.Error(w, "unknown agent type", http.StatusBadRequest)
			return
		}
		// Org-scoped template gate (Phase 6): a template stamped with an OrgID is
		// spawnable ONLY in that org; a global (OrgID == "") template is spawnable
		// everywhere (the seeded catalog). req.OrgID is the server-derived org (the
		// human token's claim, or the parent record for an agent spawn) — never the
		// body. A mismatch reads as the SAME "unknown agent type" 400 that a missing
		// template yields, so a caller in org B cannot distinguish "acme has a
		// private template X" from "X does not exist" (no cross-org existence
		// oracle). requiresTenant is untouched and evaluated below as before.
		if tpl.OrgID != "" && tpl.OrgID != req.OrgID {
			log.Printf("spawn: template %s is org-scoped to %q, not visible to org %q", req.AgentType, tpl.OrgID, req.OrgID)
			http.Error(w, "unknown agent type", http.StatusBadRequest)
			return
		}
		// A disabled template blocks all new instantiations. This gate covers both
		// top-level and agent-initiated spawns, since both fetch the target
		// template here, and runs before any tenant/lifecycle/consent logic.
		if tpl.Status == registry.TemplateStatusDisabled {
			http.Error(w, "agent type is disabled", http.StatusConflict)
			return
		}
		// Tenant-ness is derived from the presence of a tenant id; a tenant id is
		// only mandatory when the template explicitly requires one.
		if tpl.RequiresTenant && req.TenantID == "" {
			http.Error(w, "tenantId is required for this agent type", http.StatusBadRequest)
			return
		}
		lifecycle := tpl.Runtime.Lifecycle
		if lifecycle == "" {
			lifecycle = "short-lived"
		}

		// Spawn-time policy: an agent-initiated spawn (parentId present) is allowed
		// only if the parent template lists this child type in allowedChildTypes
		// (deny-by-default). Top-level spawns (no parentId) are unconstrained.
		consentRequired := false
		if req.ParentID != "" {
			dec, err := regClient.CheckSpawnPolicy(r.Context(), req.ParentID, req.AgentType)
			if err != nil {
				log.Printf("spawn policy check for parent %s -> %s: %v", req.ParentID, req.AgentType, err)
				http.Error(w, "spawn policy check failed", http.StatusBadGateway)
				return
			}
			if !dec.Allowed {
				log.Printf("spawn denied: parent %s may not spawn %s: %s", req.ParentID, req.AgentType, dec.Reason)
				go func() {
					_ = evtClient.PostEvent(context.Background(), req.ParentID, events.Event{
						Source: events.SourceOrchestrator,
						Type:   "spawn_denied",
						Payload: mustMarshal(map[string]string{
							"childType": req.AgentType,
							"reason":    dec.Reason,
						}),
					})
				}()
				http.Error(w, "spawn denied: "+dec.Reason, http.StatusForbidden)
				return
			}
			// The parent template gates this child behind user consent: the
			// child's sidecar runs a CIBA flow before serving tokens.
			consentRequired = dec.ConsentRequired
		}

		// The workload name becomes the pod name and the agent-id label, which SPIRE
		// uses to issue the SVID. The agent's identity is the SVID, not this name.
		workloadName := "agent-" + shortID()
		aw := &agentv1alpha1.AgentWorkload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workloadName,
				Namespace: "default",
			},
			Spec: agentv1alpha1.AgentWorkloadSpec{
				AgentType:       req.AgentType,
				UserID:          req.UserID,
				TenantID:        req.TenantID,
				Lifecycle:       lifecycle,
				Task:            req.Task,
				ParentID:        req.ParentID,
				ConsentRequired: consentRequired,
			},
		}

		// Pre-register BEFORE creating the workload, and fail the spawn if it
		// errors. Ordering is security-relevant: the registry record is the
		// trusted source of the agent's ParentID (the events-auth gate lets an
		// SVID caller post to its own id or its stored ParentID). Pre-registering
		// here — control-plane-authenticated, over the orchestrator's bearer —
		// sets ParentID from the validated actor chain, and the registry makes it
		// immutable thereafter. If instead the pod's sidecar were the first to
		// register (which it can only do for its own SVID-pinned id, never
		// setting a lineage), a later cpAuth pre-register would be racing a record
		// that already exists. Creating the record first closes that window; it
		// also means the agent shows "pending" in the UI before the pod starts.
		if err := regClient.PreRegisterAgent(r.Context(), registry.AgentRecord{
			AgentID:      workloadName,
			AgentType:    req.AgentType,
			TenantID:     req.TenantID,
			UserID:       req.UserID,
			Lifecycle:    lifecycle,
			SupportsChat: tpl.Runtime.SupportsChat,
			ParentID:     req.ParentID,
			// Org recorded at spawn (server-derived), so the self-registering
			// sidecar's OrgID comes from this trusted record — never its body.
			OrgID: req.OrgID,
		}); err != nil {
			log.Printf("pre-register agent: %v", err)
			http.Error(w, "failed to spawn agent", http.StatusInternalServerError)
			return
		}

		if err := k8s.Create(r.Context(), aw); err != nil {
			log.Printf("create AgentWorkload: %v", err)
			// The record now exists but the workload doesn't. The operator's
			// stale sweep deliberately skips "pending" (it races in-flight
			// spawns), so nothing else would ever reap this orphan — dismiss it
			// best-effort here so a failed spawn doesn't leave a phantom pending
			// agent in the UI. Pass the owner (the record we just pre-registered
			// carries req.UserID) so the registry's ownership gate admits the
			// dismiss. WithoutCancel: if Create failed because the inbound request
			// was canceled, the cleanup must still run on a live context.
			if derr := regClient.DismissAgent(context.WithoutCancel(r.Context()), workloadName, req.UserID); derr != nil {
				log.Printf("dismiss orphaned pre-registration %s: %v", workloadName, derr)
			}
			http.Error(w, "failed to spawn agent", http.StatusInternalServerError)
			return
		}

		go func() {
			_ = evtClient.PostEvent(context.Background(), workloadName, events.Event{
				Source: events.SourceOrchestrator,
				Type:   "workload_created",
				Payload: mustMarshal(map[string]string{
					"workloadName": workloadName,
					"agentType":    req.AgentType,
					"tenantId":     req.TenantID,
					"userId":       req.UserID,
					"task":         req.Task,
				}),
			})
		}()

		log.Printf("spawned workload %s for tenant %s user %s", workloadName, req.TenantID, req.UserID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(SpawnResponse{WorkloadName: workloadName})
	})

	mux.HandleFunc("GET /v1/agents", requireToken(validator, audience, readScope, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		agents, err := regClient.ListAgents(ctx)
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		userID := userIDFrom(ctx)
		orgID := orgIDFrom(ctx)

		// The caller's org authority is resolved against SpiceDB ONCE per request
		// (not per agent): two facts drive the whole list — is the caller a current
		// MEMBER of their active org (org read-visibility) and are they an ADMIN
		// (manage over every org agent). Deriving visibility from SpiceDB, not the
		// bare `organization` claim, keeps the list consistent with the per-agent
		// gates: when a member is removed their SpiceDB tuples drop immediately, so
		// a still-unexpired org token can no longer enumerate the org's agents
		// (closing a stale-token metadata leak). admin ⊇ member in the schema, so an
		// admin need not be re-checked for membership.
		isOrgMember, isOrgAdmin := false, false
		if userID != "" && orgID != "" {
			orgObj := "organization:" + spicedb.EncodeObjectID(orgID)
			userObj := "user:" + spicedb.EncodeObjectID(userID)
			adminOK, cerr := sdb.CheckPermission(ctx, orgObj, "admin_agents", userObj)
			if cerr != nil {
				// Fail closed: an unreadable authority fact denies (no org rows, no
				// manage), never an accidental grant.
				log.Printf("list: org-admin check for user %s org %s failed (treating as non-admin): %v", userID, orgID, cerr)
			}
			isOrgAdmin = adminOK
			if isOrgAdmin {
				isOrgMember = true
			} else {
				memberOK, merr := sdb.CheckPermission(ctx, orgObj, "view_agents", userObj)
				if merr != nil {
					log.Printf("list: org-member check for user %s org %s failed (treating as non-member): %v", userID, orgID, merr)
				}
				isOrgMember = memberOK
			}
		}

		// Visibility (spec matrix): a caller sees their OWN agents (org-scoped or
		// org-less, always — the record UserID is the owner) PLUS every agent in
		// their ACTIVE org when SpiceDB confirms membership. Cross-org agents and
		// other users' org-less agents are invisible. The own-agent term keeps the
		// list consistent with the per-agent gates: canView/canManage still succeed
		// on your own org agent via its owner tuple even in a removed-member window,
		// so it must still appear here. Per-agent canManage: owner always; org
		// admins over any agent in their org. Record-field equality on the rows —
		// no PER-ROW SpiceDB (the two org checks above are per-request).
		type agentListItem struct {
			registry.AgentRecord
			CanManage bool `json:"canManage"`
		}
		items := make([]agentListItem, 0, len(agents))
		for _, a := range agents {
			own := a.UserID != "" && a.UserID == userID
			inOrg := isOrgMember && a.OrgID != "" && a.OrgID == orgID
			if !own && !inOrg {
				continue
			}
			canManage := own || (inOrg && isOrgAdmin)
			items = append(items, agentListItem{AgentRecord: a, CanManage: canManage})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	}))

	mux.HandleFunc("GET /v1/agents/{id}/events", requireToken(validator, audience, readScope, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// events are a read → view authority (owner or any same-org member).
		if !canView(r.Context(), regClient, sdb, id, userIDFrom(r.Context())) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		evts, err := regClient.ListEvents(r.Context(), id)
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(evts)
	}))

	mux.HandleFunc("GET /v1/agents/{id}/logs", requireToken(validator, audience, readScope, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// logs are a read → view authority (owner or any same-org member).
		if !canView(r.Context(), regClient, sdb, id, userIDFrom(r.Context())) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		container := r.URL.Query().Get("container")
		if container == "" {
			container = "agent"
		}
		if container != "agent" && container != "agent-sidecar" {
			http.Error(w, "invalid container", http.StatusBadRequest)
			return
		}

		sinceTime := r.URL.Query().Get("sinceTime")
		var sinceTimePtr *metav1.Time
		if sinceTime != "" {
			t, err := time.Parse(time.RFC3339Nano, sinceTime)
			if err != nil {
				http.Error(w, "invalid sinceTime", http.StatusBadRequest)
				return
			}
			mt := metav1.NewTime(t)
			sinceTimePtr = &mt
		}

		tailLines := int64(500)
		if v := r.URL.Query().Get("tailLines"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				http.Error(w, "invalid tailLines", http.StatusBadRequest)
				return
			}
			tailLines = n
		}

		// Resolve the pod name: prefer the workload's recorded Status.PodName,
		// falling back to the conventional "{id}-pod".
		podName := id + "-pod"
		aw := &agentv1alpha1.AgentWorkload{}
		if err := k8s.Get(r.Context(), types.NamespacedName{Name: id, Namespace: "default"}, aw); err == nil {
			if aw.Status.PodName != "" {
				podName = aw.Status.PodName
			}
		}

		// Determine pod phase (best-effort). A missing pod means "Pending".
		podPhase := "Pending"
		pod, perr := clientset.CoreV1().Pods("default").Get(r.Context(), podName, metav1.GetOptions{})
		if perr == nil {
			podPhase = string(pod.Status.Phase)
		}

		resp := logsResponse{
			PodName:   podName,
			Container: container,
			PodPhase:  podPhase,
			Lines:     []logLine{},
			Complete:  podPhase == string(corev1.PodSucceeded) || podPhase == string(corev1.PodFailed),
		}

		opts := &corev1.PodLogOptions{
			Container:  container,
			Timestamps: true,
		}
		if sinceTimePtr != nil {
			opts.SinceTime = sinceTimePtr
		} else {
			opts.TailLines = &tailLines
		}

		stream, err := clientset.CoreV1().Pods("default").GetLogs(podName, opts).Stream(r.Context())
		if err != nil {
			// Container not started yet / pod missing: surface a waiting state
			// rather than a 5xx so the UI can poll.
			if isContainerNotReadyErr(err) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
			log.Printf("get logs for %s/%s: %v", podName, container, err)
			http.Error(w, "failed to fetch logs", http.StatusInternalServerError)
			return
		}
		defer stream.Close()

		raw, err := io.ReadAll(stream)
		if err != nil {
			log.Printf("read logs for %s/%s: %v", podName, container, err)
			http.Error(w, "failed to read logs", http.StatusInternalServerError)
			return
		}

		resp.Lines = parseLogLines(string(raw), sinceTime)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))

	// DELETE /v1/agents/{id} tears down an agent AND its entire descendant
	// subtree (the pods). It asks the registry for the subtree (root-first) and
	// deletes each AgentWorkload CR; deleting a CR kills its pod via owner-ref GC
	// and stops that node from spawning more children.
	//
	// We sweep in a settle-loop rather than deleting one snapshot once. chain
	// agents self-spawn at every level (~every 3s), so a child can be born during
	// the cascade and won't appear in the first snapshot. We re-fetch the subtree
	// and re-delete until a pass deletes zero LIVE CRs (every Delete returned
	// NotFound), which means no live pod remains to spawn anything new.
	//
	// The termination condition is "deleted zero live CRs", NOT "subtree is
	// empty": the operator's deletion finalizer only marks each registry record
	// Completed/Failed — it does not remove the record — and subtree() returns
	// descendants regardless of status. So the subtree does NOT shrink as we
	// delete CRs; an "empty subtree" loop would never terminate.
	//
	// Status codes: 204 success, 207 if some node deletes errored (non-NotFound),
	// 404 if the registry never heard of the id (first pass empty), 502 if the
	// registry is unavailable on the first pass.
	mux.HandleFunc("DELETE /v1/agents/{id}", requireToken(validator, audience, writeScope, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		userID := userIDFrom(ctx)
		// DELETE tears the agent + subtree down → manage authority.
		if !canManage(ctx, regClient, sdb, id, userID) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		const maxPasses = 8
		deleted := 0
		failedSet := map[string]bool{} // node ids whose delete errored (non-NotFound)

		for pass := 0; pass < maxPasses; pass++ {
			nodes, err := regClient.Subtree(ctx, id, userID)
			if err != nil {
				if pass == 0 {
					http.Error(w, "registry unavailable", http.StatusBadGateway)
					return
				}
				break // already made progress; stop sweeping
			}
			if len(nodes) == 0 {
				if pass == 0 {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				break
			}

			deletedThisPass := 0
			for _, nid := range nodes {
				aw := &agentv1alpha1.AgentWorkload{}
				aw.Name = nid
				aw.Namespace = "default"
				if err := k8s.Delete(ctx, aw); err != nil {
					if apierrors.IsNotFound(err) {
						continue // already gone — normal on re-sweep
					}
					failedSet[nid] = true
					continue
				}
				deletedThisPass++
				deleted++
			}

			if deletedThisPass == 0 {
				break // converged: nothing live left to delete or spawn
			}
			if pass < maxPasses-1 {
				// Settle pause so in-flight child spawns register before the next
				// sweep. Abort if the caller has gone away rather than hold a
				// goroutine sleeping for a disconnected request.
				select {
				case <-ctx.Done():
					return
				case <-time.After(cascadeSettleDelay):
				}
			}
		}

		if len(failedSet) > 0 {
			failed := make([]string, 0, len(failedSet))
			for nid := range failedSet {
				failed = append(failed, nid)
			}
			sort.Strings(failed)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMultiStatus) // 207
			json.NewEncoder(w).Encode(map[string]any{"deleted": deleted, "failed": failed})
			return
		}
		w.WriteHeader(http.StatusNoContent) // 204
	}))

	// withUserID returns a registry path that forwards the inbound query string
	// but forces userId to the token's authenticated user, for every route whose
	// registry endpoint scopes ownership by userId. Any client-supplied userId is
	// dropped and replaced: the registry reads userId with url.Values.Get, which
	// returns the FIRST value, so appending a second userId would let an injected
	// one win — we Set instead, making the token's user authoritative. Empty token
	// context leaves userId unset and lets the registry reject.
	withUserID := func(path string, r *http.Request) string {
		q := r.URL.Query()
		q.Del("userId")
		if uid := userIDFrom(r.Context()); uid != "" {
			q.Set("userId", uid)
		}
		if enc := q.Encode(); enc != "" {
			return path + "?" + enc
		}
		return path
	}

	// requireCanManage is the primary authorization gate for the per-agent
	// mutations the orchestrator forwards to the registry (dismiss/revoke/resume).
	// It sits INSIDE requireToken — which has already validated the access token
	// and put the user (token sub) in the context — and denies (404, never 403, so
	// authorization isn't enumerable) unless that user holds manage on the {id}
	// agent: owner for org-less agents, owner-or-org-admin for org-scoped ones.
	// This makes the validated sub authoritative, mirroring the
	// DELETE/events/logs/message handlers; the forwarded userId + the registry's
	// own agentAllowed check remain as a defense-in-depth backstop.
	requireCanManage := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !canManage(r.Context(), regClient, sdb, r.PathValue("id"), userIDFrom(r.Context())) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", requireToken(validator, audience, writeScope, requireCanManage(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Ownership is enforced above by requireOwnedAgent (token sub); the
		// forwarded userId keeps the registry's own agentOwnedBy check as a
		// backstop.
		req2, err := http.NewRequestWithContext(r.Context(), "POST",
			registryURL+withUserID("/v1/agents/"+id+"/dismiss", r), nil)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
	})))

	// forwardToRegistry relays a request to the registry (which owns agent
	// lineage, templates, and consents) and copies back the registry's status,
	// Content-Type, and body. It streams the inbound body through (empty for the
	// bodyless GET/POST/DELETE callers) and forwards the inbound Content-Type
	// when present, so it serves both the read passthroughs and the body-carrying
	// template create/update routes. path builds the registry path from the
	// inbound request (path values, query string).
	forwardToRegistry := func(method string, path func(r *http.Request) string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			req2, err := http.NewRequestWithContext(r.Context(), method, registryURL+path(r), r.Body)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if ct := r.Header.Get("Content-Type"); ct != "" {
				req2.Header.Set("Content-Type", ct)
			}
			if t := controlPlaneBearer(); t != "" {
				req2.Header.Set("Authorization", "Bearer "+t)
			}
			resp, err := http.DefaultClient.Do(req2)
			if err != nil {
				http.Error(w, "registry unavailable", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			// Relay the registry's Content-Type so error bodies (text/plain from
			// http.Error) aren't mislabeled as JSON.
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
		}
	}

	// revoke/resume are cascading authorization actions owned by the registry.
	// Ownership is gated here at the orchestrator (requireOwnedAgent, token sub);
	// the forwarded userId leaves the registry's agentOwnedBy check as a backstop.
	mux.HandleFunc("POST /v1/agents/{id}/revoke", requireToken(validator, audience, writeScope, requireCanManage(forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/agents/"+r.PathValue("id")+"/revoke", r)
	}))))
	mux.HandleFunc("POST /v1/agents/{id}/resume", requireToken(validator, audience, writeScope, requireCanManage(forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/agents/"+r.PathValue("id")+"/resume", r)
	}))))

	// withCallerOrg appends the caller's active org (the token's organization
	// claim, server-derived) as the trusted ?orgId= the registry filters template
	// lists on (Phase 6). An org-less caller (realm admin global, or no active org)
	// gets no param → the registry returns only global templates. The org is NEVER
	// read from the client's query/body — only from the validated token — so a
	// caller can't widen their template visibility to another org.
	withCallerOrg := func(path string, r *http.Request) string {
		return appendOrgID(path, orgIDFrom(r.Context()))
	}
	// withTemplateOrg appends the template-management scope (ctxTemplateOrg) set by
	// requireTemplateManager: empty for a realm admin (registry: global authority),
	// the org id for an org admin (registry: stamp/scope to that org). Same
	// token-derived, never-client-supplied guarantee as withCallerOrg.
	withTemplateOrg := func(path string, r *http.Request) string {
		return appendOrgID(path, templateOrgFrom(r.Context()))
	}
	// requireTemplateManager gates template create/disable/delete for BOTH a realm
	// admin (global authority: may manage global + any org's templates) and an org
	// admin (scoped to their OWN org's templates). It stashes ctxTemplateOrg — ""
	// for the realm admin, the caller's org for an org admin — which the path
	// builder turns into the trusted ?orgId= the registry stamps on create and
	// enforces on disable/delete (an org admin can never touch a global or
	// other-org template: the registry 404s the mismatch). A non-admin, non-org-admin
	// caller gets 403.
	requireTemplateManager := func(next http.HandlerFunc) http.HandlerFunc {
		return requireToken(validator, audience, writeScope, func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if hasRole(ctx, "admin") {
				next(w, r.WithContext(context.WithValue(ctx, ctxTemplateOrg, "")))
				return
			}
			org := orgIDFrom(ctx)
			if org == "" || !orgAdminCheck(ctx, sdb, org, userIDFrom(ctx)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next(w, r.WithContext(context.WithValue(ctx, ctxTemplateOrg, org)))
		})
	}

	mux.HandleFunc("GET /v1/templates", requireToken(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		return withCallerOrg("/v1/templates", r)
	})))
	// Non-admin spawn list: active types plus their requiresTenant flag, for the
	// spawn modal to decide whether to show the Tenant field. Same read scope as
	// the thin GET /v1/templates above (NOT admin) — the registry's detail=spawn
	// route is public, so forwarding the control-plane bearer here is harmless.
	// The downstream query is HARDCODED to detail=spawn so no client can coax this
	// read-scope route into the admin-only detail=full path; withCallerOrg adds the
	// caller's org so the dropdown shows global + own-org types only (Phase 6).
	mux.HandleFunc("GET /v1/templates/spawn", requireToken(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		return withCallerOrg("/v1/templates?detail=spawn", r)
	})))
	// Admin full-detail template list (incl. disabled) for the dashboard's Agent
	// Types admin view. Admin-gated (role=admin on top of read scope) at the
	// orchestrator; the BFF gates its /api/admin/templates twin with
	// requireAdmin too (defense in depth). forwardToRegistry carries the
	// orchestrator's control-plane bearer, so the registry's detail=full route
	// (control-plane gated) authorizes — the caller's admin role is checked
	// here, not relied on at the registry. No orgId → the registry returns ALL
	// templates (global + every org's), the realm-admin full view.
	mux.HandleFunc("GET /v1/admin/templates", requireAdmin(validator, audience, readScope, forwardToRegistry("GET", func(*http.Request) string {
		return "/v1/templates?detail=full"
	})))
	// Org-admin full-detail template list (Phase 6): the org admin's Agent Types
	// view — scoped to THEIR org's templates only (detail=full + orgId). The realm
	// admin uses /v1/admin/templates instead; here we require org-admin of the
	// active org, so a member (or cross-org token) gets 403 and no other org's
	// templates ever leak.
	mux.HandleFunc("GET /v1/org/templates", requireToken(validator, audience, readScope, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		org := orgIDFrom(ctx)
		if org == "" || !orgAdminCheck(ctx, sdb, org, userIDFrom(ctx)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		forwardToRegistry("GET", func(*http.Request) string {
			return "/v1/templates?detail=full&orgId=" + url.QueryEscape(org)
		})(w, r)
	}))
	// Template management (control-plane-auth'd at the registry, authz'd at both
	// the orchestrator and the dashboard BFF): create, update status, and delete.
	// requireTemplateManager admits a realm admin (global authority) or an org
	// admin (scoped to their org) on top of the write scope; a plain
	// orchestrator:write token without either gets 403. withTemplateOrg carries the
	// trusted scope to the registry, which stamps it on create and enforces it on
	// disable/delete. forwardToRegistry streams the body through, so the
	// body-carrying create/update and the bodyless delete all use it.
	mux.HandleFunc("POST /v1/templates", requireTemplateManager(forwardToRegistry("POST", func(r *http.Request) string {
		return withTemplateOrg("/v1/templates", r)
	})))
	mux.HandleFunc("PATCH /v1/templates/{agentType}", requireTemplateManager(forwardToRegistry("PATCH", func(r *http.Request) string {
		return withTemplateOrg("/v1/templates/"+url.PathEscape(r.PathValue("agentType")), r)
	})))
	mux.HandleFunc("DELETE /v1/templates/{agentType}", requireTemplateManager(forwardToRegistry("DELETE", func(r *http.Request) string {
		return withTemplateOrg("/v1/templates/"+url.PathEscape(r.PathValue("agentType")), r)
	})))

	// Org onboarding + membership admin API (multi-tenancy Phase 3). PLATFORM-ADMIN
	// ONLY: requireAdmin gates every route (role=admin on top of the delegated
	// token), and forwardToRegistry carries the orchestrator's control-plane bearer
	// so the registry's cp-gated org endpoints authorize. The registry is the
	// single dual-write choke point (Keycloak + SpiceDB); the orchestrator only
	// forwards. Org-admin self-service over their OWN org arrives in Phase 6.
	mux.HandleFunc("POST /v1/admin/orgs", requireAdmin(validator, audience, writeScope, forwardToRegistry("POST", func(*http.Request) string {
		return "/v1/orgs"
	})))
	// List orgs, or ?member=<user> to list one user's orgs. Only the `member`
	// param is forwarded (re-escaped) so no other query can be smuggled through.
	mux.HandleFunc("GET /v1/admin/orgs", requireAdmin(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		if m := r.URL.Query().Get("member"); m != "" {
			return "/v1/orgs?member=" + url.QueryEscape(m)
		}
		return "/v1/orgs"
	})))
	mux.HandleFunc("GET /v1/admin/orgs/{id}", requireAdmin(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id"))
	})))
	mux.HandleFunc("PATCH /v1/admin/orgs/{id}", requireAdmin(validator, audience, writeScope, forwardToRegistry("PATCH", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id"))
	})))
	mux.HandleFunc("DELETE /v1/admin/orgs/{id}", requireAdmin(validator, audience, writeScope, forwardToRegistry("DELETE", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id"))
	})))
	mux.HandleFunc("GET /v1/admin/orgs/{id}/members", requireAdmin(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members"
	})))
	mux.HandleFunc("POST /v1/admin/orgs/{id}/members", requireAdmin(validator, audience, writeScope, forwardToRegistry("POST", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members"
	})))
	mux.HandleFunc("PATCH /v1/admin/orgs/{id}/members/{user}", requireAdmin(validator, audience, writeScope, forwardToRegistry("PATCH", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members/" + url.PathEscape(r.PathValue("user"))
	})))
	mux.HandleFunc("DELETE /v1/admin/orgs/{id}/members/{user}", requireAdmin(validator, audience, writeScope, forwardToRegistry("DELETE", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members/" + url.PathEscape(r.PathValue("user"))
	})))

	// Org SELF-SERVICE membership + settings API (multi-tenancy Phase 6). Unlike
	// the /v1/admin/orgs family above (realm-admin only), these let an ORG ADMIN
	// manage their OWN org — gated by requireOrgAdmin ({id} must equal the token's
	// active org + SpiceDB admin_agents; realm admin bypasses for any org). Members
	// get the read-only GET members via requireOrgMember. Every cross-org / non-admin
	// denial is a 404, so no org existence leaks. forwardToRegistry carries the cp
	// bearer to the same single dual-write choke point (Keycloak + SpiceDB) the
	// admin routes use; the registry also wires the removed-member disposition.
	mux.HandleFunc("GET /v1/orgs/{id}", requireOrgMember(validator, audience, readScope, sdb, forwardToRegistry("GET", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id"))
	})))
	mux.HandleFunc("PATCH /v1/orgs/{id}", requireOrgAdmin(validator, audience, writeScope, sdb, forwardToRegistry("PATCH", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id"))
	})))
	mux.HandleFunc("GET /v1/orgs/{id}/members", requireOrgMember(validator, audience, readScope, sdb, forwardToRegistry("GET", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members"
	})))
	mux.HandleFunc("POST /v1/orgs/{id}/members", requireOrgAdmin(validator, audience, writeScope, sdb, forwardToRegistry("POST", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members"
	})))
	mux.HandleFunc("PATCH /v1/orgs/{id}/members/{user}", requireOrgAdmin(validator, audience, writeScope, sdb, forwardToRegistry("PATCH", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members/" + url.PathEscape(r.PathValue("user"))
	})))
	mux.HandleFunc("DELETE /v1/orgs/{id}/members/{user}", requireOrgAdmin(validator, audience, writeScope, sdb, forwardToRegistry("DELETE", func(r *http.Request) string {
		return "/v1/orgs/" + url.PathEscape(r.PathValue("id")) + "/members/" + url.PathEscape(r.PathValue("user"))
	})))

	// GET /v1/my/orgs — the CALLER's OWN org memberships (session-gated, NOT
	// admin-gated). Powers the dashboard org picker / no-organization screen. The
	// membership subject is the token sub (userIDFrom), NEVER a client param — a
	// caller can only ever enumerate their OWN orgs, so this leaks no org existence.
	// It enriches the registry's ?member= list (Keycloak-sourced membership) with
	// two token-derived facts: the active org (the single-org claim) and whether the
	// caller is that org's admin (one SpiceDB admin_agents check). Both are derived
	// server-side from the validated token, so neither can be spoofed by the browser.
	mux.HandleFunc("GET /v1/my/orgs", requireToken(validator, audience, readScope, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		member := userIDFrom(ctx)
		req2, err := http.NewRequestWithContext(ctx, "GET", registryURL+"/v1/orgs?member="+url.QueryEscape(member), nil)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if t := controlPlaneBearer(); t != "" {
			req2.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("my/orgs: registry ?member=%s returned %d", member, resp.StatusCode)
			http.Error(w, "org list failed", http.StatusBadGateway)
			return
		}
		var orgs []registry.Organization
		if err := json.NewDecoder(resp.Body).Decode(&orgs); err != nil {
			http.Error(w, "org list decode failed", http.StatusBadGateway)
			return
		}
		if orgs == nil {
			orgs = []registry.Organization{}
		}
		activeOrg := orgIDFrom(ctx)
		isOrgAdmin := false
		if activeOrg != "" {
			ok, cerr := sdb.CheckPermission(ctx,
				"organization:"+spicedb.EncodeObjectID(activeOrg), "admin_agents", "user:"+spicedb.EncodeObjectID(member))
			if cerr != nil {
				// Fail closed: an unreadable authority fact denies admin, never grants it.
				log.Printf("my/orgs: admin_agents check for user %s org %s failed (treating as non-admin): %v", member, activeOrg, cerr)
			}
			isOrgAdmin = ok
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"orgs": orgs, "activeOrg": activeOrg, "isOrgAdmin": isOrgAdmin})
	}))

	// POST /v1/signup/org — SELF-SERVE org creation (multi-tenancy Phase 7). Unlike
	// the platform-admin /v1/admin/orgs family (requireAdmin), this is only
	// requireToken-gated: any authenticated user may create their FIRST org and
	// become its admin. The first admin is ALWAYS the validated token sub
	// (userIDFrom) — never a body field — so a caller can never create an org owned
	// by someone else, and the registry's one-org-per-user + no-hijack guards ride
	// on that server-derived identity. The client body carries only {orgId, name};
	// we re-marshal it with adminUser stamped and forward to the registry's POST
	// /v1/orgs choke point (same Keycloak+SpiceDB dual-write) with ?selfServe=1 so
	// the registry applies the self-serve-only guards.
	mux.HandleFunc("POST /v1/signup/org", requireToken(validator, audience, writeScope, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		caller := userIDFrom(ctx)
		var body struct {
			OrgID string `json:"orgId"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		// adminUser is the token sub, stamped server-side; any adminUser in the body
		// is structurally ignored (it is not a field we decode).
		payload, err := json.Marshal(map[string]string{"orgId": body.OrgID, "name": body.Name, "adminUser": caller})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		req2, err := http.NewRequestWithContext(ctx, "POST", registryURL+"/v1/orgs?selfServe=1", bytes.NewReader(payload))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		req2.Header.Set("Content-Type", "application/json")
		if t := controlPlaneBearer(); t != "" {
			req2.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))

	// Stored spawn consents: registry passthroughs for the dashboard's consent
	// management view (list per user, revoke to force a re-prompt next spawn).
	// The query string carries the dashboard's session-user scoping.
	mux.HandleFunc("GET /v1/consents", requireToken(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		return withUserID("/v1/consents", r)
	})))
	mux.HandleFunc("POST /v1/consents/{id}/revoke", requireToken(validator, audience, writeScope, forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/consents/"+r.PathValue("id")+"/revoke", r)
	})))

	// Brokered consent requests (Phase 5b): registry passthroughs for the
	// dashboard's pending-consent banner and approve/deny actions. The query
	// string carries the session-user scoping (status=pending&userId=... on the
	// list, userId=... on approve/deny for confused-deputy protection).
	mux.HandleFunc("GET /v1/consent-requests", requireToken(validator, audience, readScope, forwardToRegistry("GET", func(r *http.Request) string {
		return withUserID("/v1/consent-requests", r)
	})))
	mux.HandleFunc("POST /v1/consent-requests/{id}/approve", requireToken(validator, audience, writeScope, forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/consent-requests/"+r.PathValue("id")+"/approve", r)
	})))
	mux.HandleFunc("POST /v1/consent-requests/{id}/deny", requireToken(validator, audience, writeScope, forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/consent-requests/"+r.PathValue("id")+"/deny", r)
	})))

	mux.HandleFunc("POST /v1/agents/{id}/message", requireToken(validator, audience, writeScope, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// messaging an agent is a mutation (per the spec matrix) → manage authority.
		if !canManage(r.Context(), regClient, sdb, id, userIDFrom(r.Context())) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		sessionID, _ := body["sessionId"].(string)
		if sessionID == "" {
			sessionID = "default"
		}

		target := fmt.Sprintf("http://%s-svc:8080/agents/chat/%s", id, sessionID)
		payload, _ := json.Marshal(body)
		req2, err := http.NewRequestWithContext(r.Context(), "POST", target, bytes.NewReader(payload))
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		req2.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			log.Printf("message forward to %s: %v", target, err)
			http.Error(w, "agent unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentv1alpha1.AddToScheme(scheme))
}

// controlPlaneBearerSource returns the bearer source the orchestrator presents to
// the registry (consent endpoints and control-plane-gated reads), built from the
// shared controlplane.BearerSource so it stays in lock-step with the operator and
// the registry's server-side authenticator. A misconfiguration is fatal at
// startup (the orchestrator cannot function without a valid control-plane
// credential when one is required).
func controlPlaneBearerSource() func() string {
	src, err := controlplane.BearerSource(context.Background())
	if err != nil {
		log.Fatalf("control-plane bearer: %v", err)
	}
	return src
}

// getEnv returns the value of the environment variable named by key, or def if
// the variable is unset or empty.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildSpawnValidator constructs the access-token validator used on the agent
// spawn path — the SAME tokenvalidator the resource servers (sample-api) use, so
// the agent's ACT-AS token is validated identically. tokenvalidator.New fetches
// the IdP JWKS at construction and returns an error if the IdP isn't reachable
// yet, so — like waitForRegistrySchema — we retry every 3s: in a fresh cluster
// the orchestrator and IdP start together and the orchestrator has nothing to do
// until the IdP can validate spawn tokens. The JWKS fetch is bounded (the
// tokenvalidator uses a 10s HTTP client), so a hung socket or misconfigured
// IS_JWKS_URL surfaces as a retryable error — the loop logs and retries every
// 3s rather than blocking startup on a hung connection.
func buildSpawnValidator(ctx context.Context) tokenvalidator.TokenValidator {
	issuer := getEnv("IS_ISSUER", "http://keycloak:8080/realms/spawnly")
	jwksURL := getEnv("IS_JWKS_URL", "http://keycloak:8080/realms/spawnly/protocol/openid-connect/certs")
	for attempt := 1; ; attempt++ {
		v, err := tokenvalidator.New(ctx, issuer, jwksURL)
		if err == nil {
			log.Printf("spawn token validator ready after %d attempt(s)", attempt)
			return v
		}
		log.Printf("waiting for spawn token validator JWKS (attempt %d): %v", attempt, err)
		time.Sleep(3 * time.Second)
	}
}

// waitForRegistrySchema blocks until the registry's GET /v1/schema returns 200,
// i.e. the registry has applied the SpiceDB schema (Phase 2). It logs and
// retries indefinitely rather than failing fast: in a fresh cluster the registry
// and orchestrator start together, so the registry simply may not be up yet, and
// the orchestrator has nothing useful to do until it is.
func waitForRegistrySchema(registryURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 1; ; attempt++ {
		resp, err := client.Get(registryURL + "/v1/schema")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Printf("registry schema ready after %d attempt(s)", attempt)
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		log.Printf("waiting for registry schema (attempt %d): %v", attempt, err)
		time.Sleep(3 * time.Second)
	}
}

func main() {
	spicedbEndpoint := os.Getenv("SPICEDB_ENDPOINT")
	if spicedbEndpoint == "" {
		spicedbEndpoint = "spicedb:50051"
	}
	spicedbPSK := os.Getenv("SPICEDB_PSK")
	if spicedbPSK == "" {
		spicedbPSK = "poc-secret"
	}

	// The SpiceDB client is used for spawn-time authorization checks. The schema
	// itself is now owned and written by the registry on its boot (Phase 2:
	// single-writer schema ownership), not here — so this service must come up
	// after the registry has applied the schema.
	sdb, err := spicedb.New(spicedbEndpoint, spicedbPSK)
	if err != nil {
		log.Fatalf("spicedb connect: %v", err)
	}

	cfg := ctrl.GetConfigOrDie()
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	// The controller-runtime client cannot stream pod logs, so we also build a
	// client-go clientset for log access.
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("k8s clientset: %v", err)
	}

	registryURL := os.Getenv("REGISTRY_URL")
	if registryURL == "" {
		registryURL = "http://registry:8080"
	}

	// The registry is the single writer of the SpiceDB schema (Phase 2) and the
	// orchestrator's spawn-time CheckPermission needs that schema present. Block
	// until the registry reports it has applied a schema, so a cold-boot spawn
	// can't hit SpiceDB before any schema exists.
	waitForRegistrySchema(registryURL)

	// Inbound auth: the single JWKS validator serves both the agent /spawn path
	// (scope orchestrator:spawn) and the dashboard-facing routes (scopes
	// orchestrator:read / orchestrator:write). All tokens carry aud=orchestrator.
	// The dashboard path is now ALWAYS token-authenticated via the human's
	// delegated access token — no more X-Control-Plane-Token or open tier.
	v := buildSpawnValidator(context.Background())
	audience := getEnv("SPAWN_OIDC_AUDIENCE", "orchestrator")
	spawnScope := getEnv("SPAWN_SCOPE", "orchestrator:spawn")
	readScope := getEnv("ORCH_READ_SCOPE", "orchestrator:read")
	writeScope := getEnv("ORCH_WRITE_SCOPE", "orchestrator:write")

	mux := buildMux(k8s, clientset, sdb, registryURL, v, audience, spawnScope, readScope, writeScope)
	log.Println("orchestrator listening on :8080")
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // Slowloris defense; no risk to legit responses.
		ReadTimeout:       30 * time.Second, // request bodies are all small JSON.
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: /v1/agents/{id}/logs (reads pod logs) and
		// /v1/agents/{id}/message (forwards to an agent / slow LLM) are
		// legitimately long-lived; a write deadline would truncate them.
	}
	log.Fatal(srv.ListenAndServe())
}
