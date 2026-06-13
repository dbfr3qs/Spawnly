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

// ConsentRequestStatus is the lifecycle state of a human-in-the-loop consent
// ask brokered by the registry (Phase 5b).
type ConsentRequestStatus string

const (
	ConsentPending  ConsentRequestStatus = "pending"
	ConsentApproved ConsentRequestStatus = "approved"
	ConsentDenied   ConsentRequestStatus = "denied"
)

// ConsentRequest is one human-in-the-loop ask: "user U, may parent type P spawn
// child type C with these scopes?". The registry owns its full lifecycle, so
// consent no longer requires a CIBA-capable IdP — CIBA becomes one optional
// driver that creates and resolves these via the registry's API. Keyed by the
// same (user, parentType, childType) edge as ConsentRecord; at most one open
// (pending) request exists per edge.
type ConsentRequest struct {
	ID             string               `json:"id"`
	UserID         string               `json:"userId"`
	ParentType     string               `json:"parentType"`
	ChildType      string               `json:"childType"`
	Scopes         []string             `json:"scopes"`
	BindingMessage string               `json:"bindingMessage,omitempty"`
	Status         ConsentRequestStatus `json:"status"`
	CreatedAt      time.Time            `json:"createdAt"`
	ResolvedAt     *time.Time           `json:"resolvedAt,omitempty"`
	// ExternalRef carries an optional driver-specific id (e.g. a Duende CIBA
	// BackchannelUserLoginRequest.InternalId) so a driver can correlate the
	// registry's decision back to its own pending object. Opaque to the registry.
	ExternalRef string `json:"externalRef,omitempty"`
}

// FirstUncoveredScope returns the first requested scope missing from the
// granted set, or "" when every requested scope is covered. It is the single
// definition of "scope subset" for consent decisions — the registry's
// EvaluateConsent and the sidecar's local escalation check both use it.
func FirstUncoveredScope(granted, requested []string) string {
	set := make(map[string]bool, len(granted))
	for _, sc := range granted {
		set[sc] = true
	}
	for _, sc := range requested {
		if !set[sc] {
			return sc
		}
	}
	return ""
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
	if sc := FirstUncoveredScope(rec.Scopes, requestedScopes); sc != "" {
		return ConsentDecision{Reason: fmt.Sprintf("scope %q not previously granted", sc)}
	}
	return ConsentDecision{Granted: true, Reason: "matched"}
}
