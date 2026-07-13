#!/usr/bin/env bash
# scripts/apply-realm.sh — (re)apply the Keycloak `spawnly` realm via Terraform
# and reseed templates + demo orgs. Factored out of scripts/bootstrap.sh so it can
# be re-run standalone after a Keycloak restart (on kind the dev-file DB is wiped
# on restart, so any env change to the keycloak deployment requires re-applying
# the realm). It reconstructs the TF vars from the cluster's existing secrets, so
# it is idempotent and safe to run mid-session.
#
# Requires: a running keycloak deployment, the keycloak-admin + org-admin-auth
# secrets (created by bootstrap.sh), terraform, kubectl.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KC_TF_DIR="$REPO_ROOT/deploy/keycloak"

KC_ADMIN_PASSWORD="$(kubectl get secret keycloak-admin -o jsonpath='{.data.password}' | base64 -d)"
ORG_ADMIN_SECRET="$(kubectl get secret org-admin-auth -o jsonpath='{.data.secret}' 2>/dev/null | base64 -d || true)"
# SMTP config from the keycloak-smtp Secret (bootstrap.sh; kind default: Mailpit)
# so a mid-session re-apply keeps email verification consistent. An EMPTY host
# turns email verification off (verify_email = smtp_host != ""), which is the
# legitimate no-SMTP config — but because that also disables a security control,
# we must not conflate "secret intentionally absent" with a transient read error
# (RBAC denial, wrong context/namespace, API hiccup). So probe existence FIRST and
# fail LOUD: if the Secret exists we read it (a missing field is empty, fine); if
# it does NOT exist we warn explicitly that verification will be disabled before
# proceeding, rather than silently flipping it off.
SMTP_HOST="" SMTP_PORT="" SMTP_USER="" SMTP_PASS="" SMTP_FROM=""
if kubectl get secret keycloak-smtp -o name >/dev/null 2>&1; then
  SMTP_HOST="$(kubectl get secret keycloak-smtp -o jsonpath='{.data.host}' | base64 -d 2>/dev/null || true)"
  SMTP_PORT="$(kubectl get secret keycloak-smtp -o jsonpath='{.data.port}' | base64 -d 2>/dev/null || true)"
  SMTP_USER="$(kubectl get secret keycloak-smtp -o jsonpath='{.data.user}' | base64 -d 2>/dev/null || true)"
  SMTP_PASS="$(kubectl get secret keycloak-smtp -o jsonpath='{.data.password}' | base64 -d 2>/dev/null || true)"
  SMTP_FROM="$(kubectl get secret keycloak-smtp -o jsonpath='{.data.from}' | base64 -d 2>/dev/null || true)"
else
  echo "WARN: keycloak-smtp Secret not found — applying the realm with email verification DISABLED (verify_email=false). Run scripts/bootstrap.sh to (re)create it." >&2
fi

echo "==> Waiting for keycloak to be ready..."
kubectl rollout status deployment/keycloak --timeout=300s

echo "==> Applying Keycloak realm via Terraform..."
(
  KC_LOCAL_PORT=18099
  kubectl port-forward svc/keycloak "${KC_LOCAL_PORT}:8080" >/dev/null 2>&1 &
  KC_PF_PID=$!
  trap 'kill "$KC_PF_PID" 2>/dev/null || true' EXIT

  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "http://localhost:${KC_LOCAL_PORT}/realms/master/.well-known/openid-configuration" 2>/dev/null; then
      break
    fi
    sleep 2
  done
  curl -fsS -o /dev/null "http://localhost:${KC_LOCAL_PORT}/realms/master/.well-known/openid-configuration" \
    || { echo "ERROR: Keycloak admin API not reachable on localhost:${KC_LOCAL_PORT}" >&2; exit 1; }

  export TF_VAR_keycloak_url="http://localhost:${KC_LOCAL_PORT}"
  export TF_VAR_keycloak_admin_username="admin"
  export TF_VAR_keycloak_admin_password="${KC_ADMIN_PASSWORD}"
  # Default to the localhost dev origin, but honor a pre-set value so the
  # org-subdomains TLS mimic (scripts/local-tls.sh) can re-apply the realm for
  # https://localtest.me. Unset = today's behavior, byte-identical.
  export TF_VAR_dashboard_origin="${TF_VAR_dashboard_origin:-http://localhost:8090}"
  export TF_VAR_dashboard_client_secret="dashboard-secret"
  export TF_VAR_orchestrator_client_secret="orchestrator-secret"
  export TF_VAR_idp_consent_client_secret="idp-consent-secret"
  export TF_VAR_machine_client_secret="placeholder"
  export TF_VAR_org_admin_client_secret="${ORG_ADMIN_SECRET}"
  export TF_VAR_smtp_host="${SMTP_HOST}"
  export TF_VAR_smtp_port="${SMTP_PORT}"
  export TF_VAR_smtp_user="${SMTP_USER}"
  export TF_VAR_smtp_password="${SMTP_PASS}"
  export TF_VAR_smtp_from="${SMTP_FROM}"
  export TF_VAR_root_password="root"
  export TF_VAR_alice_password="alice"
  export TF_VAR_viewer_password="viewer"
  export TF_VAR_bob_password="bob"
  export TF_VAR_carol_password="carol"
  export TF_VAR_nomad_password="nomad"

  terraform -chdir="$KC_TF_DIR" init -input=false >/dev/null
  terraform -chdir="$KC_TF_DIR" apply -input=false -auto-approve
)
echo "  Keycloak realm applied (spawnly)."

echo "==> Restarting realm-dependent services..."
# The registry MUST restart too: re-applying the realm recreates the `org-admin`
# Keycloak client, so the registry's cached admin token/session goes stale and its
# org-directory calls (CreateOrg/AddMember) start failing with 401 until it
# re-authenticates. Restarting it also gives a clean in-memory store that the
# reseed below repopulates deterministically. (Without this, a mid-session
# apply-realm — e.g. caps.spec.ts toggling MAX_USERS — leaves the registry 401ing.)
kubectl rollout restart \
  deployment/registry deployment/orchestrator deployment/dashboard deployment/agent-operator >/dev/null
kubectl rollout status deployment/registry --timeout=120s
kubectl rollout status deployment/orchestrator --timeout=120s
kubectl rollout status deployment/dashboard --timeout=120s

echo "==> Reseeding templates + demo orgs..."
"$REPO_ROOT/scripts/seed.sh"
"$REPO_ROOT/scripts/reseed-orgs.sh"
echo "Done."
