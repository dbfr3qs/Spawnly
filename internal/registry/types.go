// internal/registry/types.go
package registry

import "time"

// Template status values. An empty Status is treated as active/spawnable.
// "deprecated" is tolerated for backward compatibility but never enforced.
const (
	TemplateStatusActive   = "active"
	TemplateStatusDisabled = "disabled"
)

// Attestation anchor status values. An anchor registers Pending and only
// becomes Active after the org proves control of the AWS account (Phase 8);
// until then no credential resolves against it.
const (
	AnchorPending = "pending"
	AnchorActive  = "active"
)

// Removal disposition values — what happens to a removed member's running
// agents (Phase 6). The default is RemovalOrphanToAdmin, the non-destructive
// choice: agents keep running and management passes to the org's admins.
const (
	RemovalAutoRevoke    = "auto-revoke"
	RemovalOrphanToAdmin = "orphan-to-admin"
)

// Placement values for an AgentRecord (Phase 10). Empty (the zero value) and
// PlacementK8s both mean a platform-hosted agent backed by an AgentWorkload CR
// in Spawnly's own cluster — today's only kind. PlacementRemote marks a
// CONSUMER-hosted agent that runs on the consumer's own compute via the
// SDK/sidecar and authenticates with the org's anchored STS identity; such an
// agent has NO AgentWorkload by design, so it MUST be excluded from the
// operator's stale sweep (which fails every workload-less record). Placement is
// set SERVER-SIDE (from the preregistered record), never from a sidecar's
// self-register body — a client cannot promote a k8s agent to remote to dodge
// the sweep.
const (
	PlacementK8s    = "k8s"
	PlacementRemote = "remote"
	// PlacementSubstrate marks an agent that runs on a CONSUMER-hosted Kubernetes
	// substrate (remote-substrate Phase 3): the orchestrator dispatches the spawn
	// over the substrate channel instead of creating a local AgentWorkload CR, so
	// like PlacementRemote such a record has NO AgentWorkload in Spawnly's own
	// cluster and MUST be excluded from the operator's stale sweep. Server-set at
	// preregistration and immutable (a self-registering sidecar never sets it from
	// its body), so it cannot be forged to dodge the sweep.
	PlacementSubstrate = "substrate"
)

// ValidPlacement reports whether p is a known placement ("" means unset ⇒ k8s).
//
// Callers that validate an incoming placement MUST use this rather than
// open-coding the comparison: an open-coded allow-list silently 400s any newly
// added placement, which is exactly how every substrate-placed spawn came to be
// rejected after PlacementSubstrate was introduced. Keeping the set here means a
// future value cannot drift out of a validator.
func ValidPlacement(p string) bool {
	switch p {
	case "", PlacementK8s, PlacementRemote, PlacementSubstrate:
		return true
	default:
		return false
	}
}

// Substrate registration status values (remote-substrate Phase 3). A substrate
// registers `pending` the moment the org admin requests an identity; it flips to
// `active` when its channel first connects (Phase 4). Phase 3 only persists the
// record and issues the identity — nothing flips it to active yet.
const (
	SubstratePending = "pending"
	SubstrateActive  = "active"
)

// Organization is the SaaS business account — the NEW multi-tenancy primitive,
// deliberately distinct from the existing per-consumer tenant primitive
// (tenantId/requiresTenant). The OrgID is the single identifier threaded across
// the layers this feature touches: Keycloak org alias/ID == this OrgID ==
// SpiceDB `organization:` object id.
//
// Membership (who is in the org, member vs admin) is deliberately NOT stored
// here: Keycloak is the source of truth for membership, and SpiceDB is the
// source of truth for authorization decisions. The registry only persists the
// org's own record (name, settings, optional attestation anchor).
type Organization struct {
	OrgID string `json:"orgId"`
	Name  string `json:"name,omitempty"`
	// RemovalDisposition governs a removed member's running agents; empty is
	// treated as the non-destructive RemovalOrphanToAdmin default (Phase 6).
	RemovalDisposition string `json:"removalDisposition,omitempty"`
	// Disabled marks a SUSPENDED org (signup-lifecycle Phase 4). When true every
	// member's Keycloak login is blocked (accounts disabled, tagged
	// disabledByOrg=<orgId>), all org agents are revoked, and a defense-in-depth
	// spawn guard refuses new spawns into the org. The zero value (false) is
	// enabled — existing records need no migration. Enabling clears this and
	// re-enables ONLY the members this org disabled (asymmetric agent resume).
	Disabled bool `json:"disabled,omitempty"`
	// Anchor is the org's optional attestation anchor. Absent (nil) = the org's
	// agents attest via the platform default (in-cluster SPIRE), exactly as
	// today. The lifecycle (pending->active proof-of-control) lands in Phase 8;
	// Phase 1 only persists the record shape.
	Anchor *AttestationAnchor `json:"anchor,omitempty"`
	// Substrate is the org's optional bring-your-own-compute substrate
	// registration (remote-substrate Phase 3). Absent (nil) = the org has no
	// connected substrate and its agents run platform-hosted, exactly as today.
	// EXACTLY ONE substrate per org is a structural invariant: it is a single
	// field on the org record, so a second connect overwrites/rotates the first
	// rather than creating a parallel record (spec non-goal: one substrate per
	// org). Modeled next to Anchor and persisted as part of the org record.
	Substrate *Substrate `json:"substrate,omitempty"`
	CreatedAt time.Time  `json:"createdAt,omitempty"`
}

