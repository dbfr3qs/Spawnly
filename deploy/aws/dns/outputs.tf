output "zone_id" {
  description = "Route53 hosted zone id for the domain."
  value       = aws_route53_zone.primary.zone_id
}

output "name_servers" {
  description = "Set these as the domain's nameservers at the registrar (one-time)."
  value       = aws_route53_zone.primary.name_servers
}

output "acm_certificate_arn" {
  description = "Validated ACM certificate ARN for the ALB ingress (apex + wildcard)."
  value       = aws_acm_certificate_validation.primary.certificate_arn
}
