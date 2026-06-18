#!/usr/bin/env bash
# Render the least-privilege Terraform policy from the committed template,
# filling ACCOUNT_ID and REGION from the environment (no hand-editing, and the
# template stays placeholder-only so a real account id is never committed).
#
#   AWS_ACCOUNT_ID  (optional; defaults to `aws sts get-caller-identity`)
#   AWS_REGION      (default us-east-1)
#
# Writes terraform-principal-policy.rendered.json (gitignored). With --apply it
# also creates or updates the managed policy `spawnly-terraform`.
#
#   ./render-policy.sh            # render only
#   ./render-policy.sh --apply    # render + create/update the IAM policy
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

ACCOUNT_ID="${AWS_ACCOUNT_ID:-$(aws sts get-caller-identity --query Account --output text)}"
REGION="${AWS_REGION:-us-east-1}"
SRC="terraform-principal-policy.json"
OUT="terraform-principal-policy.rendered.json"

sed "s/ACCOUNT_ID/${ACCOUNT_ID}/g; s/REGION/${REGION}/g" "$SRC" > "$OUT"
echo "wrote $OUT (account=${ACCOUNT_ID} region=${REGION})"

if [ "${1:-}" = "--apply" ]; then
  ARN="arn:aws:iam::${ACCOUNT_ID}:policy/spawnly-terraform"
  if aws iam get-policy --policy-arn "$ARN" >/dev/null 2>&1; then
    echo "==> updating existing policy $ARN (new default version)"
    aws iam create-policy-version --policy-arn "$ARN" \
      --policy-document "file://$OUT" --set-as-default >/dev/null
    # Managed policies cap at 5 versions; prune the oldest non-default if needed.
    aws iam list-policy-versions --policy-arn "$ARN" \
      --query 'Versions[?!IsDefaultVersion].VersionId' --output text \
      | tr '\t' '\n' | tail -n +5 \
      | while read -r v; do [ -n "$v" ] && aws iam delete-policy-version --policy-arn "$ARN" --version-id "$v"; done
  else
    echo "==> creating policy $ARN"
    aws iam create-policy --policy-name spawnly-terraform \
      --policy-document "file://$OUT" >/dev/null
  fi
  echo "policy ready: $ARN  (attach it to your Terraform principal)"
fi
