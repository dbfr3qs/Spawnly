// cmd/operator/main.go
package main

import (
	"context"
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/operator"
	"github.com/spawnly/platform/internal/registry"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentv1alpha1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "Address the health probe endpoint binds to.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	registryURL := getenv("REGISTRY_URL", "http://registry:8080")
	isTokenURL := getenv("IS_TOKEN_URL", "http://identity-server:8080/connect/token")
	sampleAPIURL := getenv("SAMPLE_API_URL", "http://sample-api-a:8080")
	apiAURL := getenv("API_A_URL", "http://sample-api-a:8080")
	apiBURL := getenv("API_B_URL", "http://sample-api-b:8080")
	orchestratorURL := getenv("ORCHESTRATOR_URL", "http://orchestrator:8080")

	// Select how workload identity is delivered into agent pods. Default is
	// SPIRE (csi.spiffe.io); other attestors plug in behind ATTESTOR.
	var identityInjector operator.IdentityInjector
	switch v := getenv("ATTESTOR", "spiffe"); v {
	case "spiffe":
		identityInjector = operator.SpiffeInjector{}
	case "aws-sts":
		identityInjector = operator.AwsInjector{
			ServiceAccount: getenv("AWS_AGENT_SERVICE_ACCOUNT", "spawnly-agent"),
			Region:         getenv("AWS_REGION", ""),
		}
	case "aws-stsweb":
		identityInjector = operator.StsWebInjector{
			ServiceAccount: getenv("AWS_AGENT_SERVICE_ACCOUNT", "spawnly-agent"),
			Region:         getenv("AWS_REGION", ""),
			Audience:       getenv("STSWEB_AUDIENCE", "spawnly"),
		}
	default:
		setupLog.Error(nil, "unknown ATTESTOR", "value", v)
		os.Exit(1)
	}

	cfg := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	// Control-plane bearer for the operator's registry calls (template reads,
	// PATCH status). Uses the shared source so all three tiers — none,
	// shared-secret, and oidc — behave identically to the orchestrator and match
	// the registry's server-side authenticator.
	controlPlaneBearer, err := controlplane.BearerSource(context.Background())
	if err != nil {
		setupLog.Error(err, "unable to build control-plane bearer source")
		os.Exit(1)
	}

	if err = (&operator.AgentWorkloadReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Registry:         registry.NewWithTokenSource(registryURL, controlPlaneBearer),
		RegistryURL:      registryURL,
		ISTokenURL:       isTokenURL,
		SampleAPIURL:     sampleAPIURL,
		APIAUrl:          apiAURL,
		APIBUrl:          apiBURL,
		OrchestratorURL:  orchestratorURL,
		EventsClient:     events.New(registryURL),
		Clientset:        clientset,
		IdentityInjector: identityInjector,
		SidecarImage:     getenv("SIDECAR_IMAGE", "agent-sidecar:latest"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentWorkload")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
