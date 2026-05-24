// cmd/orchestrator/main.go
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/spicedb"
)

type SpawnRequest struct {
	AgentType string `json:"agentType"`
	UserID    string `json:"userId"`
	TenantID  string `json:"tenantId"`
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

func buildMux(k8s client.Client, sdb spicedb.Client) *http.ServeMux {
	mux := http.NewServeMux()

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
				Lifecycle: "short-lived",
			},
		}

		if err := k8s.Create(r.Context(), aw); err != nil {
			log.Printf("create AgentWorkload: %v", err)
			http.Error(w, "failed to spawn agent", http.StatusInternalServerError)
			return
		}

		log.Printf("spawned workload %s for tenant %s user %s", workloadName, req.TenantID, req.UserID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(SpawnResponse{WorkloadName: workloadName})
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

	if err := sdb.WriteSchema(context.Background(), spicedbSchema); err != nil {
		log.Printf("warn: WriteSchema: %v (schema may already exist)", err)
	}

	k8s, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	mux := buildMux(k8s, sdb)
	log.Println("orchestrator listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
