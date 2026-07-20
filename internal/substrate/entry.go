// Package substrate holds the neutral control-plane machinery for the
// remote-substrate feature (bring-your-own-compute): the CP-mediated SPIRE
// registration-entry authorization (this file), and the org-bound x509 identity
// issuance (issuer.go). It is platform code the consumer-side substrate-agent
// (Phase 4) depends on over the wire — never the reverse (dependency-direction
// rule): nothing here imports kind- or substrate-implementation specifics.
//
// # The CP-mediated Entry API — the isolation spine's third point
//
// A substrate never authors SPIRE registration entries directly. It asks the
// control plane, authenticated by its org-scoped SVID, and the control plane
// creates/deletes entries ONLY under the CALLER'S OWN org namespace. This file is
// the authorization core: EntryService validates every field of a requested entry
// against the caller's org (server-authoritative — the org comes from the
// verified SVID, never the request body) and fails closed on any mismatch, then
// hands a canonical spec to a pluggable EntryBroker (the mechanism — SPIRE gRPC
// admin API, CLI, etc. — can change without touching this authz).
package substrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spawnly/platform/internal/spiffe"
	"github.com/spawnly/platform/internal/substrate/devca"
)

// EntrySpec is a neutral, mechanism-independent description of ONE SPIRE
// registration entry. It is what a substrate asks the CP to create and what an
// EntryBroker materializes.
type EntrySpec struct {
	// SPIFFEID is the workload identity to register, e.g.
	// spiffe://<td>/org/{org}/agent/{tenant}/{user}/{type}/{id}.
	SPIFFEID string
	// ParentID is the SPIRE node the entry parents to — for a substrate workload
	// this MUST be the caller org's x509pop node
	// (spiffe://<td>/spire/agent/x509pop/org/{org}/{fingerprint}).
	ParentID string
	// Selectors are SPIRE workload selectors (e.g. "k8s:ns:foo",
	// "k8s:pod-name:bar", "k8s:sa:baz"). A ServiceAccount selector is REQUIRED so
	// an arbitrary pod in the namespace cannot be issued this identity.
	Selectors []string
}

// EntryBroker is the narrow seam behind which the entry-creation MECHANISM lives
// (SPIRE server entry gRPC admin API, the spire-server CLI as a delegated admin,
// etc.). EntryService does all authorization BEFORE calling it, so a broker only
// ever receives already-authorized, org-correct specs. Implementations must be
// idempotent on create (an already-present identical entry is success).
type EntryBroker interface {
	CreateEntry(ctx context.Context, spec EntrySpec) error
	// DeleteEntry removes the entry for spiffeID. Deleting an absent entry is a
	// no-op success (idempotent teardown).
	DeleteEntry(ctx context.Context, spiffeID string) error
}

// ParentPrefixDeleter is an OPTIONAL EntryBroker capability: delete EVERY entry
// whose parent id begins with parentPrefix. It backs the rotate hygiene
// (InvalidateOrgNodeEntries) — sweeping an org's stale node entries so a
// superseded x509pop node (old cert fingerprint) cannot keep re-issuing SVIDs to
// workload entries parented under it if the old leaf is replayed. A broker that
// cannot enumerate entries omits this; InvalidateOrgNodeEntries then reports the
// gap rather than silently skipping.
type ParentPrefixDeleter interface {
	DeleteEntriesUnderParent(ctx context.Context, parentPrefix string) error
}

// RecordOrgResolver resolves the server-authoritative owning org of an agent
// record, so the Entry API can require that a requested workload identity names a
// record that actually belongs to the caller's org. found is false for an unknown
// agentId; err is a backend outage (fail closed).
type RecordOrgResolver interface {
	RecordOrg(ctx context.Context, agentID string) (orgID string, found bool, err error)
}

// Required SPIRE k8s workload-selector prefixes for a substrate workload entry.
// ServiceAccount (k8s:sa:) is mandatory ON TOP OF namespace + pod-name so a
// workload cannot be impersonated by an arbitrary pod that lands in the namespace
// (remote-substrate Phase 3 carry-forward: entry selectors must bind the SA).
const (
	selNamespace      = "k8s:ns:"
	selPodName        = "k8s:pod-name:"
	selServiceAccount = "k8s:sa:"
)

