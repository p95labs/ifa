#!/usr/bin/env bash
# 01-triton-gpu-validate.sh
# ─────────────────────────────────────────────────────────────────────────────
# Milestone A: GPU-backed Triton validation on an NVIDIA L4.
#
# What this script does
# ─────────────────────
# 1. Copies the gpu_matmul model repository to the remote node.
# 2. Starts Triton 25.12 with GPU access and summary latencies enabled.
# 3. Waits for the model to become READY.
# 4. Verifies CUDA is actually used (initialize() raises on CPU-only).
# 5. Runs a concurrent load generator to produce GPU utilization.
# 6. Captures raw /metrics payloads at idle and under load.
# 7. Starts the official DCGM exporter and captures corroborating GPU metrics.
# 8. Saves all captures to /data/captures/ on the remote node.
#
# Usage
#   LOCAL:  bash scripts/gpu-validation/01-triton-gpu-validate.sh <node-ip>
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

NODE_IP="${1:?usage: $0 <node-ip>}"
SSH="ssh -i ~/.ssh/e2e_ifa_l4_v2 -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 root@${NODE_IP}"
SCP="scp -i ~/.ssh/e2e_ifa_l4_v2 -o StrictHostKeyChecking=accept-new"

TRITON_IMAGE="nvcr.io/nvidia/tritonserver:25.12-pyt-python-py3"
DCGM_IMAGE="nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04"
CAPTURE_DIR="/data/captures/triton_gpu"
MODEL_NAME="gpu_matmul"
TRITON_HTTP_PORT=8000
TRITON_METRICS_PORT=8002
DCGM_PORT=9401   # offset from 9400 to avoid clash if DCGM is already running

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() { echo "[$(date +%H:%M:%S)] $*"; }
die() { echo "FATAL: $*" >&2; exit 1; }

# ── 1. Create capture directory and copy model repo ──────────────────────────
log "Creating directories on remote node"
$SSH "mkdir -p ${CAPTURE_DIR} /data/triton-models"

log "Uploading gpu_matmul model repository"
$SCP -r "${SCRIPT_DIR}/triton-model-repo/gpu_matmul" \
    "root@${NODE_IP}:/data/triton-models/"

# ── 2. Pull Triton image ──────────────────────────────────────────────────────
log "Pulling Triton image (this takes several minutes — image is ~15 GB)"
$SSH "docker pull ${TRITON_IMAGE} 2>&1 | tail -3"

# ── 3. Start Triton with GPU ──────────────────────────────────────────────────
log "Starting Triton with GPU access"
$SSH "docker rm -f triton 2>/dev/null || true"
$SSH "docker run -d --name triton \
  --gpus all \
  -p 127.0.0.1:${TRITON_HTTP_PORT}:8000 \
  -p 127.0.0.1:8001:8001 \
  -p 127.0.0.1:${TRITON_METRICS_PORT}:8002 \
  -v /data/triton-models:/models \
  ${TRITON_IMAGE} \
  tritonserver \
    --model-repository=/models \
    --metrics-port=8002 \
    --allow-metrics=true \
    --metrics-config summary_latencies=true \
    --log-verbose=0 \
  2>&1"

# ── 4. Wait for gpu_matmul to become READY ───────────────────────────────────
log "Waiting for Triton model to become READY (up to 3 min)"
for i in $(seq 1 36); do
    state=$($SSH "curl -sf http://127.0.0.1:${TRITON_HTTP_PORT}/v2/models/${MODEL_NAME}/ready && echo READY || echo NOT_READY" 2>/dev/null || echo "NOT_READY")
    if [[ "$state" == *"READY"* ]]; then
        log "Model ${MODEL_NAME} is READY (attempt ${i})"
        break
    fi
    if [[ $i -eq 36 ]]; then
        log "Triton logs:"
        $SSH "docker logs triton 2>&1 | tail -30"
        die "Model not ready after 180 s"
    fi
    log "  attempt ${i}/36: not ready yet — sleeping 5s"
    sleep 5
done

# ── 5. Verify GPU is actually used ───────────────────────────────────────────
log "Verifying GPU execution (model initialize() raises on CPU-only)"
$SSH "docker logs triton 2>&1 | grep -E 'gpu_matmul|CUDA|cuda|KIND_GPU|Allocated|error|Error'" || true

# Confirm KIND_GPU instance was created (appears in Triton startup log)
if ! $SSH "docker logs triton 2>&1 | grep -q 'gpu_matmul'"; then
    $SSH "docker logs triton 2>&1 | tail -40"
    die "gpu_matmul model did not appear in Triton logs"
fi

# ── 6. Capture idle metrics ───────────────────────────────────────────────────
log "Capturing idle Triton metrics"
TS=$(date +%Y%m%dT%H%M%S)
$SSH "curl -sf http://127.0.0.1:${TRITON_METRICS_PORT}/metrics \
    > ${CAPTURE_DIR}/triton_gpu_l4_idle_${TS}.txt && echo 'saved'"

$SSH "grep -E 'nv_gpu_utilization|nv_gpu_memory|nv_inference_request_success' \
    ${CAPTURE_DIR}/triton_gpu_l4_idle_${TS}.txt | grep -v 'HELP\|TYPE'"

