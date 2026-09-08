#!/usr/bin/env bash
#
# kind-integration.sh — end-to-end integration test for Inference Fabric
# IFA using a local kind cluster.
#
# The test validates the full path from a Kubernetes-discovered inference
# workload through in-cluster metrics scraping to the recommendations API,
# without running any real GPU workload or vLLM binary.
#
# Fixture source
# ──────────────
# internal/runtime/vllm/testdata/vllm_v1_queued.txt — a verbatim capture from
# a real vLLM 0.28.0 CPU server with --max-num-seqs 2, showing 12 requests
# waiting for capacity and TTFT p95 ≈ 32 s.  A busybox httpd container serves
# it at /metrics so IFA's collector can scrape it over in-cluster DNS.
#
# Expected finding (stock thresholds, no overrides)
# ──────────────────────────────────────────────────
# IFA-LAT-002 "Time to first token dominated by prefill" (CodeTTFTPrefillBound)
#   • TTFT p95 = 32 000 ms > default ttft_p95_ms = 1 000 ms  → fires.
#   • Queue share ≈ 44.53 % < default queue_share_of_ttft_pct = 50 %
#     (condition: share >= threshold → return nil; 44.53 < 50 → does not
#     return nil → fires. IFA-LAT-001 has the opposite guard: share < 50
#     returns nil, so IFA-LAT-001 does NOT fire.)
#   • IFA-LAT-002 declares Supersedes: [IFA-LAT-003], suppressing the
#     generic symptom once the specific diagnosis is available.
#   • IFA-LAT-002 sets Window: e.Span() (not Sustained()). With
#     sustain_for=10s the span reaches 10 s after the 3rd cycle (~15 s),
#     so window_seconds >= 10 is the effective multi-cycle gate.
#
# Suppressed finding
# ──────────────────
# IFA-LAT-003 "End-to-end p95 latency above target" — suppressed by IFA-LAT-002.
#   Asserting its absence validates the suppression relationship end-to-end.
#
# Prerequisites (all present on GitHub-hosted ubuntu-latest runners):
#   kind, kubectl, helm, docker, curl, jq
#
# Usage:
#   # CI: image already built and tagged ifa:integration-test
#   IFA_IMAGE=ifa:integration-test bash scripts/kind-integration.sh
#
#   # Local: build first, then run
#   docker build -t ifa:local -f deploy/docker/Dockerfile .
#   IFA_IMAGE=ifa:local bash scripts/kind-integration.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# ── Configuration ─────────────────────────────────────────────────────────────
KIND_CLUSTER="${KIND_CLUSTER:-ifa-integration}"
# Pinned by both tag and digest for build reproducibility.
# kind v0.24.0 release notes: https://github.com/kubernetes-sigs/kind/releases/tag/v0.24.0
# Matches the project's k8s.io/client-go v0.31.0 (Kubernetes 1.31) dependency.
KIND_NODE_IMAGE="kindest/node:v1.31.0@sha256:53df588e04085fd41ae12de0c3fe4c72f7013bba32a20e7325357a1ac94ba865"
IFA_IMAGE="${IFA_IMAGE:-ifa:integration-test}"
# busybox-fixture:1.36 is built locally as a single-platform image so that
# `kind load docker-image` (which uses ctr --all-platforms) does not fail
# when Docker's image store holds a multi-arch manifest list whose non-native
# arch blobs are absent (common with Docker Desktop on Apple Silicon).
BUSYBOX_IMAGE="busybox-fixture:1.36"
IFA_NAMESPACE="ifa"
FIXTURE_NAMESPACE="inference"
LOCAL_PORT="${IFA_LOCAL_PORT:-18080}"
FIXTURE_PATH="${ROOT}/internal/runtime/vllm/testdata/vllm_v1_queued.txt"
VALUES_FILE="${ROOT}/test/integration/kind-values.yaml"
CHART_DIR="${ROOT}/deploy/helm/ifa"
IFA_API="http://localhost:${LOCAL_PORT}"

PF_PID=""

# ── Cleanup ───────────────────────────────────────────────────────────────────
cleanup() {
    local rc=$?
    if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
    echo "==> Deleting kind cluster ${KIND_CLUSTER}"
    kind delete cluster --name "${KIND_CLUSTER}" 2>/dev/null || true
    if [[ ${rc} -ne 0 ]]; then
        echo "==> Integration test FAILED (exit ${rc})" >&2
    fi
}
trap cleanup EXIT INT TERM

# ── Prerequisites ─────────────────────────────────────────────────────────────
echo "==> Checking prerequisites"
for cmd in kind kubectl helm docker curl jq; do
    if ! command -v "${cmd}" > /dev/null 2>&1; then
        echo "ERROR: ${cmd} is required but not found in PATH" >&2
        exit 1
    fi
