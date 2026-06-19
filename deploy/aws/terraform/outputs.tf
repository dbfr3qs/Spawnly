output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "region" {
  description = "AWS region."
  value       = var.region
}

output "oidc_provider" {
  description = "Cluster OIDC issuer (host/path, no scheme) — what STS federation trusts."
  value       = module.eks.oidc_provider
}

output "agent_role_arn" {
  description = "IAM role ARN for agents (bound to the agent ServiceAccount via the Pod Identity association)."
  value       = aws_iam_role.agent.arn
}

output "cluster_arn" {
  description = "EKS cluster ARN — the expected eks-cluster-arn in the attested principal_tags (STSWEB_CLUSTER_ARN)."
  value       = module.eks.cluster_arn
}

output "agent_service_account" {
  description = "ServiceAccount name agents run as."
  value       = var.agent_service_account
}

output "ecr_registry" {
  description = "ECR registry host to push images to (docker tag/push <host>/<repo>:<tag>)."
  value       = split("/", values(aws_ecr_repository.this)[0].repository_url)[0]
}

output "ecr_repository_urls" {
  description = "Per-image ECR repository URLs."
  value       = { for name, repo in aws_ecr_repository.this : name => repo.repository_url }
}

output "kubeconfig_command" {
  description = "Run this to point kubectl at the cluster."
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}
