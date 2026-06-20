variable "region" {
  description = "AWS region for the EKS cluster and STS endpoint."
  type        = string
  default     = "us-east-1"
}

variable "cluster_name" {
  description = "Name of the EKS cluster (also used to name the agent IAM role)."
  type        = string
  default     = "spawnly"
}

variable "cluster_version" {
  description = "Kubernetes version for the EKS control plane."
  type        = string
  default     = "1.30"
}

variable "agent_namespace" {
  description = "Namespace the agent pods run in (must match the platform deploy)."
  type        = string
  default     = "default"
}

variable "agent_service_account" {
  description = "ServiceAccount agent pods run as; IRSA binds it to the agent IAM role. Must match AWS_AGENT_SERVICE_ACCOUNT on the operator."
  type        = string
  default     = "spawnly-agent"
}

variable "node_instance_type" {
  description = "EC2 instance type for the managed node group."
  type        = string
  default     = "t3.medium"
}

variable "domain" {
  description = "Domain whose Route53 zone (managed in deploy/aws/dns) external-dns is scoped to."
  type        = string
  default     = "spawnly.run"
}

variable "cluster_admin_principal_arns" {
  description = <<-EOT
    IAM principal ARNs to grant cluster-admin via EKS access entries at apply time
    — avoids the manual create-access-entry / associate-access-policy dance after
    apply. For an AWS SSO role, use the underlying IAM role ARN including the
    aws-reserved/sso.amazonaws.com path (find it with:
    aws iam list-roles --query "Roles[?contains(RoleName,'AWSReservedSSO_<permset>')].Arn").
    enable_cluster_creator_admin_permissions still covers the apply principal.
  EOT
  type        = list(string)
  default     = []
}
