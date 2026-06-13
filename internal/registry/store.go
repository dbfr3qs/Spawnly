// internal/registry/store.go
package registry

import (
	"context"
	"time"

	"github.com/spawnly/platform/internal/events"
)

// Store is the registry's persistence boundary: the HTTP layer depends only on
// this interface, never on a concrete storage type, so a durable backend can be
// slotted in without touching the handlers. The in-memory implementation (the
// default + test double) lives in package main; a DynamoDB implementation for
// AWS production is specified in docs/saas/phase-4-persistence-store-interface.md.
//
// Every method takes a context.Context and returns an error even though the
// in-memory impl ignores the context and never fails — durable backends need
// both for cancellation/tracing and to surface network/throttling failures, and
// retrofitting them later would mean touching every call site twice.
//
// Graph traversal (the subtree BFS, the depth/chain up-walks) is NOT part of the
// Store; it stays in app code built on the GetAgent + ListChildren primitives,
// which keeps each traversal step a single backend call.
type Store interface {
	// Templates — point access keyed by agentType.
	PutTemplate(ctx context.Context, t AgentTemplate) error
	GetTemplate(ctx context.Context, agentType string) (AgentTemplate, bool, error)
	ListTemplateTypes(ctx context.Context) ([]string, error)

	// Schema — the active SpiceDB schema the registry applied on boot, used to
	// validate templates and serve GET /v1/schema. (Config, co-located here so
	// the HTTP layer needs only the Store handle.)
	ValidateTemplate(t AgentTemplate) error
	Schema() (text, version, source string)

	// Agent records — point access keyed by agentID. GetAgent returns the zero
	// value (AgentID == "") with no error when the id is unknown.
	RegisterAgent(ctx context.Context, r AgentRecord) error
	GetAgent(ctx context.Context, id string) (AgentRecord, error)
	ListAgents(ctx context.Context) ([]AgentRecord, error) // excludes Dismissed
	DismissAgent(ctx context.Context, id string) (bool, error)
	UpdateAgentStatus(ctx context.Context, id, status string) (bool, error)

	// Graph primitive — direct children of parentID (traversal stays in app code).
	ListChildren(ctx context.Context, parentID string) ([]AgentRecord, error)

	// Events — append-only list keyed by agentID.
	AppendEvent(ctx context.Context, agentID string, e events.Event) (events.Event, error)
	GetEvents(ctx context.Context, agentID string) ([]events.Event, error)

	// Consents — keyed by (userID, parentType, childType); list optionally scoped.
	UpsertConsent(ctx context.Context, rec ConsentRecord) (ConsentRecord, error)
	FindConsent(ctx context.Context, userID, parentType, childType string) (ConsentRecord, bool, error)
	ListConsents(ctx context.Context, userID string) ([]ConsentRecord, error)
	RevokeConsent(ctx context.Context, id, userID string) (bool, error)
	// ConsentExpiry derives a fresh grant's expiry from the parent template's
	// per-child consentTTL (policy that depends on template storage, hence here).
	ConsentExpiry(ctx context.Context, parentType, childType string, from time.Time) (*time.Time, error)

	// Consent requests (Phase 5b broker) — the pending->approved/denied lifecycle.
	CreateConsentRequest(ctx context.Context, req ConsentRequest) (ConsentRequest, bool, error)
	CreateApprovedConsentRequest(ctx context.Context, req ConsentRequest) (ConsentRequest, error)
	GetConsentRequest(ctx context.Context, id string) (ConsentRequest, bool, error)
	ListConsentRequests(ctx context.Context, userID, status string) ([]ConsentRequest, error)
	// ResolveConsentRequest approves/denies a pending request; on approve it
	// upserts the ConsentRecord and sweeps other covered pending requests. A
	// non-empty userID scopes the resolve to that user's own request.
	ResolveConsentRequest(ctx context.Context, id, userID string, approve bool, scopes []string) (ConsentRequest, bool, error)
}
