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
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

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

func buildMux(k8s client.Client, clientset kubernetes.Interface, sdb spicedb.Client, registryURL string) *http.ServeMux {
	mux := http.NewServeMux()

	regClient := registry.New(registryURL)
	evtClient := events.New(registryURL)

	mux.HandleFunc("POST /spawn", func(w http.ResponseWriter, r *http.Request) {
		var req SpawnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.UserID == "" {
			http.Error(w, "userId is required", http.StatusBadRequest)
			return
		}
		if req.AgentType == "" {
			req.AgentType = "worker"
		}

		tpl, err := regClient.GetTemplate(r.Context(), req.AgentType)
		if err != nil {
			log.Printf("get template %s: %v", req.AgentType, err)
			http.Error(w, "unknown agent type", http.StatusBadRequest)
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agents)
	})

	mux.HandleFunc("GET /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
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

	mux.HandleFunc("DELETE /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		aw := &agentv1alpha1.AgentWorkload{}
		aw.Name = id
		aw.Namespace = "default"
		if err := k8s.Delete(r.Context(), aw); err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		req2, err := http.NewRequestWithContext(r.Context(), "POST", registryURL+"/v1/agents/"+id+"/dismiss", nil)
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

	// forwardToRegistry relays a bodyless request to the registry (which owns
	// agent lineage, templates, and consents) and copies back the registry's
	// status and JSON body. path builds the registry path from the inbound
	// request (path values, query string).
	forwardToRegistry := func(method string, path func(r *http.Request) string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			req2, err := http.NewRequestWithContext(r.Context(), method, registryURL+path(r), nil)
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
			w.Header().Set("Content-Type", "application/json")
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
		return "/v1/agents/" + r.PathValue("id") + "/revoke"
	}))
	mux.HandleFunc("POST /v1/agents/{id}/resume", forwardToRegistry("POST", func(r *http.Request) string {
		return "/v1/agents/" + r.PathValue("id") + "/resume"
	}))

	mux.HandleFunc("GET /v1/templates", forwardToRegistry("GET", func(*http.Request) string {
		return "/v1/templates"
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

	mux.HandleFunc("POST /v1/agents/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

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

	mux := buildMux(k8s, clientset, sdb, registryURL)
	log.Println("orchestrator listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
