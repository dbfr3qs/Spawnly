// cmd/orchestrator/main.go
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
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

	"golang.org/x/oauth2/clientcredentials"
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

func buildMux(k8s client.Client, clientset kubernetes.Interface, sdb spicedb.Client, registryURL string, spawnValidator tokenvalidator.TokenValidator, spawnAudience, spawnScope, controlPlaneToken string) *http.ServeMux {
	mux := http.NewServeMux()

	regClient := registry.New(registryURL)
	evtClient := events.New(registryURL)

	mux.HandleFunc("POST /spawn", func(w http.ResponseWriter, r *http.Request) {
		var req SpawnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Two authenticated paths derive the spawn's authority; the body is never
		// trusted for who/where except as noted.
		//
		//   Agent path (Authorization header present): validate the agent's ACT-AS
		//   access token with the same tokenvalidator the resource servers use. The
		//   token's `sub` is the human owner ("user:<userId>", authoritative) and its
		//   outermost `act.sub` is the acting agent's SPIFFE URI (ActingAgentName is
		//   the bare parent agent id). The child's userId/parentId come from those
		//   claims and its tenantId from the PARENT's registry record — body
		//   userId/parentId/tenantId are ignored, so an agent can't forge who it
		//   spawns for or graft the child elsewhere. Audience + scope are enforced so
		//   a token minted for another resource server (e.g. sample-api) can't be
		//   replayed at /spawn.
		//
		//   Dashboard path (no Authorization header): authenticate via a SEPARATE
		//   X-Control-Plane-Token header (not Authorization — that key drives the
		//   agent path, so sharing it would let an attacker bypass the JWT by simply
		//   omitting the token). The body userId is trusted (the dashboard injects the
		//   session user), but parentId is rejected — human spawns are top-level only.
		//   An empty controlPlaneToken is the demo/none tier: the dashboard path runs
		//   open.
		if r.Header.Get("Authorization") != "" {
			rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if rawToken == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			claims, err := spawnValidator.ValidateAccessToken(r.Context(), rawToken)
			if err != nil {
				log.Printf("spawn: token validation failed: %v", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Reject delegation-only tokens and tokens minted for a different audience
			// (the cross-audience-replay defense).
			if claims.TokenUse == "delegation" {
				log.Printf("spawn: rejecting token_use=delegation")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !claims.HasAudience(spawnAudience) {
				log.Printf("spawn: rejecting token: aud %v does not contain %q", claims.Audience, spawnAudience)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !claims.HasScope(spawnScope) {
				log.Printf("spawn: rejecting token: missing scope %q (have %v)", spawnScope, claims.Scopes)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			userID := strings.TrimPrefix(claims.User, "user:")
			if userID == "" {
				log.Printf("spawn: token has no user subject")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
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
			if controlPlaneToken != "" {
				got := r.Header.Get("X-Control-Plane-Token")
				if subtle.ConstantTimeCompare([]byte(got), []byte(controlPlaneToken)) != 1 {
					log.Printf("spawn: invalid control-plane token on dashboard spawn")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			// controlPlaneToken == "" means the demo/none tier — dashboard path open.
			if req.ParentID != "" {
				http.Error(w, "parentId not allowed on dashboard spawn", http.StatusBadRequest)
				return
			}
			// req.UserID is trusted here (dashboard injected it from the session);
			// the existing "userId required" check below still applies.
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

	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := regClient.ListAgents(r.Context())
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusBadGateway)
			return
		}
		// Ownership scoping: when the caller supplies a userId (the dashboard
		// injects the session user), only return that user's agents so one user
		// can't enumerate another's. Empty userId is internal/back-compat and
		// returns everything.
		if userId := r.URL.Query().Get("userId"); userId != "" {
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
	})

	mux.HandleFunc("GET /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ownsAgent(r.Context(), regClient, id, r.URL.Query().Get("userId")) {
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
	})

	mux.HandleFunc("GET /v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ownsAgent(r.Context(), regClient, id, r.URL.Query().Get("userId")) {
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
	})

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
	mux.HandleFunc("DELETE /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		userID := r.URL.Query().Get("userId")
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
	})

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		target := registryURL + "/v1/agents/" + id + "/dismiss"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req2, err := http.NewRequestWithContext(r.Context(), "POST", target, nil)
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
	})

	// controlPlaneBearer yields the bearer the orchestrator (a trusted
	// control-plane caller) presents on every forwarded consent request. Its
	// shape follows CONTROL_PLANE_AUTH and is empty in the local demo (the
	// registry then enforces nothing). Built once; the oidc source auto-refreshes.
	controlPlaneBearer := controlPlaneBearerSource()

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
	withQuery := func(path string, r *http.Request) string {
		if r.URL.RawQuery != "" {
			return path + "?" + r.URL.RawQuery
		}
		return path
	}

	// revoke/resume are cascading authorization actions owned by the registry.
	mux.HandleFunc("POST /v1/agents/{id}/revoke", forwardToRegistry("POST", func(r *http.Request) string {
		return withQuery("/v1/agents/"+r.PathValue("id")+"/revoke", r)
	}))
	mux.HandleFunc("POST /v1/agents/{id}/resume", forwardToRegistry("POST", func(r *http.Request) string {
		return withQuery("/v1/agents/"+r.PathValue("id")+"/resume", r)
	}))

	mux.HandleFunc("GET /v1/templates", forwardToRegistry("GET", func(*http.Request) string {
		return "/v1/templates"
	}))
	// Template management (control-plane-auth'd at the registry): create, update
	// status, and delete. forwardToRegistry streams the body through, so the
	// body-carrying create/update and the bodyless delete all use it.
	mux.HandleFunc("POST /v1/templates", forwardToRegistry("POST", func(*http.Request) string {
		return "/v1/templates"
	}))
	mux.HandleFunc("PATCH /v1/templates/{agentType}", forwardToRegistry("PATCH", func(r *http.Request) string {
		return "/v1/templates/" + r.PathValue("agentType")
	}))
	mux.HandleFunc("DELETE /v1/templates/{agentType}", forwardToRegistry("DELETE", func(r *http.Request) string {
		return "/v1/templates/" + r.PathValue("agentType")
	}))

	// Stored spawn consents: registry passthroughs for the dashboard's consent
	// management view (list per user, revoke to force a re-prompt next spawn).
	// The query string carries the dashboard's session-user scoping.
	mux.HandleFunc("GET /v1/consents", forwardToRegistry("GET", func(r *http.Request) string {
		return withQuery("/v1/consents", r)
	}))
	mux.HandleFunc("POST /v1/consents/{id}/revoke", forwardToRegistry("POST", func(r *http.Request) string {
		return withQuery("/v1/consents/"+r.PathValue("id")+"/revoke", r)
	}))

	// Brokered consent requests (Phase 5b): registry passthroughs for the
	// dashboard's pending-consent banner and approve/deny actions. The query
	// string carries the session-user scoping (status=pending&userId=... on the
	// list, userId=... on approve/deny for confused-deputy protection).
	mux.HandleFunc("GET /v1/consent-requests", forwardToRegistry("GET", func(r *http.Request) string {
		return withQuery("/v1/consent-requests", r)
	}))
	mux.HandleFunc("POST /v1/consent-requests/{id}/approve", forwardToRegistry("POST", func(r *http.Request) string {
		return withQuery("/v1/consent-requests/"+r.PathValue("id")+"/approve", r)
	}))
	mux.HandleFunc("POST /v1/consent-requests/{id}/deny", forwardToRegistry("POST", func(r *http.Request) string {
		return withQuery("/v1/consent-requests/"+r.PathValue("id")+"/deny", r)
	}))

	mux.HandleFunc("POST /v1/agents/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ownsAgent(r.Context(), regClient, id, r.URL.Query().Get("userId")) {
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
	})

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

// controlPlaneBearerSource returns a function yielding the current bearer the
// orchestrator presents to the registry's consent endpoints, selected by
// CONTROL_PLANE_AUTH (must match the registry's setting):
//
//	none/unset    -> "" (no header; the registry runs consent open)
//	shared-secret -> the static CONTROL_PLANE_TOKEN
//	oidc          -> a client-credentials access token, fetched and refreshed
//	                 automatically by the oauth2 TokenSource
func controlPlaneBearerSource() func() string {
	switch v := os.Getenv("CONTROL_PLANE_AUTH"); v {
	case "", "none":
		return func() string { return "" }
	case "shared-secret":
		token := os.Getenv("CONTROL_PLANE_TOKEN")
		if token == "" {
			log.Fatalf("CONTROL_PLANE_TOKEN required when CONTROL_PLANE_AUTH=shared-secret")
		}
		return func() string { return token }
	case "oidc":
		scope := os.Getenv("CONTROL_PLANE_SCOPE")
		if scope == "" {
			scope = "registry.consent"
		}
		cfg := clientcredentials.Config{
			ClientID:     os.Getenv("CONTROL_PLANE_CLIENT_ID"),
			ClientSecret: os.Getenv("CONTROL_PLANE_CLIENT_SECRET"),
			TokenURL:     os.Getenv("CONTROL_PLANE_TOKEN_URL"),
			Scopes:       strings.Fields(scope),
		}
		if cfg.ClientID == "" || cfg.TokenURL == "" {
			log.Fatalf("CONTROL_PLANE_CLIENT_ID and CONTROL_PLANE_TOKEN_URL required when CONTROL_PLANE_AUTH=oidc")
		}
		ts := cfg.TokenSource(context.Background())
		return func() string {
			tok, err := ts.Token()
			if err != nil {
				log.Printf("control-plane token fetch failed: %v", err)
				return ""
			}
			return tok.AccessToken
		}
	default:
		log.Fatalf("unknown CONTROL_PLANE_AUTH %q", v)
		return nil // unreachable
	}
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
// until the IdP can validate spawn tokens. Note the fetch uses the default HTTP
// client (no timeout), so a hung socket or a permanently-misconfigured
// IS_JWKS_URL blocks startup (logged each attempt) rather than crash-looping.
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

	// Inbound spawn auth: the agent path validates the agent's ACT-AS access
	// token (waits for the IdP's JWKS) and enforces audience + scope; the
	// dashboard path authenticates via a separate X-Control-Plane-Token header.
	// An empty token (CONTROL_PLANE_AUTH != shared-secret) runs the dashboard path
	// open (demo/none tier).
	spawnValidator := buildSpawnValidator(context.Background())
	spawnAudience := getEnv("SPAWN_OIDC_AUDIENCE", "orchestrator")
	spawnScope := getEnv("SPAWN_SCOPE", "orchestrator:spawn")
	controlPlaneToken := ""
	if os.Getenv("CONTROL_PLANE_AUTH") == "shared-secret" {
		controlPlaneToken = os.Getenv("CONTROL_PLANE_TOKEN")
	}

	mux := buildMux(k8s, clientset, sdb, registryURL, spawnValidator, spawnAudience, spawnScope, controlPlaneToken)
	log.Println("orchestrator listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
