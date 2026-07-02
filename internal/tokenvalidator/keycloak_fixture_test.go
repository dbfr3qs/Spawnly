// internal/tokenvalidator/keycloak_fixture_test.go
package tokenvalidator

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwt"
)

// These are REAL access-token payloads minted by Keycloak on kind via the
// Phase 2 attestation SPI + identity mapper (captured with
// scripts/keycloak-probe.sh's chain-worker agent). They prove the Go
// tokenvalidator extracts the SAME Claims from a Keycloak-shaped token as from
// the IdentityServer-shaped ones the other tests use — i.e. Phase 2's mappers
// reproduce today's claim shapes closely enough that claimsFromToken needs no
// Keycloak-specific branch:
//
//   - sub = "user:<userId>"           (mapper sub-override)
//   - act = {"sub":"<spiffe URI>"}    (nested JSON object → Chain[0])
//   - scope = space-delimited string  (Keycloak's default `scope` shape)
//   - aud = single string             (custom audience mapper)
//   - token_use = "delegation"        (only on the audience=delegation mint)
//   - azp present, no `role`          (machine token carries no user role)
//
// The claims are extracted from the raw payload (the realm signing keys rotate,
// so the fixture carries no live signature). The signed end-to-end path is
// proven live by scripts/keycloak-probe.sh, which mints these exact tokens and
// has sample-api-a accept the normal one + reject the delegation one.

// A normal client_credentials access token (audience NOT delegation).
const keycloakAccessTokenClaims = `{
  "exp": 1782967918, "iat": 1782967798,
  "jti": "trrtcc:cec23a80-e955-ea07-1d0b-3b95f77573c2",
  "iss": "http://localhost:8090/realms/spawnly",
  "aud": "sample-api-a",
  "sub": "user:alice",
  "typ": "Bearer", "azp": "chain-worker", "acr": "1",
  "scope": "email sample-api-a:read profile",
  "clientHost": "127.0.0.1",
  "act": {"sub": "spiffe://cluster.local/agent/tenant-1/alice/chain-worker/fix-1782967781"},
  "email_verified": false,
  "preferred_username": "service-account-chain-worker",
  "clientAddress": "127.0.0.1",
  "client_id": "chain-worker"
}`

// The audience=delegation variant — same shape plus token_use=delegation.
const keycloakDelegationTokenClaims = `{
  "exp": 1782967918, "iat": 1782967798,
  "iss": "http://localhost:8090/realms/spawnly",
  "aud": "sample-api-a",
  "sub": "user:alice",
  "typ": "Bearer", "azp": "chain-worker",
  "scope": "email sample-api-a:read profile",
  "act": {"sub": "spiffe://cluster.local/agent/tenant-1/alice/chain-worker/fix-1782967781"},
  "token_use": "delegation",
  "client_id": "chain-worker"
}`

// tokenFromJSON builds a jwt.Token from a real Keycloak access-token PAYLOAD
// (a raw JSON claims object), so claimsFromToken can be exercised on the exact
// claim shapes Keycloak emits, via the same builder path mkToken uses.
func tokenFromJSON(t *testing.T, payload string) jwt.Token {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return mkToken(t, m)
}

func TestClaims_RealKeycloakAccessToken(t *testing.T) {
	c := claimsFromToken(tokenFromJSON(t, keycloakAccessTokenClaims))

	if c.User != "user:alice" {
		t.Errorf("User = %q, want user:alice", c.User)
	}
	wantChain := []string{"spiffe://cluster.local/agent/tenant-1/alice/chain-worker/fix-1782967781"}
	if !reflect.DeepEqual(c.Chain, wantChain) {
		t.Errorf("Chain = %v, want %v", c.Chain, wantChain)
	}
	if c.ActingAgent != wantChain[0] {
		t.Errorf("ActingAgent = %q", c.ActingAgent)
	}
	if c.ActingAgentName != "fix-1782967781" {
		t.Errorf("ActingAgentName = %q, want fix-1782967781", c.ActingAgentName)
	}
	if !c.HasScope("sample-api-a:read") {
		t.Errorf("Scopes = %v, want sample-api-a:read", c.Scopes)
	}
	if !c.HasAudience("sample-api-a") {
		t.Errorf("Audience = %v, want sample-api-a", c.Audience)
	}
	if c.TokenUse != "" {
		t.Errorf("TokenUse = %q, want empty (normal token)", c.TokenUse)
	}
	// A machine token must carry NO role (the invariant: role is user-sourced).
	if len(c.Roles) != 0 {
		t.Errorf("Roles = %v, want none on a machine token", c.Roles)
	}
}

func TestClaims_RealKeycloakDelegationToken(t *testing.T) {
	c := claimsFromToken(tokenFromJSON(t, keycloakDelegationTokenClaims))

	if c.TokenUse != "delegation" {
		t.Errorf("TokenUse = %q, want delegation", c.TokenUse)
	}
	// The act chain + user are still extracted (a downstream exchange reads them).
	if c.User != "user:alice" {
		t.Errorf("User = %q, want user:alice", c.User)
	}
	if c.ActingAgentName != "fix-1782967781" {
		t.Errorf("ActingAgentName = %q", c.ActingAgentName)
	}
	// Resource servers reject on token_use=delegation regardless of aud (proven
	// live in the probe: sample-api-a returns 401 for this token).
}
