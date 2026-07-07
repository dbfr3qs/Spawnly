// cmd/agent-sidecar/config_test.go
package main

import (
	"testing"

	"github.com/spawnly/platform/internal/registry"
)

func baseCfg() config {
	return config{
		agentType:   "weather-monitor",
		registryURL: "http://registry:8080",
		isTokenURL:  "http://keycloak/token",
	}
}

// TestValidateConfig_Placement pins the Phase-10 remote-mode invariants and the
// SPIFFE socket default behaviour.
func TestValidateConfig_Placement(t *testing.T) {
	t.Run("k8s default applies SPIFFE socket", func(t *testing.T) {
		cfg := baseCfg() // placement "" == k8s
		if err := validateConfig(&cfg); err != nil {
			t.Fatalf("validateConfig: %v", err)
		}
		if cfg.socketPath == "" {
			t.Error("k8s placement must default the SPIFFE socket")
		}
	})

	t.Run("remote requires AGENT_ID", func(t *testing.T) {
		cfg := baseCfg()
		cfg.placement = registry.PlacementRemote
		// No agentID.
		if err := validateConfig(&cfg); err == nil {
			t.Fatal("remote placement without AGENT_ID must error")
		}
	})

	t.Run("remote with AGENT_ID skips SPIFFE socket default", func(t *testing.T) {
		cfg := baseCfg()
		cfg.placement = registry.PlacementRemote
		cfg.agentID = "remote-abc123"
		if err := validateConfig(&cfg); err != nil {
			t.Fatalf("validateConfig: %v", err)
		}
		if cfg.socketPath != "" {
			t.Errorf("remote placement must NOT default the SPIFFE socket, got %q", cfg.socketPath)
		}
	})

	t.Run("unknown placement rejected", func(t *testing.T) {
		cfg := baseCfg()
		cfg.placement = "moon"
		if err := validateConfig(&cfg); err == nil {
			t.Fatal("unknown placement must error")
		}
	})

	t.Run("missing required URLs rejected", func(t *testing.T) {
		cfg := config{agentType: "x"}
		if err := validateConfig(&cfg); err == nil {
			t.Fatal("missing REGISTRY_URL/IS_TOKEN_URL must error")
		}
	})
}