// EntryService authorizes and brokers CP-mediated SPIRE entry operations. It is
// constructed once in the orchestrator with the deployment trust domain, a
// resolver over the registry, and a concrete broker.
type EntryService struct {
	Broker      EntryBroker
	Resolver    RecordOrgResolver
	TrustDomain string
}

// ErrEntryUnauthorized is the single, oracle-free error every authorization
// failure collapses to — the handler renders it as one status so a caller cannot
// distinguish "wrong org" from "unknown agent" from "malformed id".
var ErrEntryUnauthorized = errors.New("substrate entry request not authorized for caller org")

// CreateWorkloadEntry authorizes a substrate's request to register ONE workload
// identity and, iff every check passes, brokers its creation. callerOrg is the
// org segment of the caller's VERIFIED substrate-agent SVID (server-authoritative,
// never from the body). Every field of spec is validated against callerOrg:
//
//   - the SPIFFE ID is a well-formed org-scoped WORKLOAD id in THIS trust domain
//     whose org segment == callerOrg (not the substrate-agent identity, not
//     platform, not another org);
//   - the parent is callerOrg's own x509pop node;
//   - the selectors bind namespace + pod-name + service account;
//   - the agentId names a registry record that belongs to callerOrg.
//
// Any failure returns ErrEntryUnauthorized (oracle-free); a resolver/broker
// outage returns a wrapped error the handler renders as a 5xx.
func (s *EntryService) CreateWorkloadEntry(ctx context.Context, callerOrg string, spec EntrySpec) error {
	if _, _, err := s.authorizeWorkload(ctx, callerOrg, spec.SPIFFEID, spec.ParentID, spec.Selectors); err != nil {
		return err
	}
	if err := s.Broker.CreateEntry(ctx, spec); err != nil {
		return fmt.Errorf("broker create entry: %w", err)
	}
	return nil
}

// DeleteWorkloadEntry authorizes a substrate's request to remove a workload
// identity it owns and brokers the deletion. The SPIFFE ID's org must equal
// callerOrg and the named record must belong to callerOrg — identical isolation
// to create, so a substrate can only ever tear down its OWN org's entries. A
// caller may delete an entry whose record has already been removed (found=false)
// as long as the SPIFFE ID's org segment is callerOrg — teardown must still work
// after the record is gone.
func (s *EntryService) DeleteWorkloadEntry(ctx context.Context, callerOrg, spiffeID string) error {
	if s.TrustDomain == "" {
		// A misconfigured service (no trust domain) must never authorize anything —
		// mirror authorizeWorkload's fail-closed guard so delete has identical posture.
		return ErrEntryUnauthorized
	}
	normCaller, err := devca.NormalizeOrgID(callerOrg)
	if err != nil {
		return ErrEntryUnauthorized
	}
	parsed, err := spiffe.ParsePath(spiffeID)
	if err != nil || parsed.TrustDomain != s.TrustDomain || parsed.OrgID == "" || parsed.SubstrateAgent {
		return ErrEntryUnauthorized
	}
	normID, err := devca.NormalizeOrgID(parsed.OrgID)
	if err != nil || normID != normCaller {
		return ErrEntryUnauthorized
	}
	// Best-effort ownership check: if the record still exists it MUST belong to
	// the caller org; if it is already gone, allow teardown (the SPIFFE-ID org
	// segment already proved caller-org scope above).
	recOrg, found, rerr := s.Resolver.RecordOrg(ctx, parsed.AgentID)
	if rerr != nil {
		return fmt.Errorf("resolve record org: %w", rerr)
	}
	if found {
		normRec, nerr := devca.NormalizeOrgID(recOrg)
		if nerr != nil || normRec != normCaller {
			return ErrEntryUnauthorized
		}
	}
	if err := s.Broker.DeleteEntry(ctx, spiffeID); err != nil {
		return fmt.Errorf("broker delete entry: %w", err)
	}
	return nil
}

