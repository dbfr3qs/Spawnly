#!/usr/bin/env bash
# SPIKE: what does sts:GetWebIdentityToken actually return?
#
# Runs GetWebIdentityToken from inside a pod (IRSA identity) and decodes the
# resulting AWS-signed JWT, so we can see:
#   - whether the API is available + outbound federation is enabled on the account
#   - what claims appear: sub/iss/aud, the caller-passed `agent-id` tag, and
#     CRUCIALLY whether any *attested* identity context shows up as claims
#   - confirmation that a caller-passed tag becomes a readable JWT claim
#
# Phase 1 (this script): IRSA. Answers "does it work + what do caller-passed tags
# look like". IRSA has no session tags, so the attested-session-tag question is a
# Phase 2 follow-up under EKS Pod Identity (see the notes printed at the end).
#
# Run with the same AWS creds you used for terraform (e.g. AWS_PROFILE=spawnly).
# Needs: cluster up + kubeconfig + the spawnly-agent IAM role (terraform output).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

REGION="${AWS_REGION:-us-east-1}"
ROLE="${AGENT_ROLE_NAME:-spawnly-agent}"
SA="${AGENT_SA:-spawnly-agent}"
NS="${AGENT_NS:-default}"
ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
ROLE_ARN="arn:aws:iam::${ACCOUNT}:role/${ROLE}"

echo "==> account=$ACCOUNT region=$REGION role=$ROLE sa=$NS/$SA"

# 1. Temporarily allow the agent role to call GetWebIdentityToken (it has no
#    permissions policy by default). Removed on exit.
echo "==> granting sts:GetWebIdentityToken to $ROLE (temporary inline policy)"
aws iam put-role-policy --role-name "$ROLE" --policy-name spike-getwebidentitytoken \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sts:GetWebIdentityToken","sts:TagGetWebIdentityToken"],"Resource":"*"}]}'

cleanup() {
  echo "==> cleanup"
  aws iam delete-role-policy --role-name "$ROLE" --policy-name spike-getwebidentitytoken 2>/dev/null || true
  kubectl delete job getwebid-spike -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 2. Ensure the IRSA-annotated SA exists.
kubectl get sa "$SA" -n "$NS" >/dev/null 2>&1 || kubectl create sa "$SA" -n "$NS"
kubectl annotate sa "$SA" -n "$NS" "eks.amazonaws.com/role-arn=$ROLE_ARN" --overwrite >/dev/null

# 3. Run the probe from the pod's IRSA identity.
kubectl delete job getwebid-spike -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
cat <<YAML | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: getwebid-spike
  namespace: $NS
spec:
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: $SA
      restartPolicy: Never
      containers:
        - name: spike
          image: amazon/aws-cli:latest
          env:
            - name: AWS_REGION
              value: "$REGION"
            - name: AWS_STS_REGIONAL_ENDPOINTS   # GetWebIdentityToken is NOT on the global endpoint
              value: regional
          command: ["/bin/sh","-c"]
          args:
            - |
              echo "=== aws cli version ==="; aws --version
              echo; echo "=== GetCallerIdentity (this pod's STS principal) ==="
              aws sts get-caller-identity || true
              echo; echo "=== GetWebIdentityToken (caller-passed tag agent-id=spike-abc123) ==="
              TOKEN=\$(aws sts get-web-identity-token \
                --audience spawnly-spike \
                --signing-algorithm RS256 \
                --duration-seconds 300 \
                --tags Key=agent-id,Value=spike-abc123 \
                --query WebIdentityToken --output text) || { echo "GetWebIdentityToken FAILED (see error above)"; exit 1; }
              echo "ok, token length: \${#TOKEN}"
              decode() { P="\$1"; case \$((\${#P} % 4)) in 2) P="\${P}==";; 3) P="\${P}=";; esac; echo "\$P" | tr '_-' '/+' | base64 -d 2>/dev/null; echo; }
              echo; echo "=== JWT header ===";  decode "\$(echo "\$TOKEN" | cut -d. -f1)"
              echo "=== JWT payload (CLAIMS — this is the answer) ==="; decode "\$(echo "\$TOKEN" | cut -d. -f2)"
YAML

# 4. Wait + show output.
kubectl wait --for=condition=complete job/getwebid-spike -n "$NS" --timeout=150s 2>/dev/null || true
echo "===================== SPIKE OUTPUT ====================="
kubectl logs job/getwebid-spike -n "$NS" || true
echo "======================================================="
cat <<'NOTES'

WHAT TO LOOK FOR in the JWT payload:
  - iss  -> an STS issuer URL with a JWKS our verifier can fetch (readback works)
  - sub  -> the caller's AWS identity (the assumed-role principal)
  - aud  -> "spawnly-spike" (we control this)
  - the caller-passed tag (agent-id=spike-abc123) -> appears as a claim?
            (confirms tags become READABLE claims — the core capability)
  - any *attested* pod/identity context we did NOT pass in?
            (under IRSA there won't be session tags; that's Phase 2)

If you see "OutboundWebIdentityFederationDisabled": enable it once per account:
  aws iam enable-outbound-web-identity-federation
  aws iam get-outbound-web-identity-federation-info   # shows the issuer URL + JWKS
then re-run.

PHASE 2 (attested session tags) needs EKS Pod Identity, which tags the session
with cluster-set kubernetes-pod-name/uid. We'll add that association and re-probe
to see whether GetWebIdentityToken surfaces those attested tags as claims.
NOTES
