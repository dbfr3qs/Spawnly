# Least-privilege policy for the Terraform principal

`terraform-principal-policy.json` is the IAM policy to attach to the user/role
that runs `terraform apply` in `deploy/aws/terraform`. It avoids
`AdministratorAccess` by:

- **Region-boxing the infrastructure services** (`ec2`, `eks`, `elasticloadbalancing`,
  `autoscaling`, `kms`, `logs`) with an `aws:RequestedRegion` condition — broad
  within one region, nothing outside it.
- **Tightly scoping the identity-sensitive services**: IAM by the `spawnly*`
  role/policy/instance-profile name prefixes (+ `PassRole` constrained to EKS/EC2
  and service-linked roles by service name), and ECR by the `agent-*` repository
  prefix.

This is a deliberate balance: a hand-enumerated `ec2:` action list tends to break
mid-apply as the modules evolve, so EC2/EKS are granted at service level but
fenced to one region, while the powerful IAM/ECR grants are resource-scoped.

## Assumptions

- `cluster_name = "spawnly"` (the default). The IAM/ECR ARNs key on the
  `spawnly` / `agent-` prefixes — if you change `cluster_name` or
  `ecr_repositories`, update the ARNs to match.
- It bootstraps Terraform itself, so attach it **manually** (console or CLI)
  before the first `apply` — it can't be a Terraform-managed resource.

## Attach it

```bash
# Replace the two placeholders first:
sed -i "s/ACCOUNT_ID/$(aws sts get-caller-identity --query Account --output text)/g; s/REGION/us-east-1/g" \
  terraform-principal-policy.json

aws iam create-policy \
  --policy-name spawnly-terraform \
  --policy-document file://terraform-principal-policy.json

# Then attach the returned ARN to your user or role:
aws iam attach-user-policy  --user-name <you>  --policy-arn <arn>   # for an IAM user
# or
aws iam attach-role-policy  --role-name <role> --policy-arn <arn>   # for an assumed role
```

## Tightening further (optional)

To drop the `kms` and `logs` grants entirely, disable those features in
`deploy/aws/terraform/main.tf` (`create_kms_key = false`,
`cluster_encryption_config = {}`, `create_cloudwatch_log_group = false`,
`cluster_enabled_log_types = []`) — at the cost of secret-envelope encryption and
control-plane logging. Acceptable for an ephemeral demo; not recommended
otherwise.