done

if [[ ! -f "${FIXTURE_PATH}" ]]; then
    echo "ERROR: fixture not found: ${FIXTURE_PATH}" >&2
    exit 1
fi
if [[ ! -f "${VALUES_FILE}" ]]; then
    echo "ERROR: Helm values file not found: ${VALUES_FILE}" >&2
    exit 1
fi
if [[ ! -d "${CHART_DIR}" ]]; then
    echo "ERROR: Helm chart directory not found: ${CHART_DIR}" >&2
    exit 1
fi

# ── Kind cluster ──────────────────────────────────────────────────────────────
echo "==> Creating kind cluster ${KIND_CLUSTER}"
if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"; then
    echo "    cluster already exists — deleting it first"
    kind delete cluster --name "${KIND_CLUSTER}"
fi
kind create cluster --name "${KIND_CLUSTER}" --image "${KIND_NODE_IMAGE}" --wait 120s

# ── Load images ───────────────────────────────────────────────────────────────
echo "==> Loading images into kind"
echo "    IFA: ${IFA_IMAGE}"
kind load docker-image "${IFA_IMAGE}" --name "${KIND_CLUSTER}"

echo "    fixture server: ${BUSYBOX_IMAGE}"
# Build a single-platform image using docker buildx --load so that
# kind load docker-image (which uses ctr images import --all-platforms)
# does not fail when Docker's store holds a multi-arch manifest list whose
# non-native blobs are absent (a common situation on Apple Silicon).
# --platform is intentionally omitted: buildx naturally targets the host
# architecture (arm64 on Apple Silicon, amd64 on GitHub-hosted runners),
# producing a single-arch image without pinning a specific platform.
printf 'FROM busybox:1.36\n' \
    | docker buildx build --tag "${BUSYBOX_IMAGE}" --load -
kind load docker-image "${BUSYBOX_IMAGE}" --name "${KIND_CLUSTER}"

# ── Namespaces ────────────────────────────────────────────────────────────────
echo "==> Creating namespaces"
kubectl create namespace "${FIXTURE_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace "${IFA_NAMESPACE}"     --dry-run=client -o yaml | kubectl apply -f -

# ── Fixture ConfigMap ─────────────────────────────────────────────────────────
echo "==> Creating vllm-fixture ConfigMap from ${FIXTURE_PATH}"
# The key is named 'metrics' so that busybox httpd serves it at GET /metrics.
kubectl create configmap vllm-fixture \
    --namespace "${FIXTURE_NAMESPACE}" \
    --from-file=metrics="${FIXTURE_PATH}" \
    --dry-run=client -o yaml \
    | kubectl apply -f -

# ── Fixture HTTP server ───────────────────────────────────────────────────────
# A single Deployment serves two roles:
#   1. Runs busybox httpd, exposing the queued fixture at /metrics over HTTP.
#   2. Carries inference.io/* labels so the Kubernetes watcher discovers it.
# This exercises both the collector (scraping) and the watcher (discovery)
# against the same object, matching production topology.
echo "==> Deploying vllm-fixture server (namespace: ${FIXTURE_NAMESPACE})"
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-fixture
  namespace: ${FIXTURE_NAMESPACE}
  labels:
    app: vllm-fixture
    inference.io/runtime: vllm
  annotations:
    inference.io/model: facebook/opt-125m
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vllm-fixture
  template:
    metadata:
      labels:
        app: vllm-fixture
        inference.io/runtime: vllm
    spec:
      containers:
        - name: httpd
          image: ${BUSYBOX_IMAGE}
          imagePullPolicy: Never
          command: ["/bin/sh", "-c"]
          args:
            - |
              mkdir -p /www
              cp /fixtures/metrics /www/metrics
              exec httpd -f -p 9090 -h /www
          ports:
            - containerPort: 9090
          volumeMounts:
            - name: fixture
              mountPath: /fixtures
              readOnly: true
      volumes:
        - name: fixture
          configMap:
            name: vllm-fixture
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-fixture
  namespace: ${FIXTURE_NAMESPACE}
spec:
  selector:
    app: vllm-fixture
  ports:
    - port: 9090
      targetPort: 9090
EOF

echo "==> Waiting for vllm-fixture to be ready"
kubectl rollout status deployment/vllm-fixture \
    --namespace "${FIXTURE_NAMESPACE}" \
    --timeout=90s

# ── IFA Helm installation ─────────────────────────────────────────────────────
echo "==> Installing IFA via Helm (namespace: ${IFA_NAMESPACE})"
helm install ifa "${CHART_DIR}" \
    --namespace "${IFA_NAMESPACE}" \
    --values "${VALUES_FILE}" \
    --wait \
    --timeout 120s

