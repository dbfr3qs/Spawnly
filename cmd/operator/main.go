// cmd/operator/main.go
package main

import (
	"context"
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

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
	isTokenURL := getenv("IS_TOKEN_URL", "http://keycloak:8080/realms/spawnly/protocol/openid-connect/token")
	sampleAPIURL := getenv("SAMPLE_API_URL", "http://sample-api-a:8080")
	apiAURL := getenv("API_A_URL", "http://sample-api-a:8080")
	apiBURL := getenv("API_B_URL", "http://sample-api-b:8080")
	orchestratorURL := getenv("ORCHESTRATOR_URL", "http://orchestrator:8080")

	// Select how workload identity is delivered into agent pods. Default is
	// SPIRE (csi.spiffe.io); other attestors plug in behind ATTESTOR.
	var identityInjector operator.IdentityInjector
	switch v := getenv("ATTESTOR", "spiffe"); v {
	case "spiffe":
		// SPIFFE_CSI_DRIVER lets the substrate overlay point at its own CSI driver
		// (csi.substrate.spiffe.io) so substrate pods mount the substrate SPIRE
		// agent's socket on a shared node; empty ⇒ the platform default.
		identityInjector = operator.SpiffeInjector{Driver: getenv("SPIFFE_CSI_DRIVER", "")}
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

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	}
	// WATCH_NAMESPACE scopes the reconciler's cache+watch to a single namespace.
	// Unset (the platform default) keeps today's cluster-wide behavior verbatim.
	// The remote-substrate overlay sets it to `substrate-sim` so a SECOND operator
	// can run there without racing the platform operator over `default` pods.
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
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
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Registry:        registry.NewWithTokenSource(registryURL, controlPlaneBearer),
		RegistryURL:     registryURL,
		ISTokenURL:      isTokenURL,
		SampleAPIURL:    sampleAPIURL,
		APIAUrl:         apiAURL,
		APIBUrl:         apiBURL,
		OrchestratorURL: orchestratorURL,
		// Present the control-plane bearer on event posts: the events endpoint is
		// now authenticated, and the reconciler posts to arbitrary agent ids, so it
		// authenticates as a control-plane caller (empty/no-op under auth=none).
		EventsClient:     events.NewWithToken(registryURL, controlPlaneBearer),
		Clientset:        clientset,
		IdentityInjector: identityInjector,
		SidecarImage:     getenv("SIDECAR_IMAGE", "agent-sidecar:latest"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentWorkload")
		os.Exit(1)
	}

	// Stale-agent reconciliation. After a cluster rebuild the durable registry
	// still holds agent records whose workloads no longer exist; the sweep marks
	// those orphans `failed` so the dashboard never shows phantom-`active` agents.
	// STALE_SWEEP=off is the kill-switch; STALE_SWEEP_INTERVAL (a Go duration)
	// turns the startup-only sweep into a periodic resync (empty/0 = startup-only).
	if getenv("STALE_SWEEP", "on") != "off" {
		var sweepInterval time.Duration
		if v := os.Getenv("STALE_SWEEP_INTERVAL"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				setupLog.Error(err, "invalid STALE_SWEEP_INTERVAL", "value", v)
				os.Exit(1)
			}
			sweepInterval = d
		}
		sweeper := &operator.StaleSweeper{
			// A distinct registry client (not the reconciler's) keeps the sweep's
			// lifecycle independent; both share the same control-plane bearer.
			Registry: registry.NewWithTokenSource(registryURL, controlPlaneBearer),
			// Control-plane bearer: the sweep posts reconcile.stale to arbitrary
			// orphaned agent ids, so it authenticates as a control-plane caller.
			Events:    events.NewWithToken(registryURL, controlPlaneBearer),
			Reader:    mgr.GetClient(),
			Namespace: getenv("AGENT_NAMESPACE", "default"),
		}
		// Register as a manager Runnable. A RunnableFunc implements neither
		// hasCache nor LeaderElectionRunnable, so runnables.Add routes it to the
		// LeaderElection group — which manager.Start only starts AFTER
		// runnables.Caches.Start has returned, i.e. after WaitForCacheSync. That
		// ordering matters: sweeping against an unsynced cache would List zero
		// AgentWorkloads and fail EVERY live agent. And even if a refactor moved
		// this off the manager, the real backstop is in the cache itself: a
		// cache-backed List lazily creates the AgentWorkload informer and BLOCKS
		// until that informer has synced (cache/internal/informers.go Get →
		// WaitForCacheSync), erroring on ctx timeout — which trips the sweep's
		// list-error fail-safe abort rather than returning an empty list. Do not
		// swap the Reader for a live (non-cached) client without rechecking both
		// guards.
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			// Startup sweep, retried until it succeeds once. On a cluster rebuild
			// the operator and registry boot concurrently, so the first attempts
			// routinely hit a not-yet-ready registry and trip the fail-safe abort.
			// Without retry the phantom-active records — the exact target of this
			// feature — would persist until the next operator restart (no manifest
			// sets STALE_SWEEP_INTERVAL, so startup-only is the default path).
			// Capped exponential backoff, forever until first success or shutdown:
			// the sweep is cheap and idempotent, and giving up would silently leave
			// the registry unreconciled.
			backoff := 5 * time.Second
			const maxBackoff = 2 * time.Minute
			trigger := "startup"
			for {
				err := sweeper.Sweep(ctx, trigger)
				if err == nil {
					break
				}
				setupLog.Error(err, "stale-agent sweep failed; will retry", "backoff", backoff.String())
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				trigger = "retry"
			}
			if sweepInterval <= 0 {
				return nil // startup-only
			}
			ticker := time.NewTicker(sweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					// Periodic resync failures are logged and retried on the next
					// tick — no extra backoff loop needed once the first sweep landed.
					if err := sweeper.Sweep(ctx, "resync"); err != nil {
						setupLog.Error(err, "periodic stale-agent sweep")
					}
				}
			}
		})); err != nil {
			setupLog.Error(err, "unable to add stale-agent sweeper")
			os.Exit(1)
		}
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
