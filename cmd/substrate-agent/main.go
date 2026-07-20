// Command substrate-agent is the consumer-side adapter of the remote-substrate
// feature (bring-your-own-compute): the Spawnly component a customer runs in
// their own Kubernetes cluster. It holds a persistent, client-initiated
// WebSocket to the control-plane orchestrator, authenticated by its org-scoped
// SPIRE SVID, and turns dispatched spawn/kill commands into local AgentWorkload
// CRs (materialized by the reused internal/operator reconciler) plus org-scoped
// SPIRE entries authored via the CP-mediated Entry API. On every (re)connect it
// reconciles desired-vs-actual from its org's registry records — the records are
// the source of truth, there is no durable queue.
//
// It is an ADAPTER: it depends on the platform's neutral contract (the channel
// protocol, the Entry API, the registry list, the AgentWorkload CRD), never the
// reverse ([[dependency-direction-rule]]).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/substrate/devca"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	trustDomain := getenv("SPIRE_TRUST_DOMAIN", "cluster.local")
	namespace := getenv("SUBSTRATE_NAMESPACE", "substrate-sim")
	audience := getenv("SUBSTRATE_SVID_AUDIENCE", "orchestrator")
	orchestratorURL := strings.TrimRight(getenv("ORCHESTRATOR_URL", "http://orchestrator.default:8080"), "/")
	socketPath := getenv("SPIFFE_ENDPOINT_SOCKET", "unix:///spiffe-workload-api/spire-agent.sock")
	leafFile := getenv("SUBSTRATE_LEAF_CERT_FILE", "/run/spire/identity/svid.crt")
	workloadSA := getenv("SUBSTRATE_WORKLOAD_SA", "default")

	// Derive our org + x509pop node id from the mounted leaf CERT (never a claim):
	// the CP-issued leaf's CommonName is the org, and its SHA-1 fingerprint is the
	// node the org's workload entries parent to. This is the SAME derivation the
	// connect flow used, so the ids agree by construction.
	leafPEM, err := os.ReadFile(leafFile)
	if err != nil {
		log.Fatalf("substrate-agent: read leaf cert %s: %v", leafFile, err)
	}
	org, err := devca.OrgFromLeaf(leafPEM)
	if err != nil {
		log.Fatalf("substrate-agent: derive org from leaf: %v", err)
	}
	nodeID, err := devca.NodeIDFromLeaf(trustDomain, leafPEM)
	if err != nil {
		log.Fatalf("substrate-agent: derive node id from leaf: %v", err)
	}
	log.Printf("substrate-agent: org=%s node=%s namespace=%s", org, nodeID, namespace)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentv1alpha1.AddToScheme(scheme))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("substrate-agent: kube config: %v", err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("substrate-agent: kube client: %v", err)
	}

	source := &attestor.SpiffeSource{SocketPath: socketPath}
	cp := newCPClient(orchestratorURL, audience, source)

	a := &agent{
		trustDomain:    trustDomain,
		org:            org,
		namespace:      namespace,
		nodeID:         nodeID,
		serviceAccount: workloadSA,
		store:          &k8sWorkloadStore{c: k8s, namespace: namespace},
		entries:        cp,
		lister:         cp,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	channelURL := wsURL(orchestratorURL) + "/v1/substrate/channel"
	log.Printf("substrate-agent: dialing channel %s", channelURL)
	a.serve(ctx, dialer{channelURL: channelURL, audience: audience, source: source})
	log.Println("substrate-agent: exiting")
}

// wsURL rewrites an http(s) base to the ws(s) scheme for the WebSocket dial.
func wsURL(httpBase string) string {
	switch {
	case strings.HasPrefix(httpBase, "https://"):
		return "wss://" + strings.TrimPrefix(httpBase, "https://")
	case strings.HasPrefix(httpBase, "http://"):
		return "ws://" + strings.TrimPrefix(httpBase, "http://")
	default:
		return httpBase
	}
}
