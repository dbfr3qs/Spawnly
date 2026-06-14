// internal/registry/types.go
package registry

// Template status values. An empty Status is treated as active/spawnable.
// "deprecated" is tolerated for backward compatibility but never enforced.
const (
	TemplateStatusActive   = "active"
	TemplateStatusDisabled = "disabled"
)

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
}
