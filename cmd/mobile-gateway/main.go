// Command mobile-gateway is the user-facing edge a mobile app talks to to answer
// CIBA spawn-consent prompts. It proxies consent actions to the orchestrator's
// user-scoped endpoints (which stay the authorization authority) and owns a
// device registry plus, in later phases, the push fan-out and event stream.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/mobilegateway"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildValidator fetches the IdP JWKS, retrying until the IdP is reachable —
// the gateway can't validate a single mobile token until then. Mirrors the
// orchestrator's buildSpawnValidator (the JWKS fetch is bounded inside
// tokenvalidator.New, so a hung IdP surfaces as a retryable error).
func buildValidator(ctx context.Context) tokenvalidator.TokenValidator {
	issuer := getEnv("IS_ISSUER", "http://identity-server")
	jwksURL := getEnv("IS_JWKS_URL", "http://identity-server/.well-known/openid-configuration/jwks")
	for attempt := 1; ; attempt++ {
		v, err := tokenvalidator.New(ctx, issuer, jwksURL)
		if err == nil {
			return v
		}
		log.Printf("waiting for mobile token validator JWKS (attempt %d): %v", attempt, err)
		time.Sleep(3 * time.Second)
	}
}

// buildControlPlaneAuth selects the authenticator for the /internal/notify
// webhook from CONTROL_PLANE_AUTH (none|shared-secret), mirroring the registry's
// own selector so the registry→gateway hop uses the one shared control-plane
// secret. Local/demo runs open (none); AWS uses shared-secret.
func buildControlPlaneAuth() controlplane.Authenticator {
	switch v := getEnv("CONTROL_PLANE_AUTH", "none"); v {
	case "none":
		return controlplane.AllowAll()
	case "shared-secret":
		token := getEnv("CONTROL_PLANE_TOKEN", "")
		if token == "" {
			log.Fatalf("CONTROL_PLANE_TOKEN required when CONTROL_PLANE_AUTH=shared-secret")
		}
		return controlplane.NewSharedSecret(token)
	default:
		log.Fatalf("unknown CONTROL_PLANE_AUTH %q", v)
		return nil // unreachable
	}
}

// buildTransport selects the background-push transport from NOTIFIER. The dev
// transport (default) sends no external push — the SSE stream is the delivery —
// so local bootstrap needs no Apple/Google credentials. fcmapns reads its
// credentials from mounted secret files (never the image/env literals).
func buildTransport() mobilegateway.Transport {
	switch v := getEnv("NOTIFIER", "dev"); v {
	case "dev":
		return mobilegateway.NoopTransport{}
	case "fcmapns":
		cfg := mobilegateway.FCMAPNsConfig{
			ServiceAccountJSON: mustReadFile("FCM_SERVICE_ACCOUNT_FILE"),
			APNsKeyP8:          mustReadFile("APNS_KEY_FILE"),
			APNsKeyID:          mustGetEnv("APNS_KEY_ID"),
			APNsTeamID:         mustGetEnv("APNS_TEAM_ID"),
			APNsBundleID:       mustGetEnv("APNS_BUNDLE_ID"),
			APNsProduction:     getEnv("APNS_PRODUCTION", "false") == "true",
		}
		t, err := mobilegateway.NewFCMAPNsTransport(cfg)
		if err != nil {
			log.Fatalf("fcmapns transport: %v", err)
		}
		return t
	default:
		log.Fatalf("unknown NOTIFIER %q (supported: dev, fcmapns)", v)
		return nil // unreachable
	}
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required when NOTIFIER=fcmapns", key)
	}
	return v
}

// mustReadFile reads a credential from the file path in the named env var. The
// credentials are mounted as secret files (Secret volumes / mounted SSM), so
// they never appear in env literals or the image.
func mustReadFile(pathEnv string) []byte {
	path := os.Getenv(pathEnv)
	if path == "" {
		log.Fatalf("%s is required when NOTIFIER=fcmapns", pathEnv)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s (%s): %v", pathEnv, path, err)
	}
	return b
}

func main() {
	orchestratorURL := getEnv("ORCHESTRATOR_URL", "http://orchestrator:8080")

	deps := mobilegateway.Deps{
		Validator:       buildValidator(context.Background()),
		Devices:         mobilegateway.NewMemoryDeviceStore(),
		OrchestratorURL: orchestratorURL,
		Audience:        getEnv("SPAWN_OIDC_AUDIENCE", "orchestrator"),
		ReadScope:       getEnv("ORCH_READ_SCOPE", "orchestrator:read"),
		WriteScope:      getEnv("ORCH_WRITE_SCOPE", "orchestrator:write"),
		ControlPlane:    buildControlPlaneAuth(),
		Hub:             mobilegateway.NewHub(),
		Transport:       buildTransport(),
	}

	// The control-plane webhook (/internal/notify) is served on a SEPARATE
	// internal port so a NetworkPolicy can lock it to the registry alone, while
	// the public :8080 surface (ALB / port-forward) stays open and token-gated.
	// Both servers share the one Deps — same Hub, device store, and transport —
	// so a notify on :8081 reaches an SSE subscriber on :8080.
	internalAddr := ":" + getEnv("INTERNAL_PORT", "8081")
	go func() {
		log.Printf("mobile-gateway internal (webhook) listening on %s", internalAddr)
		internal := &http.Server{
			Addr:              internalAddr,
			Handler:           mobilegateway.BuildInternalMux(deps),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		log.Fatal(internal.ListenAndServe())
	}()

	log.Println("mobile-gateway listening on :8080")
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mobilegateway.BuildMux(deps),
		ReadHeaderTimeout: 10 * time.Second, // Slowloris defense.
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the dev SSE stream is long-lived.
	}
	log.Fatal(srv.ListenAndServe())
}