// Substrate is an org's bring-your-own-compute registration (remote-substrate
// Phase 3). The control plane ISSUES the org-bound x509 identity (the x509pop
// leaf) the substrate's SPIRE agents present; the identity itself is returned to
// the admin ONCE at connect and NEVER persisted server-side — only its
// fingerprint + serial are kept here so a later connect/rotate can be correlated
// and the old credential logically invalidated. Admin authentication + CP
// issuance IS the org binding (no separate proof-of-control step), so unlike the
// STS AttestationAnchor there is no activate handshake.
type Substrate struct {
	// Status is SubstratePending until the substrate's channel first connects
	// (Phase 4), then SubstrateActive. Phase 3 only ever writes `pending`.
	Status string `json:"status"`
	// CertFingerprint is the SHA-256 (hex) fingerprint of the issued leaf cert —
	// the ONLY cryptographic material retained (the private key never leaves the
	// admin's download). A rotate reissues and replaces this, logically revoking
	// the prior credential.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	// CertSerial is the issued leaf's serial number (hex), for audit correlation.
	CertSerial string `json:"certSerial,omitempty"`
	// IssuedAt / ActivatedAt are timestamps for the connect and first-connection
	// events; ActivatedAt is nil until the channel connects (Phase 4/5).
	IssuedAt    time.Time  `json:"issuedAt,omitempty"`
	ActivatedAt *time.Time `json:"activatedAt,omitempty"`
}

// AttestationAnchor is an org's per-org trust anchor (first mode: STS trust
// federation with the consumer's AWS account). Status starts AnchorPending and
// flips to AnchorActive only on a successful proof-of-control presentation
// (Phase 8) — Phase 1 persists the shape but nothing verifies against it yet.
type AttestationAnchor struct {
	Type           string     `json:"type"` // e.g. "sts"
	AWSAccountID   string     `json:"awsAccountId,omitempty"`
	RoleArnPattern string     `json:"roleArnPattern,omitempty"`
	Status         string     `json:"status"` // AnchorPending | AnchorActive
	ActivatedAt    *time.Time `json:"activatedAt,omitempty"`
}

type AgentTemplate struct {
	AgentType string `json:"agentType"`
	Version   string `json:"version"`
	// Status gates instantiation. "disabled" blocks new instantiations and
	// hides the template from the spawnable list; an empty value is treated as
	// active/spawnable; "deprecated" is tolerated but not enforced.
	Status     string           `json:"status"`
	Meta       TemplateMeta     `json:"meta"`
	Runtime    RuntimeSpec      `json:"runtimeSpec"`
	AuthZ      AuthZSpec        `json:"authzTemplate"`
	Delegation DelegationPolicy `json:"delegation"`
	// RequiresTenant, when true, makes the orchestrator reject a spawn of this
	// agent-type that has no tenant id. Defaults false: tenant-ness is otherwise
	// derived from the presence of a tenant id (present => tenanted, absent =>
	// global). Set true on templates whose authz relations reference
	// {{tenant_id}}, so a tenant-less spawn fails fast instead of silently
	// coming up "global" with no tenant grant.
	RequiresTenant bool `json:"requiresTenant"`
	// OrgID scopes a UI-created template to one organization; absent = a global
	// template, visible and spawnable in every org (the seeded platform
	// catalog). This is ORTHOGONAL to RequiresTenant (the tenant primitive): a
	// template can be org-scoped and require a tenant, or either, or neither.
	// Enforcement (org-scoped visibility/authz) lands in Phase 6; Phase 1 only
	// carries the field.
	OrgID string `json:"orgId,omitempty"`
	// OAuthScopes declares the OAuth2 scopes this agent type requests at
	// runtime. When a parent spawns this type behind requireUserConsent, these
	// are the scopes of the CIBA consent request — and the set a stored
	// consent is matched against (any scope outside it forces a re-prompt).
	OAuthScopes []string `json:"oauthScopes,omitempty"`
}

// DelegationPolicy describes which child agent-types a given agent-type may
// delegate to, which scopes it may grant them, and the maximum delegation depth.
type DelegationPolicy struct {
	AllowedChildTypes []string `json:"allowedChildTypes"`
	GrantableScopes   []string `json:"grantableScopes"`
	MaxDepth          int      `json:"maxDepth"`
	// ChildPolicies holds per-child-type spawn options, keyed by child
	// agent-type. A key only has effect if that type is also listed in
	// AllowedChildTypes (ChildPolicies refines the edge; it never creates one).
	ChildPolicies map[string]ChildSpawnPolicy `json:"childPolicies,omitempty"`
}

