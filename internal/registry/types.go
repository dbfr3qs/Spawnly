// internal/registry/types.go
package registry

type AgentTemplate struct {
	AgentType string       `json:"agentType"`
	Version   string       `json:"version"`
	Status    string       `json:"status"` // active | deprecated
	Meta      TemplateMeta `json:"meta"`
	Runtime   RuntimeSpec  `json:"runtimeSpec"`
	AuthZ     AuthZSpec    `json:"authzTemplate"`
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
}
