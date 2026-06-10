// internal/registry/consent_test.go
package registry

import (
	"strings"
	"testing"
	"time"
)

func consentFixture() ConsentRecord {
	return ConsentRecord{
		ID:         "consent-1",
		UserID:     "alice",
		ParentType: "parent-agent",
		ChildType:  "currency-converter",
		Scopes:     []string{"openid", "sample-api-b:read"},
		GrantedAt:  time.Now(),
	}
}

func TestEvaluateConsentMatched(t *testing.T) {
	d := EvaluateConsent(consentFixture(), []string{"openid", "sample-api-b:read"}, time.Now())
	if !d.Granted || d.Reason != "matched" {
		t.Fatalf("want granted/matched, got %+v", d)
	}
}

func TestEvaluateConsentSubsetIsGranted(t *testing.T) {
	d := EvaluateConsent(consentFixture(), []string{"sample-api-b:read"}, time.Now())
	if !d.Granted {
		t.Fatalf("requesting a subset of granted scopes should pass, got %+v", d)
	}
}

func TestEvaluateConsentEmptyRequestIsGranted(t *testing.T) {
	if d := EvaluateConsent(consentFixture(), nil, time.Now()); !d.Granted {
		t.Fatalf("empty request is trivially a subset, got %+v", d)
	}
}

func TestEvaluateConsentScopeEscalation(t *testing.T) {
	d := EvaluateConsent(consentFixture(), []string{"sample-api-b:read", "sample-api-a:write"}, time.Now())
	if d.Granted {
		t.Fatal("scope outside the granted set must force re-consent")
	}
	if !strings.Contains(d.Reason, "sample-api-a:write") {
		t.Fatalf("reason should name the escalating scope, got %q", d.Reason)
	}
}

func TestEvaluateConsentRevoked(t *testing.T) {
	rec := consentFixture()
	rec.Revoked = true
	d := EvaluateConsent(rec, []string{"openid"}, time.Now())
	if d.Granted || d.Reason != "consent revoked" {
		t.Fatalf("want revoked denial, got %+v", d)
	}
}

func TestEvaluateConsentExpired(t *testing.T) {
	rec := consentFixture()
	past := time.Now().Add(-time.Minute)
	rec.ExpiresAt = &past
	d := EvaluateConsent(rec, []string{"openid"}, time.Now())
	if d.Granted || d.Reason != "consent expired" {
		t.Fatalf("want expired denial, got %+v", d)
	}
}

func TestEvaluateConsentNoExpiryNeverExpires(t *testing.T) {
	rec := consentFixture()
	rec.GrantedAt = time.Now().Add(-24 * 365 * time.Hour)
	if d := EvaluateConsent(rec, []string{"openid"}, time.Now()); !d.Granted {
		t.Fatalf("nil ExpiresAt must never expire, got %+v", d)
	}
}
