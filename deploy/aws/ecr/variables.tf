variable "region" {
  description = "AWS region for the ECR repositories (match the cluster region)."
  type        = string
  default     = "us-east-1"
}

variable "ecr_repositories" {
  description = "Container image repositories (platform services + agents)."
  type        = list(string)
  # Image names match the Dockerfile stages (agent-<stage>, except agent-sidecar).
  # Mirrors `make print-SERVICES`.
  default = [
    "agent-operator",
    "agent-orchestrator",
    "agent-registry",
    "agent-sample-api",
    "agent-sidecar",
    "agent-dashboard",
    "agent-chain-worker",
    "agent-identity-server",
    "agent-weather-monitor",
    "agent-travel-specialist",
    "agent-travel-planner",
    "agent-travel-tools",
  ]
}
