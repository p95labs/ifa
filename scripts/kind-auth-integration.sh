#!/usr/bin/env bash
#
# kind-auth-integration.sh — Kubernetes smoke test for API bearer-token auth.
#
# Deploys IFA in a kind cluster with api_auth.enabled: true, verifies that
# data endpoints reject unauthenticated requests, accept the correct token,
# and that health probes remain unauthenticated. Also verifies the token does
# not appear in the rendered ConfigMap.
#
# Prerequisites: kind, kubectl, helm, docker, curl, jq
#
# Usage:
#   IFA_IMAGE=ifa:integration-test bash scripts/kind-auth-integration.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

KIND_CLUSTER="${KIND_CLUSTER:-ifa-auth-test}"
KIND_NODE_IMAGE="kindest/node:v1.31.0@sha256:53df588e04085fd41ae12de0c3fe4c72f7013bba32a20e7325357a1ac94ba865"
IFA_IMAGE="${IFA_IMAGE:-ifa:integration-test}"
IFA_NAMESPACE="ifa"
FIXTURE_NAMESPACE="inference"
LOCAL_PORT="${IFA_LOCAL_PORT:-18090}"
CHART_DIR="${ROOT}/deploy/helm/ifa"
IFA_API="http://localhost:${LOCAL_PORT}"
# Synthetic non-sensitive test token — never a real credential.
TEST_TOKEN="kind-auth-integration-test-token-abc"

PF_PID=""

cleanup() {
    local rc=$?
    if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
    echo "==> Deleting kind cluster ${KIND_CLUSTER}"
    kind delete cluster --name "${KIND_CLUSTER}" 2>/dev/null || true
    if [[ ${rc} -ne 0 ]]; then
        echo "==> Auth integration test FAILED (exit ${rc})" >&2
    fi
}
trap cleanup EXIT INT TERM

echo "==> Checking prerequisites"
for cmd in kind kubectl helm docker curl jq; do
    if ! command -v "${cmd}" > /dev/null 2>&1; then
        echo "ERROR: ${cmd} is required but not found in PATH" >&2
        exit 1
    fi
done

# ── Cluster ───────────────────────────────────────────────────────────────────
echo "==> Creating kind cluster ${KIND_CLUSTER}"
kind create cluster --name "${KIND_CLUSTER}" --image "${KIND_NODE_IMAGE}" \
    --wait 60s

echo "==> Loading IFA image into kind"
kind load docker-image "${IFA_IMAGE}" --name "${KIND_CLUSTER}"

kubectl create namespace "${IFA_NAMESPACE}"
kubectl create namespace "${FIXTURE_NAMESPACE}"

# ── Fixture metrics server ────────────────────────────────────────────────────
# A minimal busybox httpd that serves a static vLLM payload so IFA has
# something to scrape and the recommender has something to fire on.
FIXTURE_PATH="${ROOT}/internal/runtime/vllm/testdata/vllm_v1_queued.txt"
BUSYBOX_IMAGE="busybox-fixture:1.36"
docker build -q -t "${BUSYBOX_IMAGE}" - <<'DOCKERFILE'
FROM busybox:1.36
RUN mkdir -p /www
CMD ["busybox", "httpd", "-f", "-p", "9090", "-h", "/www"]
DOCKERFILE
kind load docker-image "${BUSYBOX_IMAGE}" --name "${KIND_CLUSTER}"

kubectl create configmap vllm-fixture \
    --from-file=metrics="${FIXTURE_PATH}" \
    --namespace "${FIXTURE_NAMESPACE}"
kubectl apply -n "${FIXTURE_NAMESPACE}" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-fixture
spec:
  replicas: 1
  selector:
    matchLabels: {app: vllm-fixture}
  template:
    metadata:
      labels: {app: vllm-fixture}
    spec:
      containers:
        - name: httpd
          image: busybox-fixture:1.36
          imagePullPolicy: Never
          volumeMounts:
            - name: data
              mountPath: /www
      volumes:
        - name: data
          configMap:
            name: vllm-fixture
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-fixture
spec:
  selector: {app: vllm-fixture}
  ports:
    - port: 9090
      targetPort: 9090
YAML

# ── Kubernetes Secret with the test token ─────────────────────────────────────
echo "==> Creating API token Secret"
kubectl create secret generic ifa-api-auth \
    --namespace "${IFA_NAMESPACE}" \
    --from-literal=token="${TEST_TOKEN}"