// InvalidateOrgNodeEntries deletes ALL registration entries parented under the
// org's x509pop node prefix (spiffe://<td>/spire/agent/x509pop/org/{org}/). It is
// the substrate ROTATE / disconnect hygiene: a rotate mints a NEW node
// (fingerprint) and the connect flow re-provisions the bootstrap entry under it,
// but the OLD node's entries (the superseded bootstrap entry + any workload
// entries the substrate authored) would otherwise linger and keep issuing SVIDs
// if the old (replaced) leaf were replayed. Sweeping the whole org-node prefix
// removes both in one shot; the substrate then re-authors its workload entries
// under the new node on reconcile-on-connect. Org is server-authoritative
// (normalized here); a broker without enumeration support returns an error the
// caller treats as best-effort.
func (s *EntryService) InvalidateOrgNodeEntries(ctx context.Context, orgID string) error {
	if s.TrustDomain == "" {
		return ErrEntryUnauthorized
	}
	normOrg, err := devca.NormalizeOrgID(orgID)
	if err != nil {
		return fmt.Errorf("invalid org id: %w", err)
	}
	prefix, err := devca.NodeSpiffePathPrefix(s.TrustDomain, normOrg)
	if err != nil {
		return fmt.Errorf("node prefix: %w", err)
	}
	pd, ok := s.Broker.(ParentPrefixDeleter)
	if !ok {
		return errors.New("entry broker does not support parent-prefix deletion")
	}
	return pd.DeleteEntriesUnderParent(ctx, prefix)
}

// authorizeWorkload is the pure authorization core shared by create (and the
// shape checks of delete). It returns the normalized caller org and the workload
// agentId on success, or ErrEntryUnauthorized / a wrapped outage error.
func (s *EntryService) authorizeWorkload(ctx context.Context, callerOrg, spiffeID, parentID string, selectors []string) (string, string, error) {
	if s.TrustDomain == "" {
		// A misconfigured service (no trust domain) must never authorize anything.
		return "", "", ErrEntryUnauthorized
	}
	// (1) Caller org: an org-LESS (platform) caller normalizes to an error and is
	// refused here — a platform SVID can never author an org entry.
	normCaller, err := devca.NormalizeOrgID(callerOrg)
	if err != nil {
		return "", "", ErrEntryUnauthorized
	}

	// (2) SPIFFE ID shape + org: must be an org-scoped WORKLOAD id in this trust
	// domain whose org == caller org. The substrate-agent identity and platform
	// (org-less) ids are both refused.
	parsed, err := spiffe.ParsePath(spiffeID)
	if err != nil {
		return "", "", ErrEntryUnauthorized
	}
	if parsed.TrustDomain != s.TrustDomain || parsed.OrgID == "" || parsed.SubstrateAgent || parsed.AgentID == "" {
		return "", "", ErrEntryUnauthorized
	}
	normID, err := devca.NormalizeOrgID(parsed.OrgID)
	if err != nil || normID != normCaller {
		return "", "", ErrEntryUnauthorized
	}
	// The literal org segment must ALREADY be canonical (lowercase): creating an
	// entry with a non-canonical segment (e.g. /org/Acme/…) would authorize here
	// but produce a workload SVID that later fails the EXACT-match org binding in
	// the verifiers (rec.OrgID != id.OrgID) — a CP-authored dead entry. Fail closed
	// instead of minting a dud. (NormalizeOrgID is the single funnel; this pins the
	// funnel's output at the boundary.)
	if parsed.OrgID != normID {
		return "", "", ErrEntryUnauthorized
	}

	// (3) Parent must be caller org's x509pop node (prefix match; the trailing
	// segment is the per-cert fingerprint, which must be present and single).
	nodePrefix, err := devca.NodeSpiffePathPrefix(s.TrustDomain, normCaller)
	if err != nil {
		return "", "", ErrEntryUnauthorized
	}
	if !strings.HasPrefix(parentID, nodePrefix) {
		return "", "", ErrEntryUnauthorized
	}
	fp := strings.TrimPrefix(parentID, nodePrefix)
	if fp == "" || strings.Contains(fp, "/") {
		return "", "", ErrEntryUnauthorized
	}

	// (4) Selectors must bind namespace + pod-name + service account.
	if err := validateSelectors(selectors); err != nil {
		return "", "", ErrEntryUnauthorized
	}

	// (5) The agentId must name a record owned by the caller org. Unknown record
	// ⇒ refuse (a substrate may only register entries for records the CP already
	// preregistered under its org); a resolver outage ⇒ fail closed (5xx).
	recOrg, found, rerr := s.Resolver.RecordOrg(ctx, parsed.AgentID)
	if rerr != nil {
		return "", "", fmt.Errorf("resolve record org: %w", rerr)
	}
	if !found {
		return "", "", ErrEntryUnauthorized
	}
	normRec, err := devca.NormalizeOrgID(recOrg)
	if err != nil || normRec != normCaller {
		return "", "", ErrEntryUnauthorized
	}

	return normCaller, parsed.AgentID, nil
}

