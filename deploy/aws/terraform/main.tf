data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 2)
}

# ── Networking ────────────────────────────────────────────────────────────────
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.8"

  name = "${var.cluster_name}-vpc"
  cidr = "10.0.0.0/16"

  azs             = local.azs
  private_subnets = ["10.0.1.0/24", "10.0.2.0/24"]
  public_subnets  = ["10.0.101.0/24", "10.0.102.0/24"]

  enable_nat_gateway   = true
  single_nat_gateway   = true
  enable_dns_hostnames = true

  # Tags EKS uses to discover subnets for load balancers.
  public_subnet_tags  = { "kubernetes.io/role/elb" = "1" }
  private_subnet_tags = { "kubernetes.io/role/internal-elb" = "1" }
}

# ── EKS cluster (creates the IAM OIDC provider via enable_irsa) ────────────────
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.8"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  # Publish the API + OIDC issuer publicly so the platform's verifiers (and your
  # kubectl) can reach them. The OIDC issuer is what STS federation trusts.
  cluster_endpoint_public_access = true
  enable_irsa                    = true

  # Grant the principal running `terraform apply` cluster-admin via an EKS access
  # entry, so your kubectl works immediately after apply (v20 default is false).
  enable_cluster_creator_admin_permissions = true

  eks_managed_node_groups = {
    default = {
      instance_types = [var.node_instance_type]
      min_size       = 1
      max_size       = 3
      desired_size   = 2

      # Name the node IAM role with the cluster prefix so the least-privilege
      # Terraform policy (scoped to role/spawnly*) matches it. Without this the
      # module derives "default-eks-node-group-*" from the node-group key.
      iam_role_name            = "${var.cluster_name}-node"
      iam_role_use_name_prefix = true
    }
  }
}

# ── Agent IAM role (IRSA) ─────────────────────────────────────────────────────
# Federates the cluster OIDC provider; assumable only by the agent ServiceAccount.
# No permissions policy is attached: attestation only needs sts:GetCallerIdentity,
# which requires no IAM permissions. Add policies here if agents call AWS APIs.
data "aws_iam_policy_document" "agent_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [module.eks.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${module.eks.oidc_provider}:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "${module.eks.oidc_provider}:sub"
      values   = ["system:serviceaccount:${var.agent_namespace}:${var.agent_service_account}"]
    }
  }
}

resource "aws_iam_role" "agent" {
  name               = "${var.cluster_name}-agent"
  assume_role_policy = data.aws_iam_policy_document.agent_assume.json
}

# ── ECR repositories ──────────────────────────────────────────────────────────
resource "aws_ecr_repository" "this" {
  for_each = toset(var.ecr_repositories)

  name                 = each.value
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}