# ── IFA via Helm with auth enabled ────────────────────────────────────────────
echo "==> Installing IFA with api_auth.enabled=true"
helm install ifa "${CHART_DIR}" \
    --namespace "${IFA_NAMESPACE}" \
    --set image.repository=ifa \
    --set image.tag=integration-test \
    --set image.pullPolicy=Never \
    --set config.cluster_name=kind-auth-test \
    --set config.logging.level=debug \
    --set config.logging.format=text \
    --set config.collector.interval=5s \
    --set config.collector.timeout=4s \
    --set "config.collector.targets[0].name=vllm-fixture" \
    --set "config.collector.targets[0].namespace=inference" \
    --set "config.collector.targets[0].runtime=vllm" \
    --set "config.collector.targets[0].model_name=facebook/opt-125m" \
    --set "config.collector.targets[0].metrics_url=http://vllm-fixture.inference.svc:9090/metrics" \
    --set config.kubernetes.enabled=true \
    --set config.kubernetes.namespace=inference \
    --set "config.api_auth.enabled=true" \
    --set "config.api_auth.token=" \
    --set "apiAuth.existingSecret=ifa-api-auth" \
    --set "apiAuth.secretKey=token" \
    --set rbac.scope=namespace \
    --timeout 120s \
    --wait

# ── Port-forward ──────────────────────────────────────────────────────────────
echo "==> Port-forwarding IFA"
kubectl port-forward --namespace "${IFA_NAMESPACE}" \
    "svc/ifa" "${LOCAL_PORT}:8080" &
PF_PID=$!
sleep 3

# ── Helper ───────────────────────────────────────────────────────────────────
poll() {
    local desc="$1" path="$2" token="$3" want_status="$4"
    local max_wait=60 interval=3 elapsed=0
    while true; do
        local http_code
        if [[ -n "${token}" ]]; then
            http_code=$(curl -s -o /dev/null -w "%{http_code}" \
                -H "Authorization: Bearer ${token}" \
                "${IFA_API}${path}" 2>/dev/null || echo "000")
        else
            http_code=$(curl -s -o /dev/null -w "%{http_code}" \
                "${IFA_API}${path}" 2>/dev/null || echo "000")
        fi
        if [[ "${http_code}" == "${want_status}" ]]; then
            echo "  PASS  ${desc}: ${path} returned ${http_code}"
            return 0
        fi
        elapsed=$((elapsed + interval))
        if [[ ${elapsed} -ge ${max_wait} ]]; then
            echo "  FAIL  ${desc}: ${path} returned ${http_code}, want ${want_status}" >&2
            return 1
        fi
        sleep "${interval}"
    done
}

FAIL=0

# ── 1. Health probes unauthenticated ─────────────────────────────────────────
echo ""
echo "==> 1. Health probes must be reachable without credentials"
poll "liveness probe (no token)"  "/api/v1/healthz" "" "200" || FAIL=1
poll "readiness probe (no token)" "/api/v1/readyz"  "" "200" || FAIL=1
poll "legacy healthz (no token)"  "/healthz"        "" "200" || FAIL=1

# ── 2. Data endpoints reject unauthenticated requests ─────────────────────────
echo ""
echo "==> 2. Data endpoints must reject unauthenticated requests (401)"
for path in /api/v1/telemetry /api/v1/recommendations /api/v1/workloads /api/v1/rules \
            /telemetry /recommendations /workloads; do
    poll "no-token ${path}" "${path}" "" "401" || FAIL=1
done

# ── 3. Data endpoints accept correct token ────────────────────────────────────
echo ""
echo "==> 3. Data endpoints must accept correct token (200)"
for path in /api/v1/telemetry /api/v1/workloads /api/v1/rules; do
    poll "with-token ${path}" "${path}" "${TEST_TOKEN}" "200" || FAIL=1
done

# ── 4. Wrong token is rejected ────────────────────────────────────────────────
echo ""
echo "==> 4. Wrong token must be rejected (401)"
WRONG="definitely-the-wrong-token"
http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${WRONG}" \
    "${IFA_API}/api/v1/telemetry" 2>/dev/null || echo "000")
if [[ "${http_code}" == "401" ]]; then
    echo "  PASS  wrong token: /api/v1/telemetry returned 401"
else
    echo "  FAIL  wrong token: /api/v1/telemetry returned ${http_code}, want 401" >&2
    FAIL=1
fi

# ── 5. Token not in ConfigMap ─────────────────────────────────────────────────
echo ""
echo "==> 5. Token must not appear in the rendered ConfigMap"
cm=$(kubectl get configmap -n "${IFA_NAMESPACE}" ifa -o json 2>/dev/null || echo "{}")
if echo "${cm}" | grep -q "${TEST_TOKEN}"; then
    echo "  FAIL  token found in ConfigMap" >&2
    FAIL=1
else
    echo "  PASS  token absent from ConfigMap"
fi

# ── 6. /metrics unauthenticated ───────────────────────────────────────────────
echo ""
echo "==> 6. /metrics must be reachable without credentials"
poll "metrics (no token)" "/metrics" "" "200" || FAIL=1

# ── Result ────────────────────────────────────────────────────────────────────
echo ""
if [[ ${FAIL} -ne 0 ]]; then
    echo "==> IFA pod logs (last 80 lines):"
    kubectl logs --namespace "${IFA_NAMESPACE}" \
        -l app.kubernetes.io/name=ifa --tail=80 2>/dev/null || true
    echo "==> Auth integration test FAILED" >&2
    exit 1
fi
echo "==> Auth integration test PASSED"
