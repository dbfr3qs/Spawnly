// internal/registry/types.go
package registry

type AgentTemplate struct {
	AgentType  string           `json:"agentType"`
	Version    string           `json:"version"`
	Status     string           `json:"status"` // active | deprecated
	Meta       TemplateMeta     `json:"meta"`
	Runtime    RuntimeSpec      `json:"runtimeSpec"`
	AuthZ      AuthZSpec        `json:"authzTemplate"`
	Delegation DelegationPolicy `json:"delegation"`
}

// DelegationPolicy describes which child agent-types a given agent-type may
// delegate to, which scopes it may grant them, and the maximum delegation depth.
type DelegationPolicy struct {
	AllowedChildTypes []string `json:"allowedChildTypes"`
	GrantableScopes   []string `json:"grantableScopes"`
	MaxDepth          int      `json:"maxDepth"`
}

// SpawnDecision is the registry's answer to "may this parent spawn this child
// type?". Returned by GET /v1/spawn-policy and consumed by the orchestrator at
// spawn time. The edge is gated by the parent template's AllowedChildTypes
// (reused from DelegationPolicy); a parent that lists no child types may spawn
// none (deny-by-default).
type SpawnDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
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
	Dismissed bool   `json:"dismissed,omitempty"`
	ParentID  string `json:"parentId,omitempty"`
}
