# ECR repositories — a SEPARATE Terraform root/state from the cluster, so the
# images survive a cluster teardown (down.sh destroys only the cluster root).
# Push images once and reuse them across down/up cycles.
#
# force_delete stays true so a deliberate teardown of THIS root (destroy-ecr.sh)
# still works even with images present; down.sh never touches this root.
resource "aws_ecr_repository" "this" {
  for_each = toset(var.ecr_repositories)

  name                 = each.value
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}