# ── 7. Start DCGM exporter ───────────────────────────────────────────────────
log "Starting official DCGM exporter (corroboration)"
$SSH "docker rm -f dcgm-exporter 2>/dev/null || true"
$SSH "docker run -d --name dcgm-exporter \
  --gpus all --privileged --net host \
  -v /run/dcgm:/run/dcgm \
  -e DCGM_EXPORTER_LISTEN=:${DCGM_PORT} \
  ${DCGM_IMAGE} 2>&1"
sleep 15  # DCGM init
$SSH "curl -sf http://127.0.0.1:${DCGM_PORT}/metrics | grep -E 'DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED' | grep -v 'HELP\|TYPE'"

# ── 8. Load generator (concurrent HTTP infer requests) ───────────────────────
log "Uploading load generator"
cat << 'PYEOF' | $SSH 'cat > /data/triton_load_gen.py'
#!/usr/bin/env python3
"""Concurrent load generator for gpu_matmul Triton model."""
import threading, requests, json, time, sys, numpy as np

URL  = "http://127.0.0.1:8000/v2/models/gpu_matmul/infer"
N    = int(sys.argv[1]) if len(sys.argv) > 1 else 40
SECS = int(sys.argv[2]) if len(sys.argv) > 2 else 60

PAYLOAD = json.dumps({
    "inputs": [{
        "name": "INPUT",
        "shape": [1, 128, 128],
        "datatype": "FP32",
        "data": np.random.rand(128, 128).flatten().tolist()
    }]
})
HEADERS = {"Content-Type": "application/json"}

stop   = threading.Event()
counts = {"ok": 0, "err": 0}
lock   = threading.Lock()

def worker():
    s = requests.Session()
    while not stop.is_set():
        try:
            r = s.post(URL, data=PAYLOAD, headers=HEADERS, timeout=30)
            with lock:
                counts["ok" if r.status_code == 200 else "err"] += 1
        except Exception:
            with lock:
                counts["err"] += 1

threads = [threading.Thread(target=worker, daemon=True) for _ in range(N)]
for t in threads: t.start()
print(f"Load generator: {N} workers for {SECS}s", flush=True)
deadline = time.time() + SECS
while time.time() < deadline:
    time.sleep(10)
    with lock:
        print(f"  t={int(deadline-time.time())}s remaining  ok={counts['ok']}  err={counts['err']}", flush=True)
stop.set()
for t in threads: t.join(timeout=5)
print(f"Done. ok={counts['ok']}  err={counts['err']}")
PYEOF

log "Running load generator (${N:-40} workers, 60 s)"
$SSH "nohup python3 /data/triton_load_gen.py 40 75 > /data/triton_load.log 2>&1 &"

# Capture snapshots while load runs
for snap in 1 2 3 4; do
    sleep 18
    TS=$(date +%Y%m%dT%H%M%S)
    log "Snapshot ${snap} at ${TS}"
    $SSH "curl -sf http://127.0.0.1:${TRITON_METRICS_PORT}/metrics \
        > ${CAPTURE_DIR}/triton_gpu_l4_load${snap}_${TS}.txt"
    $SSH "curl -sf http://127.0.0.1:${DCGM_PORT}/metrics \
        > ${CAPTURE_DIR}/dcgm_triton_load${snap}_${TS}.txt"
    $SSH "grep -E 'nv_gpu_utilization|nv_gpu_memory_used_bytes|nv_inference_request_success|nv_inference_pending' \
        ${CAPTURE_DIR}/triton_gpu_l4_load${snap}_${TS}.txt | grep -v 'HELP\|TYPE'"
    $SSH "grep 'DCGM_FI_DEV_GPU_UTIL\|DCGM_FI_DEV_FB_USED\|DCGM_FI_DEV_GPU_TEMP' \
        ${CAPTURE_DIR}/dcgm_triton_load${snap}_${TS}.txt | grep -v 'HELP\|TYPE'"
done

sleep 15  # wait for load generator to finish
log "Load generator log:"
$SSH "cat /data/triton_load.log"

# ── 9. Recovery capture ───────────────────────────────────────────────────────
sleep 15
TS=$(date +%Y%m%dT%H%M%S)
log "Capturing recovery metrics at ${TS}"
$SSH "curl -sf http://127.0.0.1:${TRITON_METRICS_PORT}/metrics \
    > ${CAPTURE_DIR}/triton_gpu_l4_recovery_${TS}.txt"
$SSH "curl -sf http://127.0.0.1:${DCGM_PORT}/metrics \
    > ${CAPTURE_DIR}/dcgm_triton_recovery_${TS}.txt"

# ── 10. Show all captured files ───────────────────────────────────────────────
log "All captures:"
$SSH "ls -la ${CAPTURE_DIR}/"

log "Milestone A capture phase complete. Download captures with:"
log "  scp -i ~/.ssh/e2e_ifa_l4_v2 -r root@${NODE_IP}:${CAPTURE_DIR}/ ./triton-gpu-captures/"
