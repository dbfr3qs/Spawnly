// internal/registry/consent.go
package registry

import (
	"fmt"
	"time"
)

// ConsentRecord captures a user's CIBA-granted approval of one spawn edge:
// user U allows parent agent-type P to spawn child agent-type C with the
// granted scopes. One record exists per (user, parentType, childType) — a
// fresh grant replaces the previous one, and a different parent wanting the
// same child type needs its own consent (confused-deputy protection).
type ConsentRecord struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	ParentType string    `json:"parentType"`
	ChildType  string    `json:"childType"`
	Scopes     []string  `json:"scopes"`
	GrantedAt  time.Time `json:"grantedAt"`
	// ExpiresAt is absent when the parent template sets no consentTTL.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Revoked   bool       `json:"revoked"`
}

// ConsentDecision answers "may this spawn proceed without asking the user?".
// When Granted is false, Reason says why the stored consent (if any) doesn't
// apply — which is exactly the context a fresh CIBA prompt should show.
type ConsentDecision struct {
	Granted bool   `json:"granted"`
	Reason  string `json:"reason"`
}

// EvaluateConsent checks a stored consent record against the scopes a new
// spawn requests. The re-consent triggers are revocation, TTL expiry, and
// scope escalation: every requested scope must be inside the granted set.
func EvaluateConsent(rec ConsentRecord, requestedScopes []string, now time.Time) ConsentDecision {
	if rec.Revoked {
		return ConsentDecision{Reason: "consent revoked"}
	}
	if rec.ExpiresAt != nil && now.After(*rec.ExpiresAt) {
		return ConsentDecision{Reason: "consent expired"}
	}
	granted := make(map[string]bool, len(rec.Scopes))
	for _, sc := range rec.Scopes {
		granted[sc] = true
	}
	for _, sc := range requestedScopes {
		if !granted[sc] {
			return ConsentDecision{Reason: fmt.Sprintf("scope %q not previously granted", sc)}
		}
	}
	return ConsentDecision{Granted: true, Reason: "matched"}
}
