#!/usr/bin/env bash
# 03-k8s-deploy.sh
# ─────────────────────────────────────────────────────────────────────────────
# Deploys vLLM (GPU pod), DCGM exporter (DaemonSet), and IFA (Helm) on the
# K3s cluster prepared by 02-k3s-setup.sh.
#
# Usage
#   bash scripts/gpu-validation/03-k8s-deploy.sh <node-ip> <ifa-repo-root>
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

NODE_IP="${1:?usage: $0 <node-ip> <ifa-repo-root>}"
IFA_ROOT="${2:?usage: $0 <node-ip> <ifa-repo-root>}"
SSH="ssh -i ~/.ssh/e2e_ifa_l4_v2 -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 root@${NODE_IP}"
SCP="scp -i ~/.ssh/e2e_ifa_l4_v2 -o StrictHostKeyChecking=accept-new"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K="$SSH kubectl"

log() { echo "[$(date +%H:%M:%S)] $*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

wait_pod_ready() {
    local ns="$1" name="$2" timeout="${3:-300}"
    log "Waiting for pod ${ns}/${name} (up to ${timeout}s)"
    if ! $SSH "kubectl wait pod/${name} -n ${ns} --for=condition=Ready --timeout=${timeout}s 2>&1"; then
        $SSH "kubectl describe pod/${name} -n ${ns} | tail -20"
        $SSH "kubectl logs ${name} -n ${ns} --tail=30 2>&1 || true"
        die "Pod ${ns}/${name} did not become Ready"
    fi
    log "Pod ${ns}/${name} is Ready"
}

# ── 1. Upload manifests ───────────────────────────────────────────────────────
log "Uploading Kubernetes manifests"
$SSH "mkdir -p /data/k8s"
$SCP "${SCRIPT_DIR}/k8s/namespace.yaml"     "root@${NODE_IP}:/data/k8s/"
$SCP "${SCRIPT_DIR}/k8s/vllm.yaml"          "root@${NODE_IP}:/data/k8s/"
$SCP "${SCRIPT_DIR}/k8s/dcgm-exporter.yaml" "root@${NODE_IP}:/data/k8s/"
$SCP "${SCRIPT_DIR}/k8s/ifa-gpu-values.yaml" "root@${NODE_IP}:/data/k8s/"

# ── 2. Create namespaces ──────────────────────────────────────────────────────
log "Creating namespaces"
$K apply -f /data/k8s/namespace.yaml

# ── 3. Pull vLLM image (19 GB — start early) ─────────────────────────────────
log "Pre-pulling vLLM image into K3s containerd (takes several minutes)"
$SSH "k3s ctr images pull docker.io/vllm/vllm-openai:v0.28.0 2>&1 | tail -3" &
PULL_PID=$!

# ── 4. Deploy DCGM exporter ───────────────────────────────────────────────────
log "Deploying DCGM exporter DaemonSet"
$K apply -f /data/k8s/dcgm-exporter.yaml
$K rollout status daemonset/dcgm-exporter -n gpu-operator --timeout=120s

# Verify DCGM is emitting metrics inside the cluster
DCGM_POD=$($K get pods -n gpu-operator -l app=dcgm-exporter -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
log "DCGM exporter pod: ${DCGM_POD}"
$K exec "${DCGM_POD}" -n gpu-operator -- \
    wget -qO- http://localhost:9400/metrics 2>/dev/null | \
    grep -E "DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED" | grep -v "HELP\|TYPE" | head -4 || true

# ── 5. Build IFA image from source on the node ───────────────────────────────
log "Uploading IFA source and building image on node"
# Tar the repo (excluding large dirs) and upload
tar -czf /tmp/ifa-src.tar.gz \
    -C "${IFA_ROOT}" \
    --exclude='.git' \
    --exclude='vendor' \
    --exclude='testdata' \
    . 2>/dev/null

$SCP /tmp/ifa-src.tar.gz "root@${NODE_IP}:/data/"
$SSH "mkdir -p /data/ifa-src && tar -xzf /data/ifa-src.tar.gz -C /data/ifa-src"

# Build Docker image on node then import into K3s containerd
$SSH "cd /data/ifa-src && docker build -t ifa:gpu-validation -f deploy/docker/Dockerfile . 2>&1 | tail -5"
$SSH "docker save ifa:gpu-validation | k3s ctr images import -"

# Update values file to use the locally-built image
$SSH "sed -i 's|repository: localhost/ifa|repository: docker.io/library/ifa|' /data/k8s/ifa-gpu-values.yaml"

# ── 6. Install IFA via Helm ───────────────────────────────────────────────────
log "Installing IFA via Helm into ifa-system namespace"
$SSH "mkdir -p /data/ifa-src && ls /data/ifa-src/deploy/helm/ifa/ || true"
$SSH "helm upgrade --install ifa /data/ifa-src/deploy/helm/ifa \
    --namespace ifa-system --create-namespace \
    --values /data/k8s/ifa-gpu-values.yaml \
    --set image.repository=docker.io/library/ifa \
    --set image.tag=gpu-validation \
    --wait --timeout=120s 2>&1"

$K get pods -n ifa-system
$K get pods -n ifa-system -l 'app.kubernetes.io/name=ifa' -o wide

# ── 7. Wait for vLLM image pull to finish, then deploy ───────────────────────
log "Waiting for vLLM image pre-pull to finish"
wait $PULL_PID || true

log "Deploying vLLM pod"
$K apply -f /data/k8s/vllm.yaml
log "Waiting for vLLM pod to become Ready (model loading can take 3-4 min)"
wait_pod_ready inference vllm 360

# ── 8. Verify scraping is working ─────────────────────────────────────────────
log "Waiting for IFA to complete first scrape cycle (30s)"
sleep 35

IFA_POD=$($K get pods -n ifa-system -l 'app.kubernetes.io/name=ifa' \
    -o jsonpath='{.items[0].metadata.name}')
log "IFA pod: ${IFA_POD}"

# Port-forward IFA API
$SSH "nohup kubectl port-forward -n ifa-system pod/${IFA_POD} 18080:8080 \
    > /data/ifa-pf.log 2>&1 &"
sleep 3

# Test API
$SSH "curl -sf http://localhost:18080/api/v1/healthz"
$SSH "curl -sf http://localhost:18080/api/v1/targets | python3 -m json.tool | head -40"

log "Deployment complete"
log "IFA API is accessible via port-forward on the node at http://localhost:18080"
log "Next: bash scripts/gpu-validation/04-k8s-validate.sh ${NODE_IP}"
