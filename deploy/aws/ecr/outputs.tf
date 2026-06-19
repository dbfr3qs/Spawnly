output "ecr_registry" {
  description = "ECR registry host (docker tag/push <host>/<repo>:<tag>)."
  value       = split("/", values(aws_ecr_repository.this)[0].repository_url)[0]
}

output "ecr_repository_urls" {
  description = "Per-image ECR repository URLs."
  value       = { for name, repo in aws_ecr_repository.this : name => repo.repository_url }
}