echo "==> Waiting for IFA Deployment rollout"
kubectl rollout status deployment/ifa \
    --namespace "${IFA_NAMESPACE}" \
    --timeout=120s

# ── Port-forward ──────────────────────────────────────────────────────────────
echo "==> Starting port-forward → ${IFA_API}"
kubectl port-forward \
    --namespace "${IFA_NAMESPACE}" \
    svc/ifa "${LOCAL_PORT}:8080" &
PF_PID=$!
# Give the port-forward a moment to establish.
sleep 3

# ── Assertion helpers ─────────────────────────────────────────────────────────

# assert_eventually <description> <max_attempts> <delay_sec> <check_command>
# Retries <check_command> up to <max_attempts> times, sleeping <delay_sec>
# between tries.  Prints diagnostics and exits 1 on timeout.
assert_eventually() {
    local desc="$1"
    local max="$2"
    local delay="$3"
    shift 3
    local i
    for i in $(seq 1 "${max}"); do
        if "$@" > /dev/null 2>&1; then
            return 0
        fi
        echo "    [${i}/${max}] ${desc} — not satisfied yet, retrying in ${delay}s"
        sleep "${delay}"
    done
    echo "ERROR: ${desc} did not become true after ${max} attempts" >&2
    return 1
}

# ── Assert 1: API readiness ───────────────────────────────────────────────────
echo "==> [1/6] Asserting IFA API readiness"
if ! assert_eventually "GET /api/v1/readyz returns 200" 30 2 \
        curl -fsS "${IFA_API}/api/v1/readyz"; then
    echo "--- IFA pod logs ---" >&2
    kubectl logs --namespace "${IFA_NAMESPACE}" \
        -l app.kubernetes.io/name=ifa --tail=80 >&2 || true
    exit 1
fi
echo "    PASS: IFA API is ready"

# ── Assert 2: Kubernetes workload discovery ───────────────────────────────────
echo "==> [2/6] Asserting Kubernetes workload discovery"
check_workload_discovered() {
    local count
    count=$(curl -fsS "${IFA_API}/api/v1/workloads" | jq '.count // 0')
    [[ "${count}" -ge 1 ]]
}
if ! assert_eventually "vllm-fixture workload discovered" 20 2 \
        check_workload_discovered; then
    echo "--- workloads response ---" >&2
    curl -fsS "${IFA_API}/api/v1/workloads" | jq . >&2 || true
    exit 1
fi
echo "    PASS: workload discovered (namespace=inference, name=vllm-fixture)"

# ── Assert 3: Metrics scraping ────────────────────────────────────────────────
echo "==> [3/6] Asserting fixture metrics are scraped (requests_waiting=12)"
check_requests_waiting() {
    local waiting
    waiting=$(curl -fsS "${IFA_API}/api/v1/telemetry" \
        | jq '[.items[] | select(.namespace == "inference") | .requests_waiting // 0] | max // 0')
    # jq returns 12 or 12.0; compare numerically via integer truncation
    local int_waiting
    int_waiting=$(printf "%.0f" "${waiting}" 2>/dev/null) || return 1
    [[ "${int_waiting}" -eq 12 ]]
}
if ! assert_eventually "requests_waiting == 12 in telemetry" 20 3 \
        check_requests_waiting; then
    echo "--- telemetry response (inference namespace) ---" >&2
    curl -fsS "${IFA_API}/api/v1/telemetry" \
        | jq '[.items[] | select(.namespace == "inference")]' >&2 || true
    echo "--- IFA pod logs (scrape errors) ---" >&2
    kubectl logs --namespace "${IFA_NAMESPACE}" \
        -l app.kubernetes.io/name=ifa --tail=40 >&2 || true
    exit 1
fi
echo "    PASS: requests_waiting=12 confirmed from queued fixture"

# ── Assert 4: Prometheus self-metrics ────────────────────────────────────────
echo "==> [4/6] Asserting IFA Prometheus self-metrics endpoint"
if ! curl -fsS "${IFA_API}/metrics" | grep -q "^ifa_scrapes_total"; then
    echo "ERROR: /metrics does not expose ifa_scrapes_total" >&2
    curl -fsS "${IFA_API}/metrics" | head -30 >&2 || true
    exit 1
fi
echo "    PASS: ifa_scrapes_total present"

