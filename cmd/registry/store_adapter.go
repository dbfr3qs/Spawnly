// cmd/registry/store_adapter.go
//
// Adapter making the in-memory *store satisfy the registry.Store interface
// (Phase 4). The interface is the persistence seam the HTTP layer is built on,
// so a DynamoDB implementation (see docs/saas/phase-4-persistence-store-interface.md)
// can replace the in-memory store without changing the registry's logic. These
// exported methods add the ctx/error contract a durable backend needs and
// delegate to the existing in-memory primitives; the in-memory impl ignores ctx
// and never errors.
package main

import (
	"context"
	"time"

	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
)

// Compile-time proof the in-memory store satisfies the Dynamo-ready contract.
var _ registry.Store = (*store)(nil)

func (s *store) PutTemplate(_ context.Context, t registry.AgentTemplate) error {
	s.putTemplate(t)
	return nil
}

func (s *store) GetTemplate(_ context.Context, agentType string) (registry.AgentTemplate, bool, error) {
	t, ok := s.getTemplate(agentType)
	return t, ok, nil
}

func (s *store) ListTemplateTypes(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.templates))
	for k := range s.templates {
		out = append(out, k)
	}
	return out, nil
}

func (s *store) ValidateTemplate(t registry.AgentTemplate) error {
	return s.validateTemplate(t)
}

func (s *store) Schema() (text, version, source string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schemaText, s.schemaVersion, s.schemaSource
}

func (s *store) RegisterAgent(_ context.Context, r registry.AgentRecord) error {
	s.registerAgent(r)
	return nil
}

func (s *store) GetAgent(_ context.Context, id string) (registry.AgentRecord, error) {
	return s.getAgent(id), nil
}

func (s *store) ListAgents(_ context.Context) ([]registry.AgentRecord, error) {
	return s.listAgents(), nil
}

func (s *store) DismissAgent(_ context.Context, id string) (bool, error) {
	return s.dismissAgent(id), nil
}

func (s *store) UpdateAgentStatus(_ context.Context, id, status string) (bool, error) {
	return s.updateAgent(id, status), nil
}

// ListChildren returns the agent records whose ParentID is parentID — the
// single primitive the subtree BFS is built on, replacing the old "scan all
// agents, build an adjacency map" step buried inside subtree.
func (s *store) ListChildren(_ context.Context, parentID string) ([]registry.AgentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []registry.AgentRecord{}
	for _, rec := range s.agents {
		if rec.ParentID == parentID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *store) AppendEvent(_ context.Context, agentID string, e events.Event) (events.Event, error) {
	s.appendEvent(agentID, e)
	evts := s.getEvents(agentID)
	return evts[len(evts)-1], nil
}

func (s *store) GetEvents(_ context.Context, agentID string) ([]events.Event, error) {
	return s.getEvents(agentID), nil
}

func (s *store) UpsertConsent(_ context.Context, rec registry.ConsentRecord) (registry.ConsentRecord, error) {
	return s.upsertConsent(rec), nil
}

func (s *store) FindConsent(_ context.Context, userID, parentType, childType string) (registry.ConsentRecord, bool, error) {
	rec, ok := s.findConsent(userID, parentType, childType)
	return rec, ok, nil
}

func (s *store) ListConsents(_ context.Context, userID string) ([]registry.ConsentRecord, error) {
	return s.listConsents(userID), nil
}

func (s *store) RevokeConsent(_ context.Context, id, userID string) (bool, error) {
	return s.revokeConsent(id, userID), nil
}

func (s *store) ConsentExpiry(_ context.Context, parentType, childType string, from time.Time) (*time.Time, error) {
	return s.consentExpiry(parentType, childType, from), nil
}

func (s *store) CreateConsentRequest(_ context.Context, req registry.ConsentRequest) (registry.ConsentRequest, bool, error) {
	cr, created := s.createConsentRequest(req)
	return cr, created, nil
}

func (s *store) CreateApprovedConsentRequest(_ context.Context, req registry.ConsentRequest) (registry.ConsentRequest, error) {
	return s.createApprovedConsentRequest(req), nil
}

func (s *store) GetConsentRequest(_ context.Context, id string) (registry.ConsentRequest, bool, error) {
	cr, ok := s.getConsentRequest(id)
	return cr, ok, nil
}

func (s *store) ListConsentRequests(_ context.Context, userID, status string) ([]registry.ConsentRequest, error) {
	return s.listConsentRequests(userID, status), nil
}

func (s *store) ResolveConsentRequest(_ context.Context, id, userID string, approve bool, scopes []string) (registry.ConsentRequest, bool, error) {
	cr, ok := s.resolveConsentRequest(id, userID, approve, scopes)
	return cr, ok, nil
}
