#!/usr/bin/env bash
# SPIKE (spoof): can a workload forge its identity by passing kubernetes-pod-name
# as a caller tag to GetWebIdentityToken?
#
# Runs under EKS Pod Identity and calls GetWebIdentityToken WITH a forged
# `--tags Key=kubernetes-pod-name,Value=agent-evil-imposter-pod`. The decoded JWT
# shows the forgery lands in `request_tags` (caller-supplied) while the EKS-set
# `principal_tags.kubernetes-pod-name` stays the REAL pod — and the verifier reads
# only principal_tags. Demonstrates the attestor can't be spoofed.
#
# Note: passing tags needs sts:TagGetWebIdentityToken (this spike grants it). The
# real agent role does NOT have it, so an agent can't even put anything in
# request_tags — this spike grants it only to prove the verifier ignores it anyway.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)"

REGION="${AWS_REGION:-us-east-1}"
CLUSTER="${CLUSTER:-spawnly}"
NS=default
SA=podid-spoof
ROLE=spawnly-podid-spoof
ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
ROLE_ARN="arn:aws:iam::${ACCOUNT}:role/${ROLE}"

echo "==> ensuring eks-pod-identity-agent addon is active"
aws eks create-addon --cluster-name "$CLUSTER" --addon-name eks-pod-identity-agent --region "$REGION" >/dev/null 2>&1 || true
aws eks wait addon-active --cluster-name "$CLUSTER" --addon-name eks-pod-identity-agent --region "$REGION" 2>/dev/null || true

echo "==> creating IAM role $ROLE (GetWebIdentityToken + TagGetWebIdentityToken)"
aws iam create-role --role-name "$ROLE" \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"pods.eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}' >/dev/null 2>&1 || echo "   (role already exists)"
aws iam put-role-policy --role-name "$ROLE" --policy-name getwebidentitytoken \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sts:GetWebIdentityToken","sts:TagGetWebIdentityToken"],"Resource":"*"}]}'

kubectl get sa "$SA" -n "$NS" >/dev/null 2>&1 || kubectl create sa "$SA" -n "$NS"
echo "==> creating pod identity association ($NS/$SA -> $ROLE)"
aws eks create-pod-identity-association --cluster-name "$CLUSTER" --namespace "$NS" \
  --service-account "$SA" --role-arn "$ROLE_ARN" --region "$REGION" >/dev/null 2>&1 || echo "   (association already exists)"

cleanup() {
  echo "==> cleanup"
  kubectl delete job podid-spoof-spike -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  AID="$(aws eks list-pod-identity-associations --cluster-name "$CLUSTER" --namespace "$NS" --service-account "$SA" --region "$REGION" --query 'associations[0].associationId' --output text 2>/dev/null)"
  if [ -n "$AID" ] && [ "$AID" != "None" ]; then
    aws eks delete-pod-identity-association --cluster-name "$CLUSTER" --association-id "$AID" --region "$REGION" >/dev/null 2>&1 || true
  fi
  kubectl delete sa "$SA" -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  aws iam delete-role-policy --role-name "$ROLE" --policy-name getwebidentitytoken >/dev/null 2>&1 || true
  aws iam delete-role --role-name "$ROLE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> waiting 25s for IAM + association propagation"
sleep 25

kubectl delete job podid-spoof-spike -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
cat <<YAML | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: podid-spoof-spike
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
            - name: AWS_STS_REGIONAL_ENDPOINTS
              value: regional
          command: ["/bin/sh","-c"]
          args:
            - |
              echo "=== GetCallerIdentity (real pod) ==="; aws sts get-caller-identity || true
              echo; echo "=== GetWebIdentityToken WITH forged --tags kubernetes-pod-name=agent-evil-imposter-pod ==="
              TOKEN=\$(aws sts get-web-identity-token --audience spawnly --signing-algorithm RS256 --duration-seconds 300 \
                --tags Key=kubernetes-pod-name,Value=agent-evil-imposter-pod \
                --query WebIdentityToken --output text) || { echo "FAILED"; exit 1; }
              decode() { P="\$1"; case \$((\${#P} % 4)) in 2) P="\${P}==";; 3) P="\${P}=";; esac; echo "\$P" | tr '_-' '/+' | base64 -d 2>/dev/null; echo; }
              echo; echo "=== JWT payload ==="; decode "\$(echo "\$TOKEN" | cut -d. -f2)"
YAML

kubectl wait --for=condition=complete job/podid-spoof-spike -n "$NS" --timeout=180s 2>/dev/null || true
echo "===================== SPOOF SPIKE OUTPUT ====================="
kubectl logs job/podid-spoof-spike -n "$NS" || true
echo "============================================================="
cat <<'NOTES'

EXPECTED:
  "https://sts.amazonaws.com/": {
    "principal_tags": { "kubernetes-pod-name": "podid-spoof-spike-XXXXX", ... },   <- REAL (EKS-set, what the verifier reads)
    "request_tags":   { "kubernetes-pod-name": "agent-evil-imposter-pod" }          <- FORGED (caller-set, IGNORED)
  }
The registrant/IS verifier reads principal_tags only, so the forgery has no effect.
(The real agent role lacks sts:TagGetWebIdentityToken, so an agent can't even populate request_tags.)
NOTES