# ── Assert 5: IFA-LAT-002 present with exact TTFT evidence ───────────────────
echo "==> [5/6] Asserting IFA-LAT-002 with exact TTFT evidence and window>=10s"
#
# IFA-LAT-002 fires under stock thresholds:
#   TTFT p95 = 32 000 ms > default ttft_p95_ms = 1 000 ms.
#   Queue share ≈ 44.53 % < default queue_share_of_ttft_pct = 50 %.
#
# TTFT p95 derivation (deterministic from committed fixture):
#   vllm:time_to_first_token_seconds histogram, count=8, q=0.95:
#   rank = 0.95 × 8 = 7.6; falls in [le=20.0 (n=7), le=40.0 (n=8)].
#   value = 20 + (40-20) × (7.6-7)/1 = 32.0 s × 1000 = 32 000 ms.
#   Float64 gives 31 999.999...ms; assertion uses ε = 1 ms.
#
# IFA-LAT-002 uses e.Span() (not Sustained()). Span reaches 10 s after
# the 3rd collection cycle, so window_seconds >= 10 proves the window-
# tracking code path and that ≥ 3 real cycles elapsed.
#
# Evidence fields are located by name (not index) to guard against
# reordering.
#
check_rec_lat002() {
    local resp
    resp=$(curl -fsS "${IFA_API}/api/v1/recommendations") || return 1
    # jq -e exits 1 when the final expression is false or null.
    # Locate the ttft_p95_ms evidence item by metric name, then check:
    #   observed ≈ 32 000 ms (|observed - 32000| < 1, float64 epsilon)
    #   threshold == 1000    (stock default; confirms no override was applied)
    #   window_seconds >= 10 (≥ 3 cycles of 5s each with sustain_for=10s)
    echo "${resp}" | jq -e '
        .items
        | map(select(.code == "IFA-LAT-002"))
        | .[0] // null
        | . != null
          and (.window_seconds >= 10)
          and (
            (.evidence | map(select(.metric == "ttft_p95_ms")) | .[0] // null)
            | . != null
              and ((.observed - 32000 | fabs) < 1)
              and (.threshold == 1000)
              and (.comparison == ">")
          )
    ' > /dev/null 2>&1
}
if ! assert_eventually \
        "IFA-LAT-002 with ttft_p95_ms evidence and window_seconds>=10" 30 3 \
        check_rec_lat002; then
    echo "--- full recommendations response ---" >&2
    curl -fsS "${IFA_API}/api/v1/recommendations" | jq . >&2 || true
    echo "--- telemetry (TTFT / queue share) ---" >&2
    curl -fsS "${IFA_API}/api/v1/telemetry" \
        | jq '[.items[] | select(.namespace == "inference") | {workload_name, ttft_p95_ms, queue_time_p95_ms, requests_waiting}]' >&2 || true
    echo "--- IFA pod logs ---" >&2
    kubectl logs --namespace "${IFA_NAMESPACE}" \
        -l app.kubernetes.io/name=ifa --tail=80 >&2 || true
    exit 1
fi
echo "    PASS: IFA-LAT-002 confirmed — ttft_p95_ms≈32000ms, threshold=1000, window_seconds>=10"
curl -fsS "${IFA_API}/api/v1/recommendations" \
    | jq '.items[] | select(.code == "IFA-LAT-002") | {code, severity, window_seconds, evidence}' \
    2>/dev/null || true

# ── Assert 6: IFA-LAT-003 absent (suppressed by IFA-LAT-002) ─────────────────
echo "==> [6/6] Asserting IFA-LAT-003 absent (suppressed by IFA-LAT-002)"
#
# IFA-LAT-002 declares Supersedes: [IFA-LAT-003] (CodeE2ELatencyHigh).
# When IFA-LAT-002 fires it must prevent IFA-LAT-003 from appearing in
# the output. Asserting absence validates this product behavior end-to-end.
# If both codes appear simultaneously the suppression logic has regressed.
#
check_rec_absent() {
    local code="$1"
    local resp
    resp=$(curl -fsS "${IFA_API}/api/v1/recommendations") || return 1
    echo "${resp}" | jq -e --arg code "${code}" \
        '[.items[] | select(.code == $code)] | length == 0' > /dev/null 2>&1
}
if ! check_rec_absent "IFA-LAT-003"; then
    echo "ERROR: IFA-LAT-003 is present but should be suppressed by IFA-LAT-002" >&2
    curl -fsS "${IFA_API}/api/v1/recommendations" | jq . >&2 || true
    exit 1
fi
echo "    PASS: IFA-LAT-003 absent — suppression by IFA-LAT-002 confirmed"

echo
echo "════════════════════════════════════════════════════════════════════════"
echo " All 6 assertions passed — IFA kind integration test succeeded."
echo
echo " Cluster:    ${KIND_CLUSTER}"
echo " Workload:   vllm-fixture (namespace: inference)"
echo " Fixture:    vllm_v1_queued.txt (requests_waiting=12, ttft_p95=32000ms)"
echo " Finding:    IFA-LAT-002 — TTFT dominated by prefill (stock thresholds)"
echo " Suppressed: IFA-LAT-003 — absent as expected"
echo "════════════════════════════════════════════════════════════════════════"
