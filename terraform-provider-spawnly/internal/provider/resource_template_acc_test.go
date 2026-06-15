package provider

import (
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// testAccProtoV6ProviderFactories wires the in-process provider (built from
// New()) into the test framework over the protocol-v6 plugin server. No binary
// or dev_overrides install is involved — the acc test drives the provider
// directly.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"spawnly": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck guards the live-registry acceptance tests. The provider reads
// SPAWNLY_ENDPOINT / SPAWNLY_TOKEN from the environment itself; here we only
// assert the endpoint is present so a misconfigured run fails fast with a clear
// message rather than a confusing downstream API error.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("SPAWNLY_ENDPOINT") == "" {
		t.Fatal("SPAWNLY_ENDPOINT must be set for TF_ACC acceptance tests")
	}
}

// TestAccAgentTemplate_basic exercises the full resource lifecycle against a
// live registry: create + state/import verification, then an in-place update
// (meta field + status active->disabled), then implicit destroy (which the
// resource implements as disable-then-delete).
//
// It is gated on TF_ACC via helper/resource.Test, so a plain `go test ./...`
// (TF_ACC unset) skips it cleanly without needing a cluster.
func TestAccAgentTemplate_basic(t *testing.T) {
	// A unique agent_type per run so reruns (and parallel CI) don't collide on
	// a leftover template in a shared registry.
	agentType := fmt.Sprintf("tf-acc-worker-%d", rand.Intn(1_000_000))

	const resourceName = "spawnly_agent_template.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create + verify state attributes.
			{
				Config: testAccAgentTemplateConfig(agentType, "TF Acc Worker", "active"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "agent_type", agentType),
					resource.TestCheckResourceAttr(resourceName, "version", "1.0.0"),
					resource.TestCheckResourceAttr(resourceName, "status", "active"),
					resource.TestCheckResourceAttr(resourceName, "requires_tenant", "false"),
					resource.TestCheckResourceAttr(resourceName, "meta.display_name", "TF Acc Worker"),
					resource.TestCheckResourceAttr(resourceName, "runtime_spec.image", "agent-go-worker:latest"),
					resource.TestCheckResourceAttr(resourceName, "runtime_spec.resources.cpu_limits", "500m"),
				),
			},
			// Import by agent_type and verify state round-trips. The resource has
			// no synthetic "id"; its identity is agent_type, so point the
			// harness's identifier check at that attribute.
			{
				ResourceName:                         resourceName,
				ImportState:                          true,
				ImportStateId:                        agentType,
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "agent_type",
			},
			// Update: change a meta field AND flip status active->disabled.
			{
				Config: testAccAgentTemplateConfig(agentType, "TF Acc Worker (updated)", "disabled"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "agent_type", agentType),
					resource.TestCheckResourceAttr(resourceName, "status", "disabled"),
					resource.TestCheckResourceAttr(resourceName, "meta.display_name", "TF Acc Worker (updated)"),
				),
			},
			// Implicit destroy at the end of the test exercises the
			// disable-then-delete path in the resource's Delete.
		},
	})
}

// testAccAgentTemplateConfig renders a minimal but complete template config
// with the given agent_type, meta display name, and status. endpoint/token are
// left to the SPAWNLY_ENDPOINT / SPAWNLY_TOKEN environment variables.
func testAccAgentTemplateConfig(agentType, displayName, status string) string {
	return fmt.Sprintf(`
provider "spawnly" {
  # endpoint and token sourced from SPAWNLY_ENDPOINT / SPAWNLY_TOKEN.
}

resource "spawnly_agent_template" "test" {
  agent_type      = %[1]q
  version         = "1.0.0"
  status          = %[3]q
  requires_tenant = false

  meta {
    display_name = %[2]q
    description  = "Created by the Spawnly Terraform provider acceptance tests."
  }

  runtime_spec {
    image         = "agent-go-worker:latest"
    lifecycle     = "short-lived"
    supports_chat = false

    resources {
      cpu_limits    = "500m"
      memory_limits = "256Mi"
    }
  }
}
`, agentType, displayName, status)
}
