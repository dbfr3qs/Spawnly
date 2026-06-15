#!/usr/bin/env bash
# test-provider.sh — the full gate for terraform-provider-spawnly, identical for
# local use (`make test-provider`) and CI. Brings up the lightweight registry
# testbed, runs fmt/vet/unit + acceptance + the seeded-template parity check,
# and always tears the testbed down.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROVIDER_DIR="$REPO_ROOT/terraform-provider-spawnly"
STATE_DIR="${SPAWNLY_ACC_STATE_DIR:-/tmp/spawnly-acc-testbed}"
export GOWORK=off # the provider is a standalone module, outside go.work

command -v terraform >/dev/null 2>&1 || {
  echo "ERROR: terraform CLI not found (https://developer.hashicorp.com/terraform/install)" >&2
  exit 1
}

cleanup() { "$REPO_ROOT/scripts/acc-testbed.sh" down >/dev/null 2>&1 || true; }
trap cleanup EXIT

cd "$PROVIDER_DIR"

echo "==> gofmt"
fmt_out="$(gofmt -l .)"
[ -z "$fmt_out" ] || { echo "gofmt needs running on:"; echo "$fmt_out"; exit 1; }

echo "==> terraform fmt (examples)"
terraform fmt -check -recursive examples/

echo "==> go vet"
go vet ./...

echo "==> unit tests"
go test ./... -count=1

echo "==> bringing up testbed"
"$REPO_ROOT/scripts/acc-testbed.sh" up
# shellcheck disable=SC1091
source "$STATE_DIR/env"

echo "==> acceptance tests"
TF_ACC=1 go test ./internal/provider/ -run TestAcc -count=1 -timeout 20m

echo "==> parity check (seeded templates round-trip on a fresh registry)"
make install >/dev/null
export TF_CLI_CONFIG_FILE="$PROVIDER_DIR/dev.tfrc"
cd examples/seeded-templates
rm -f terraform.tfstate terraform.tfstate.backup
terraform apply -auto-approve -no-color >/dev/null
if terraform plan -detailed-exitcode -no-color >/dev/null 2>&1; then
  echo "    parity: clean (no drift)"
else
  echo "PARITY DRIFT — the seeded-template HCL no longer round-trips:" >&2
  terraform plan -no-color >&2 || true
  exit 1
fi

echo ""
echo "ALL PROVIDER CHECKS PASSED"
