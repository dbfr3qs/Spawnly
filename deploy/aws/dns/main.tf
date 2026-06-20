# DNS + TLS — a PERSISTENT Terraform root/state, independent of the cluster.
# The registrar delegates nameservers to this hosted zone, so it must survive
# `down.sh` (which destroys only the cluster). The ACM cert is DNS-validated
# against this zone and reused by the ALB ingress. Tear this down only via an
# explicit full teardown (`down.sh --all`) — it breaks the registrar delegation.

resource "aws_route53_zone" "primary" {
  name = var.domain
}

# Apex + wildcard so spawnly.run and auth.spawnly.run (and future subdomains) are
# covered. docs.<domain> is served by GitHub Pages with its own TLS, not this cert.
resource "aws_acm_certificate" "primary" {
  domain_name               = var.domain
  subject_alternative_names = ["*.${var.domain}"]
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "acm_validation" {
  for_each = {
    for dvo in aws_acm_certificate.primary.domain_validation_options :
    dvo.domain_name => {
      name   = dvo.resource_record_name
      type   = dvo.resource_record_type
      record = dvo.resource_record_value
    }
  }

  zone_id         = aws_route53_zone.primary.zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

# Completes only once the validation records resolve publicly — i.e. AFTER the
# registrar's nameservers point at this zone (see README). On a first apply you
# may need: apply the zone, cut over NS at the registrar, then apply again.
resource "aws_acm_certificate_validation" "primary" {
  certificate_arn         = aws_acm_certificate.primary.arn
  validation_record_fqdns = [for r in aws_route53_record.acm_validation : r.fqdn]
}

# docs.<domain> -> GitHub Pages (Pages provisions its own TLS).
resource "aws_route53_record" "docs" {
  zone_id = aws_route53_zone.primary.zone_id
  name    = "docs.${var.domain}"
  type    = "CNAME"
  ttl     = 300
  records = [var.docs_pages_target]
}
