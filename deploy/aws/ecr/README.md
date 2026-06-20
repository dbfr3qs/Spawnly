# ECR repositories (separate Terraform state)

These image repositories live in their **own Terraform root/state**, independent
of the cluster (`deploy/aws/terraform`). Why: `down.sh` destroys only the
cluster, so the images you push **persist across down/up cycles** — push once,
reuse. (In a single root, `terraform destroy` would force-delete the repos and
every recreate would need a full 15-image re-push.)

- **Created/updated by** `up.sh` (idempotent: `terraform -chdir=deploy/aws/ecr apply`).
- **NOT destroyed by** `down.sh`. To delete the repos + images deliberately:
  `./deploy/aws/destroy-ecr.sh`.
- The registry host scripts use comes from `terraform -chdir=deploy/aws/ecr output -raw ecr_registry`.
