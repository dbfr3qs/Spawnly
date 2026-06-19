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

  # Additional cluster-admins (e.g. an AWS SSO role whose ARN the creator-admin
  # entry mis-records). Declarative — no post-apply access-entry commands needed.
  access_entries = {
    for arn in var.cluster_admin_principal_arns : arn => {
      principal_arn = arn
      policy_associations = {
        admin = {
          policy_arn   = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
          access_scope = { type = "cluster" }
        }
      }
    }
  }

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

# ── EKS Pod Identity ──────────────────────────────────────────────────────────
# The Pod Identity agent injects AWS credentials into agent pods and stamps the
# session with cluster-attested tags (kubernetes-pod-name/uid). The hardened
# attestor (aws-stsweb) reads kubernetes-pod-name from the GetWebIdentityToken
# JWT to derive the agent id.
resource "aws_eks_addon" "pod_identity" {
  cluster_name = module.eks.cluster_name
  addon_name   = "eks-pod-identity-agent"
}

# ── Agent IAM role (Pod Identity) ─────────────────────────────────────────────
# Assumed by the EKS Pod Identity service on behalf of the agent ServiceAccount.
# The only permission it needs is sts:GetWebIdentityToken (outbound web identity
# federation); add more here if agents call other AWS APIs.
data "aws_iam_policy_document" "agent_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole", "sts:TagSession"]

    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "agent" {
  name               = "${var.cluster_name}-agent"
  assume_role_policy = data.aws_iam_policy_document.agent_assume.json
}

resource "aws_iam_role_policy" "agent_getwebidentitytoken" {
  name = "getwebidentitytoken"
  role = aws_iam_role.agent.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "sts:GetWebIdentityToken", Resource = "*" }]
  })
}

# Bind the agent ServiceAccount to the role. One association covers every agent
# pod; each pod's token still carries its own attested kubernetes-pod-name.
resource "aws_eks_pod_identity_association" "agent" {
  cluster_name    = module.eks.cluster_name
  namespace       = var.agent_namespace
  service_account = var.agent_service_account
  role_arn        = aws_iam_role.agent.arn
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
