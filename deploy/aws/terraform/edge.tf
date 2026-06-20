# ── Public edge IAM: AWS Load Balancer Controller + external-dns ──────────────
# Gated behind enable_public_edge so a plain (non-public) deploy creates nothing
# here — including the data.aws_route53_zone lookup, which would otherwise error
# at apply on any account that hasn't applied the DNS root. up.sh sets
# TF_VAR_enable_public_edge=true only when deploy/aws/dns is applied.
#
# The controllers themselves are installed via Helm (deploy/aws/install-edge.sh);
# this provisions their IAM via EKS Pod Identity. Both roles reuse the
# agent_assume trust (pods.eks.amazonaws.com + sts:TagSession).

# The persistent DNS root (deploy/aws/dns) owns the hosted zone; look it up to
# scope external-dns to exactly that zone.
data "aws_route53_zone" "primary" {
  count = var.enable_public_edge ? 1 : 0
  name  = var.domain
}

# AWS Load Balancer Controller — the AWS-published IAM policy (docs/install/iam_policy.json).
resource "aws_iam_policy" "lbc" {
  count  = var.enable_public_edge ? 1 : 0
  name   = "${var.cluster_name}-lbc"
  policy = file("${path.module}/lbc-iam-policy.json")
}

resource "aws_iam_role" "lbc" {
  count              = var.enable_public_edge ? 1 : 0
  name               = "${var.cluster_name}-lbc"
  assume_role_policy = data.aws_iam_policy_document.agent_assume.json
}

resource "aws_iam_role_policy_attachment" "lbc" {
  count      = var.enable_public_edge ? 1 : 0
  role       = aws_iam_role.lbc[0].name
  policy_arn = aws_iam_policy.lbc[0].arn
}

resource "aws_eks_pod_identity_association" "lbc" {
  count           = var.enable_public_edge ? 1 : 0
  cluster_name    = module.eks.cluster_name
  namespace       = "kube-system"
  service_account = "aws-load-balancer-controller"
  role_arn        = aws_iam_role.lbc[0].arn
}

# external-dns — scoped to changing records in our hosted zone only.
resource "aws_iam_policy" "external_dns" {
  count = var.enable_public_edge ? 1 : 0
  name  = "${var.cluster_name}-external-dns"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["route53:ChangeResourceRecordSets"]
        Resource = ["arn:aws:route53:::hostedzone/${data.aws_route53_zone.primary[0].zone_id}"]
      },
      {
        Effect   = "Allow"
        Action   = ["route53:ListHostedZones", "route53:ListResourceRecordSets", "route53:ListTagsForResources"]
        Resource = ["*"]
      },
    ]
  })
}

resource "aws_iam_role" "external_dns" {
  count              = var.enable_public_edge ? 1 : 0
  name               = "${var.cluster_name}-external-dns"
  assume_role_policy = data.aws_iam_policy_document.agent_assume.json
}

resource "aws_iam_role_policy_attachment" "external_dns" {
  count      = var.enable_public_edge ? 1 : 0
  role       = aws_iam_role.external_dns[0].name
  policy_arn = aws_iam_policy.external_dns[0].arn
}

resource "aws_eks_pod_identity_association" "external_dns" {
  count           = var.enable_public_edge ? 1 : 0
  cluster_name    = module.eks.cluster_name
  namespace       = "kube-system"
  service_account = "external-dns"
  role_arn        = aws_iam_role.external_dns[0].arn
}
