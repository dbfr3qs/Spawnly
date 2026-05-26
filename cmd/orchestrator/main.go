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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/events"
	"github.com/agent-platform/poc/internal/registry"
	"github.com/agent-platform/poc/internal/spicedb"
)

type SpawnRequest struct {
	AgentType string `json:"agentType"`
	UserID    string `json:"userId"`
	TenantID  string `json:"tenantId"`
	Task      string `json:"task,omitempty"`
}

type SpawnResponse struct {
	WorkloadName string `json:"workloadName"`
}

const spicedbSchema = `
definition agent {}

definition tenant {
    relation agent: agent
    permission work_on = agent
}
`

func shortID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func buildMux(k8s client.Client, sdb spicedb.Client, registryURL string) *http.ServeMux {
	mux := http.NewServeMux()

	regClient := registry.New(registryURL)
	evtClient := events.New(registryURL)

	mux.HandleFunc("POST /spawn", func(w http.ResponseWriter, r *http.Request) {
		var req SpawnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.TenantID == "" || req.UserID == "" {
			http.Error(w, "tenantId and userId are required", http.StatusBadRequest)
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
		lifecycle := tpl.Runtime.Lifecycle
		if lifecycle == "" {
			lifecycle = "short-lived"
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
				AgentType: req.AgentType,
				UserID:    req.UserID,
				TenantID:  req.TenantID,
				Lifecycle: lifecycle,
				Task:      req.Task,
			},
		}

		if err := k8s.Create(r.Context(), aw); err != nil {
			log.Printf("create AgentWorkload: %v", err)
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

	sdb, err := spicedb.New(spicedbEndpoint, spicedbPSK)
	if err != nil {
		log.Fatalf("spicedb connect: %v", err)
	}

	// Retry schema write — SpiceDB may not be ready immediately on first start.
	for i := 1; i <= 10; i++ {
		if err := sdb.WriteSchema(context.Background(), spicedbSchema); err == nil {
			break
		} else if i == 10 {
			log.Fatalf("WriteSchema failed after 10 attempts: %v", err)
		} else {
			log.Printf("WriteSchema attempt %d/10 failed, retrying: %v", i, err)
			time.Sleep(3 * time.Second)
		}
	}

	k8s, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	registryURL := os.Getenv("REGISTRY_URL")
	if registryURL == "" {
		registryURL = "http://registry:8080"
	}

	mux := buildMux(k8s, sdb, registryURL)
	log.Println("orchestrator listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
