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

// ownsAgent reports whether userId owns the agent (its registry record's
// UserID). Empty userId never owns. Used to gate the read/interact endpoints
// the orchestrator fronts directly.
func ownsAgent(ctx context.Context, regClient registry.Client, id, userId string) bool {
	if userId == "" {
		return false
	}
	rec, err := regClient.GetAgent(ctx, id)
	return err == nil && rec.UserID == userId
}

// ctxKey is the private type for orchestrator request-context values.
type ctxKey int

const (
	ctxUserID ctxKey = iota
	ctxRoles
)

// userIDFrom returns the authenticated user id placed in the context by
// requireToken (the dashboard token's sub). Empty if the request was not
// token-authenticated — ownership checks then deny by construction (empty userId
// never owns), so this never silently widens access.
func userIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxUserID).(string)
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
		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxRoles, claims.Roles)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin gates a dashboard-facing handler behind an admin role in addition
// to a valid delegated token: it runs requireToken's token/audience/scope
// checks, then requires the token's `role` claim to include "admin" (else 403).
// The admin role is issued by IdentityServer (TestUsers) and rides in the
// orchestrator-audience access token via the orchestrator ApiResource's
// UserClaims. This is the orchestrator-side enforcement; the dashboard BFF
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
	evtClient := events.New(registryURL)

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

		if err := k8s.Create(r.Context(), aw); err != nil {
			log.Printf("create AgentWorkload: %v", err)
			http.Error(w, "failed to spawn agent", http.StatusInternalServerError)
			return
		}

		// Pre-register immediately so the agent appears in the UI with "pending"
		// status before the pod starts. The sidecar will overwrite this to "active".
		_ = regClient.PreRegisterAgent(r.Context(), registry.AgentRecord{
			AgentID:      workloadName,
			AgentType:    req.AgentType,
			TenantID:     req.TenantID,
			UserID:       req.UserID,
			Lifecycle:    lifecycle,
			SupportsChat: tpl.Runtime.SupportsChat,
			ParentID:     req.ParentID,
		})

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
		agents, err := regClient.ListAgents(r.Context())
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		// Ownership scoping: the token's userId (the authenticated human) limits
		// the result to this user's agents — no cross-user enumeration.
		if userId := userIDFrom(r.Context()); userId != "" {
			owned := make([]registry.AgentRecord, 0, len(agents))
			for _, a := range agents {
				if a.UserID == userId {
					owned = append(owned, a)
				}
			}
			agents = owned
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agents)
	}))

	mux.HandleFunc("GET /v1/agents/{id}/events", requireToken(validator, audience, readScope, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ownsAgent(r.Context(), regClient, id, userIDFrom(r.Context())) {
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
		if !ownsAgent(r.Context(), regClient, id, userIDFrom(r.Context())) {
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
		if !ownsAgent(ctx, regClient, id, userID) {
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

	// requireOwnedAgent is the primary ownership gate for the per-agent ops the
	// orchestrator forwards to the registry (dismiss/revoke/resume). It sits
	// INSIDE requireToken — which has already validated the access token and put
	// the user (token sub) in the context — and denies (404, never 403, so
	// ownership isn't enumerable) unless that user owns the {id} agent per its
	// registry record. This makes the validated sub authoritative, mirroring the
	// DELETE/events/logs/message handlers; the forwarded userId + the registry's
	// own agentOwnedBy check remain as a defense-in-depth backstop.
	requireOwnedAgent := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !ownsAgent(r.Context(), regClient, r.PathValue("id"), userIDFrom(r.Context())) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", requireToken(validator, audience, writeScope, requireOwnedAgent(func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("POST /v1/agents/{id}/revoke", requireToken(validator, audience, writeScope, requireOwnedAgent(forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/agents/"+r.PathValue("id")+"/revoke", r)
	}))))
	mux.HandleFunc("POST /v1/agents/{id}/resume", requireToken(validator, audience, writeScope, requireOwnedAgent(forwardToRegistry("POST", func(r *http.Request) string {
		return withUserID("/v1/agents/"+r.PathValue("id")+"/resume", r)
	}))))

	mux.HandleFunc("GET /v1/templates", requireToken(validator, audience, readScope, forwardToRegistry("GET", func(*http.Request) string {
		return "/v1/templates"
	})))
	// Non-admin spawn list: active types plus their requiresTenant flag, for the
	// spawn modal to decide whether to show the Tenant field. Same read scope as
	// the thin GET /v1/templates above (NOT admin) — the registry's detail=spawn
	// route is public, so forwarding the control-plane bearer here is harmless.
	// The downstream query is HARDCODED to detail=spawn so no client can coax this
	// read-scope route into the admin-only detail=full path.
	mux.HandleFunc("GET /v1/templates/spawn", requireToken(validator, audience, readScope, forwardToRegistry("GET", func(*http.Request) string {
		return "/v1/templates?detail=spawn"
	})))
	// Admin full-detail template list (incl. disabled) for the dashboard's Agent
	// Types admin view. Admin-gated (role=admin on top of read scope) at the
	// orchestrator; the BFF gates its /api/admin/templates twin with
	// requireAdmin too (defense in depth). forwardToRegistry carries the
	// orchestrator's control-plane bearer, so the registry's detail=full route
	// (control-plane gated) authorizes — the caller's admin role is checked
	// here, not relied on at the registry.
	mux.HandleFunc("GET /v1/admin/templates", requireAdmin(validator, audience, readScope, forwardToRegistry("GET", func(*http.Request) string {
		return "/v1/templates?detail=full"
	})))
	// Template management (control-plane-auth'd at the registry, admin-gated at
	// both the orchestrator and the dashboard BFF): create, update status, and
	// delete. requireAdmin requires role=admin on top of the write scope, so a
	// valid orchestrator:write token without the admin role gets 403. The thin
	// GET /v1/templates above stays on readScope (the spawn dropdown).
	// forwardToRegistry streams the body through, so the body-carrying
	// create/update and the bodyless delete all use it.
	mux.HandleFunc("POST /v1/templates", requireAdmin(validator, audience, writeScope, forwardToRegistry("POST", func(*http.Request) string {
		return "/v1/templates"
	})))
	mux.HandleFunc("PATCH /v1/templates/{agentType}", requireAdmin(validator, audience, writeScope, forwardToRegistry("PATCH", func(r *http.Request) string {
		return "/v1/templates/" + r.PathValue("agentType")
	})))
	mux.HandleFunc("DELETE /v1/templates/{agentType}", requireAdmin(validator, audience, writeScope, forwardToRegistry("DELETE", func(r *http.Request) string {
		return "/v1/templates/" + r.PathValue("agentType")
	})))

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
		if !ownsAgent(r.Context(), regClient, id, userIDFrom(r.Context())) {
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
	issuer := getEnv("IS_ISSUER", "http://identity-server")
	jwksURL := getEnv("IS_JWKS_URL", "http://identity-server/.well-known/openid-configuration/jwks")
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