// ChildSpawnPolicy configures how a parent's spawn of one child type is gated.
// RequireUserConsent switches on the CIBA flow: at spawn the child's sidecar
// runs a backchannel authentication request that the spawning user approves on
// the dashboard — unless a stored consent for the same (user, parentType,
// childType) edge still covers the requested scopes, in which case it
// auto-approves. ConsentTTL is a Go duration string ("720h"); empty means a
// granted consent never expires.
type ChildSpawnPolicy struct {
	RequireUserConsent bool   `json:"requireUserConsent"`
	ConsentTTL         string `json:"consentTTL,omitempty"`
}

// SpawnDecision is the registry's answer to "may this parent spawn this child
// type?". Returned by GET /v1/spawn-policy and consumed by the orchestrator at
// spawn time. The edge is gated by the parent template's AllowedChildTypes
// (reused from DelegationPolicy); a parent that lists no child types may spawn
// none (deny-by-default).
type SpawnDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	// ConsentRequired reports that the parent's template gates this child type
	// behind user consent (CIBA). The orchestrator stamps it onto the workload
	// so the child's sidecar runs the backchannel flow before serving tokens.
	ConsentRequired bool `json:"consentRequired,omitempty"`
}

type TemplateMeta struct {
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type RuntimeSpec struct {
	Image       string            `json:"image"`
	Resources   ResourceLimits    `json:"resources"`
	EnvDefaults map[string]string `json:"envDefaults"`
	Lifecycle   string            `json:"lifecycle"`
	// SupportsChat, when true, tells the dashboard this agent serves the
	// /agents/chat/:sessionId endpoint, so the chat UI should be offered. Only
	// meaningful for long-lived agents (a Service must exist to route to).
	SupportsChat bool `json:"supportsChat"`
}

type ResourceLimits struct {
	CPULimit    string `json:"cpuLimits"`
	MemoryLimit string `json:"memoryLimits"`
}

type AuthZSpec struct {
	SpiceDBRelations []SpiceDBRelationTemplate `json:"spiceDbRelations"`
}

type SpiceDBRelationTemplate struct {
	Resource string `json:"resource"` // e.g. "tenant:{{tenant_id}}"
	Relation string `json:"relation"` // e.g. "agent"
	Subject  string `json:"subject"`  // e.g. "agent:{{agent_id}}"
}

type AgentRecord struct {
	AgentID   string `json:"agentId"`
	AgentType string `json:"agentType"`
	TenantID  string `json:"tenantId"`
	UserID    string `json:"userId"`
	Status    string `json:"status"` // active | completed | failed
	Lifecycle string `json:"lifecycle"`
	// SupportsChat is copied from the template's runtimeSpec so the dashboard
	// can offer chat only for agents that actually serve the chat endpoint.
	// omitempty: a false value is simply absent, which the dashboard reads as
	// "no chat" — and keeps records without the field decoding cleanly.
	SupportsChat bool   `json:"supportsChat,omitempty"`
	Dismissed    bool   `json:"dismissed,omitempty"`
	ParentID     string `json:"parentId,omitempty"`
	// OrgID binds the agent to an organization (the SaaS account authorization
	// boundary). Set server-side at spawn — from the preregistered record for a
	// self-registering sidecar, never from its request body — and inherited by
	// child agents from their parent's record. Absent = an org-less (global)
	// agent, today's behaviour. Distinct from TenantID (the consumer primitive).
	OrgID string `json:"orgId,omitempty"`
	// Placement records WHERE the agent runs (Phase 10): "" / PlacementK8s is a
	// platform-hosted agent backed by an AgentWorkload CR (the default, so it is
	// omitempty); PlacementRemote is a consumer-hosted agent with no CR. Set
	// server-side at preregistration and IMMUTABLE thereafter (a self-registering
	// sidecar never sets it from its body) so it cannot be used to make a k8s
	// agent immortal against the operator's stale sweep. Distinct from OrgID and
	// TenantID.
	Placement string `json:"placement,omitempty"`
	// ConsentRequired mirrors the spawn-time decision (parent template gates this
	// child behind CIBA consent) onto the durable record. It exists so a
	// remote-substrate reconcile-on-connect (which re-materializes a CR from the
	// record alone, after the live spawn push was missed while the substrate was
	// offline) preserves the consent gate — without it an offline-then-reconciled
	// child could serve tokens without the CIBA backchannel (a consent bypass).
	// Server-set at preregistration alongside Placement; platform-hosted spawns
	// leave it false (their CR carries the flag directly and survives in etcd).
	ConsentRequired bool `json:"consentRequired,omitempty"`
}
