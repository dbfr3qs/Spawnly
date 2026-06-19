#!/usr/bin/env bash
# SPIKE Phase 2: does GetWebIdentityToken carry CLUSTER-ATTESTED session tags?
#
# Runs a pod under EKS Pod Identity (which stamps cluster-set session tags
# kubernetes-pod-name / kubernetes-pod-uid that the workload CANNOT forge), then
# calls GetWebIdentityToken WITHOUT passing any --tags. So any tag-like claim in
# the returned JWT came from the attested session, not from us.
#
#   if pod tags appear  -> attested + readable + STS-native identity (the ideal)
#   if they don't       -> GetWebIdentityToken won't carry attested tags; we use
#                          the "readable carrier + cross-check" design instead.
#
# Creates: eks-pod-identity-agent addon, an IAM role trusting pods.eks.amazonaws.com,
# a ServiceAccount, and a pod identity association. Cleans up everything except the
# addon on exit. Run as an admin identity (you're SSO admin).
set -uo pipefail   # not -e: cleanup must run even if a step fails

cd "$(git rev-parse --show-toplevel)"

REGION="${AWS_REGION:-us-east-1}"
CLUSTER="${CLUSTER:-spawnly}"
NS=default
SA=podid-spike
ROLE=spawnly-podid-spike
ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
ROLE_ARN="arn:aws:iam::${ACCOUNT}:role/${ROLE}"

echo "==> account=$ACCOUNT cluster=$CLUSTER region=$REGION"

echo "==> ensuring eks-pod-identity-agent addon is active"
aws eks create-addon --cluster-name "$CLUSTER" --addon-name eks-pod-identity-agent --region "$REGION" >/dev/null 2>&1 || true
aws eks wait addon-active --cluster-name "$CLUSTER" --addon-name eks-pod-identity-agent --region "$REGION" 2>/dev/null || true

echo "==> creating IAM role $ROLE (trusts pods.eks.amazonaws.com; allows GetWebIdentityToken)"
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
  kubectl delete job podid-getwebid-spike -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  AID="$(aws eks list-pod-identity-associations --cluster-name "$CLUSTER" --namespace "$NS" --service-account "$SA" --region "$REGION" --query 'associations[0].associationId' --output text 2>/dev/null)"
  if [ -n "$AID" ] && [ "$AID" != "None" ]; then
    aws eks delete-pod-identity-association --cluster-name "$CLUSTER" --association-id "$AID" --region "$REGION" >/dev/null 2>&1 || true
  fi
  kubectl delete sa "$SA" -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  aws iam delete-role-policy --role-name "$ROLE" --policy-name getwebidentitytoken >/dev/null 2>&1 || true
  aws iam delete-role --role-name "$ROLE" >/dev/null 2>&1 || true
  echo "   (left eks-pod-identity-agent addon installed; remove with:"
  echo "      aws eks delete-addon --cluster-name $CLUSTER --addon-name eks-pod-identity-agent --region $REGION )"
}
trap cleanup EXIT

echo "==> waiting 25s for IAM + association propagation"
sleep 25

kubectl delete job podid-getwebid-spike -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
cat <<YAML | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: podid-getwebid-spike
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
              echo "=== AWS_* env (Pod Identity injects AWS_CONTAINER_CREDENTIALS_FULL_URI) ==="
              env | grep -i AWS_ | sort
              echo; echo "=== GetCallerIdentity (note the cluster-set session name) ==="
              aws sts get-caller-identity || true
              echo; echo "=== GetWebIdentityToken (NO --tags: any tag-like claim is cluster-attested) ==="
              TOKEN=\$(aws sts get-web-identity-token --audience spawnly-spike --signing-algorithm RS256 --duration-seconds 300 --query WebIdentityToken --output text) || { echo "GetWebIdentityToken FAILED (see error above)"; exit 1; }
              decode() { P="\$1"; case \$((\${#P} % 4)) in 2) P="\${P}==";; 3) P="\${P}=";; esac; echo "\$P" | tr '_-' '/+' | base64 -d 2>/dev/null; echo; }
              echo; echo "=== JWT payload (CLAIMS — the answer) ==="; decode "\$(echo "\$TOKEN" | cut -d. -f2)"
YAML

kubectl wait --for=condition=complete job/podid-getwebid-spike -n "$NS" --timeout=180s 2>/dev/null || true
echo "===================== PHASE 2 SPIKE OUTPUT ====================="
kubectl logs job/podid-getwebid-spike -n "$NS" || true
echo "==============================================================="
cat <<'NOTES'

WHAT TO LOOK FOR:
  - GetCallerIdentity session name like  eks-spawnly-<pod>-<uuid>  (cluster-set).
  - Inside the JWT's "https://sts.amazonaws.com/" claim, any tag we did NOT pass:
      session_tags / principal_tags with kubernetes-pod-name / kubernetes-pod-uid
    PRESENT -> cluster-attested, readable, STS-native identity. The ideal: no
               per-agent IAM, no cross-check.
    ABSENT  -> only federated_provider/principal_id (no pod tags). Then
               GetWebIdentityToken does not carry attested tags, and we go with
               "readable carrier (request_tags) + cross-check against the
               cluster-signed SA token".
NOTES
