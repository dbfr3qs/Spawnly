// internal/tokenvalidator/validator_test.go
package tokenvalidator

import (
	"reflect"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwt"
)

func mkToken(t *testing.T, claims map[string]any) jwt.Token {
	t.Helper()
	b := jwt.NewBuilder()
	for k, v := range claims {
		b = b.Claim(k, v)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	return tok
}

func TestClaims_NestedActChain(t *testing.T) {
	tok := mkToken(t, map[string]any{
		"sub": "user:user-1",
		"aud": "sample-api-a",
		"act": map[string]any{
			"sub": "spiffe://cluster.local/agent/agent-abc",
			"act": map[string]any{
				"sub": "spiffe://cluster.local/agent/agent-parent",
			},
		},
		"scope": "sample-api-a:read sample-api-a:write",
	})

	c := claimsFromToken(tok)

	if c.User != "user:user-1" {
		t.Errorf("User = %q", c.User)
	}
	wantChain := []string{
		"spiffe://cluster.local/agent/agent-abc",
		"spiffe://cluster.local/agent/agent-parent",
	}
	if !reflect.DeepEqual(c.Chain, wantChain) {
		t.Errorf("Chain = %v, want %v", c.Chain, wantChain)
	}
	if c.ActingAgent != wantChain[0] {
		t.Errorf("ActingAgent = %q", c.ActingAgent)
	}
	if c.ActingAgentName != "agent-abc" {
		t.Errorf("ActingAgentName = %q, want agent-abc", c.ActingAgentName)
	}
	if !c.HasScope("sample-api-a:read") || !c.HasScope("sample-api-a:write") {
		t.Errorf("Scopes = %v", c.Scopes)
	}
	if !c.HasAudience("sample-api-a") {
		t.Errorf("Audience = %v", c.Audience)
	}
}

func TestClaims_NoAct_FallsBackToSub(t *testing.T) {
	tok := mkToken(t, map[string]any{
		"sub": "spiffe://cluster.local/agent/legacy-agent",
		"aud": "sample-api-a",
	})
	c := claimsFromToken(tok)
	if c.ActingAgent != "spiffe://cluster.local/agent/legacy-agent" {
		t.Errorf("ActingAgent = %q", c.ActingAgent)
	}
	if c.ActingAgentName != "legacy-agent" {
		t.Errorf("ActingAgentName = %q", c.ActingAgentName)
	}
	if len(c.Chain) != 0 {
		t.Errorf("expected empty Chain, got %v", c.Chain)
	}
}

func TestClaims_ScopeAsArray(t *testing.T) {
	tok := mkToken(t, map[string]any{
		"sub":   "user:u",
		"scope": []any{"a:read", "a:write"},
	})
	c := claimsFromToken(tok)
	want := []string{"a:read", "a:write"}
	if !reflect.DeepEqual(c.Scopes, want) {
		t.Errorf("Scopes = %v, want %v", c.Scopes, want)
	}
}

func TestClaims_AudienceAsArray(t *testing.T) {
	tok := mkToken(t, map[string]any{
		"sub": "user:u",
		"aud": []string{"sample-api-a", "sample-api-b"},
	})
	c := claimsFromToken(tok)
	if !c.HasAudience("sample-api-a") || !c.HasAudience("sample-api-b") {
		t.Errorf("Audience = %v", c.Audience)
	}
	if c.HasAudience("delegation") {
		t.Errorf("unexpected delegation audience")
	}
}

func TestClaims_TokenUseDelegation(t *testing.T) {
	tok := mkToken(t, map[string]any{
		"sub":       "user:u",
		"aud":       "delegation",
		"token_use": "delegation",
	})
	c := claimsFromToken(tok)
	if c.TokenUse != "delegation" {
		t.Errorf("TokenUse = %q", c.TokenUse)
	}
	if !c.HasAudience("delegation") {
		t.Errorf("expected delegation audience")
	}
}

func TestParseActChain_Malformed(t *testing.T) {
	// act present but not an object -> empty chain, no panic.
	if got := parseActChain("not-an-object"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	// nested act missing sub is skipped gracefully.
	chain := parseActChain(map[string]any{
		"sub": "a",
		"act": map[string]any{"act": map[string]any{"sub": "c"}},
	})
	want := []string{"a", "c"}
	if !reflect.DeepEqual(chain, want) {
		t.Errorf("chain = %v, want %v", chain, want)
	}
}
