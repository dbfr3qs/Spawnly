variable "region" {
  description = "AWS region (ACM cert for the ALB must be in the ALB's region)."
  type        = string
  default     = "us-east-1"
}

variable "domain" {
  description = "Apex domain. The hosted zone, the ACM cert (apex + wildcard), and the docs CNAME are created under it."
  type        = string
  default     = "spawnly.run"
}

variable "docs_pages_target" {
  description = "GitHub Pages target for docs.<domain> (the <user>.github.io host, NOT the project path)."
  type        = string
  default     = "dbfr3qs.github.io"
}