// validateSelectors requires at least one namespace, one pod-name, and one
// service-account selector — the SA selector being the carry-forward hardening so
// an arbitrary pod in the namespace cannot claim the identity. Extra selectors
// are permitted (they only ever NARROW attestation). Used for WORKLOAD entries,
// whose pod name IS known (the substrate agent creates the pod and requests the
// entry for it).
func validateSelectors(selectors []string) error {
	hasNS, hasPod, hasSA := hasSelectorKinds(selectors)
	if !hasNS || !hasPod || !hasSA {
		return errors.New("selectors must bind namespace, pod-name, and service account")
	}
	return nil
}

// validateBootstrapSelectors is validateSelectors minus pod-name: the
// substrate-agent is a Deployment whose pod name is not predictable at connect,
// so its bootstrap entry binds namespace + service account (the SA being the
// strong binding). Requiring pod-name here would make the bootstrap entry
// impossible to author before the pod exists.
func validateBootstrapSelectors(selectors []string) error {
	hasNS, _, hasSA := hasSelectorKinds(selectors)
	if !hasNS || !hasSA {
		return errors.New("selectors must bind namespace and service account")
	}
	return nil
}

func hasSelectorKinds(selectors []string) (hasNS, hasPod, hasSA bool) {
	for _, sel := range selectors {
		switch {
		case strings.HasPrefix(sel, selNamespace) && len(sel) > len(selNamespace):
			hasNS = true
		case strings.HasPrefix(sel, selPodName) && len(sel) > len(selPodName):
			hasPod = true
		case strings.HasPrefix(sel, selServiceAccount) && len(sel) > len(selServiceAccount):
			hasSA = true
		}
	}
	return
}

// ProvisionSubstrateAgentEntry CP-provisions the org's OWN substrate-agent
// bootstrap entry (spiffe://<td>/org/{orgId}/substrate-agent, parented to the
// org's x509pop node). This closes the identity chicken-and-egg: it is the SVID
// the substrate-agent then uses to authenticate the dispatch channel AND the
// Entry API, so it MUST exist before any workload entry and the consumer never
// creates it. Unlike CreateWorkloadEntry this is driven by the ADMIN-authenticated
// connect flow (org from the path, already authorized by requireOrgAdmin), so it
// takes no caller SVID and does no record lookup (the substrate-agent has no agent
// record). nodeFingerprint is the org node's x509pop fingerprint (or empty to
// parent to the org node prefix once the fingerprint is known at connect —
// callers pass the concrete node id when they have it).
func (s *EntryService) ProvisionSubstrateAgentEntry(ctx context.Context, orgID, nodeID string, selectors []string) error {
	normOrg, err := devca.NormalizeOrgID(orgID)
	if err != nil {
		return fmt.Errorf("invalid org id: %w", err)
	}
	spiffeID, err := spiffe.BuildSubstrateAgentPath(s.TrustDomain, normOrg)
	if err != nil {
		return fmt.Errorf("build substrate-agent id: %w", err)
	}
	// The node id must be the caller org's x509pop node.
	nodePrefix, err := devca.NodeSpiffePathPrefix(s.TrustDomain, normOrg)
	if err != nil {
		return fmt.Errorf("node prefix: %w", err)
	}
	if !strings.HasPrefix(nodeID, nodePrefix) {
		return fmt.Errorf("substrate-agent parent %q is not org %s's x509pop node", nodeID, normOrg)
	}
	// The substrate-agent runs as a Deployment in the customer cluster, so its
	// pod name carries a random suffix and is NOT predictable at connect. Its
	// entry therefore binds namespace + service account (the stable, CP-known
	// identity of the bundle's substrate-agent SA) — NOT pod-name. The SA selector
	// is the strong binding either way (an arbitrary pod under a different SA can't
	// claim it).
	if err := validateBootstrapSelectors(selectors); err != nil {
		return fmt.Errorf("substrate-agent %w", err)
	}
	return s.Broker.CreateEntry(ctx, EntrySpec{SPIFFEID: spiffeID, ParentID: nodeID, Selectors: selectors})
}
