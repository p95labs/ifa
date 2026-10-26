#!/usr/bin/env bash
# 02-k3s-setup.sh
# ─────────────────────────────────────────────────────────────────────────────
# Installs K3s with NVIDIA GPU support on the E2E Networks L4 node.
#
# Steps
# ─────
# 1. Verifies NVIDIA driver and Docker NVIDIA runtime are present.
# 2. Configures the NVIDIA container toolkit for containerd (K3s's runtime).
# 3. Installs K3s with the NVIDIA container runtime.
# 4. Deploys the NVIDIA device plugin DaemonSet.
# 5. Verifies GPU resource is advertised (nvidia.com/gpu: 1).
# 6. Runs a test pod that executes nvidia-smi to confirm GPU passthrough.
#
# Usage
#   bash scripts/gpu-validation/02-k3s-setup.sh <node-ip>
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

NODE_IP="${1:?usage: $0 <node-ip>}"
SSH="ssh -i ~/.ssh/e2e_ifa_l4_v2 -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 root@${NODE_IP}"

log() { echo "[$(date +%H:%M:%S)] $*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

# ── 1. Pre-flight checks ──────────────────────────────────────────────────────
log "Checking GPU driver and NVIDIA toolkit"
$SSH "nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader"
$SSH "docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi -L"

# ── 2. Configure containerd for NVIDIA (K3s uses its own containerd) ──────────
log "Configuring NVIDIA container toolkit for K3s containerd"
$SSH "nvidia-ctk runtime configure --runtime=containerd \
    --config=/var/lib/rancher/k3s/agent/etc/containerd/config.toml 2>&1 || true"

# K3s installs its own containerd; we need to pre-create the config directory
# and add the NVIDIA runtime so K3s picks it up at start time.
$SSH 'mkdir -p /var/lib/rancher/k3s/agent/etc/containerd'
$SSH 'cat > /etc/rancher/k3s/registries.yaml << '"'"'EOF'"'"'
mirrors: {}
EOF
mkdir -p /etc/rancher/k3s'

# Write a K3s containerd config snippet that adds the NVIDIA runtime
$SSH 'cat > /var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl << '"'"'TMPL'"'"'
[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "nvidia"

  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
    runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
    SystemdCgroup = true

  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.nvidia]
    privileged_without_host_devices = false
    runtime_engine = ""
    runtime_root = ""
    runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.nvidia.options]
    BinaryName = "/usr/bin/nvidia-container-runtime"
    SystemdCgroup = true
TMPL'

# ── 3. Install K3s ────────────────────────────────────────────────────────────
log "Installing K3s (this takes ~1 min)"
$SSH 'curl -sfL https://get.k3s.io | \
    INSTALL_K3S_EXEC="server --disable=traefik --write-kubeconfig-mode=644" \
    sh -'

# Wait for K3s to start
log "Waiting for K3s to become ready (up to 2 min)"
for i in $(seq 1 24); do
    if $SSH "kubectl get nodes 2>/dev/null | grep -q Ready"; then
        log "K3s node is Ready (attempt ${i})"
        break
    fi
    [[ $i -eq 24 ]] && die "K3s node not ready after 2 min"
    log "  attempt ${i}/24 — sleeping 5s"
    sleep 5
done

$SSH "kubectl get nodes -o wide"

# ── 4. Install NVIDIA device plugin ──────────────────────────────────────────
log "Deploying NVIDIA device plugin DaemonSet"
$SSH 'kubectl create -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.0/deployments/static/nvidia-device-plugin.yml 2>/dev/null || \
    kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.0/deployments/static/nvidia-device-plugin.yml'

# ── 5. Wait for GPU resource to be advertised ─────────────────────────────────
log "Waiting for nvidia.com/gpu resource (up to 3 min)"
for i in $(seq 1 36); do
    gpus=$($SSH "kubectl get node -o jsonpath='{.items[0].status.allocatable.nvidia\.com/gpu}' 2>/dev/null || echo 0")
    if [[ "$gpus" == "1" ]]; then
        log "GPU resource advertised: nvidia.com/gpu=1"
        break
    fi
    [[ $i -eq 36 ]] && die "GPU not advertised after 3 min; check device plugin logs"
    log "  attempt ${i}/36: allocatable gpu='${gpus}' — sleeping 5s"
    sleep 5
done

$SSH "kubectl describe node | grep -A5 'Allocatable:' | grep -E 'cpu|memory|nvidia'"

# ── 6. Test pod: nvidia-smi in a K8s pod ─────────────────────────────────────
log "Running nvidia-smi test pod inside Kubernetes"
$SSH 'kubectl run gpu-test --rm --restart=Never --image=nvidia/cuda:12.4.1-base-ubuntu22.04 \
    --overrides='"'"'{"spec":{"containers":[{"name":"gpu-test","image":"nvidia/cuda:12.4.1-base-ubuntu22.04","command":["nvidia-smi","-L"],"resources":{"limits":{"nvidia.com/gpu":"1"}}}],"restartPolicy":"Never"}}'"'"' \
    --timeout=120s -- 2>&1'

log "K3s + NVIDIA GPU device plugin setup complete"
log "KUBECONFIG is at /etc/rancher/k3s/k3s.yaml on the node"
log "Next: bash scripts/gpu-validation/03-k8s-deploy.sh ${NODE_IP}"
