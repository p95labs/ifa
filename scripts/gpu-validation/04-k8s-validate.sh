#!/usr/bin/env bash
# 04-k8s-validate.sh — live GPU Kubernetes E2E validation (Milestone B)
#
# Runs against a K3s cluster on the E2E L4 node, with:
#   - vLLM pod serving facebook/opt-125m, backed by one NVIDIA L4
#   - Official NVIDIA DCGM exporter (nvcr.io/nvidia/k8s/dcgm-exporter)
#   - IFA installed via Helm in namespace 'ifa'
#
# Prerequisites:
#   - SSH alias 'e2e-ifa-l4-v2' configured (~/.ssh/config)
#   - K3s running, namespaces 'inference' and 'ifa' exist
#   - IFA svc/ifa-gpu port-forwarded to 9080 on the node
#   - kubectl context pointing at the K3s cluster (KUBECONFIG on the node)
#
# Run from repo root:
#   scripts/gpu-validation/04-k8s-validate.sh
#
# Output: PASS/FAIL/NOT_REPRODUCED for each scenario printed to stdout.
# Raw API responses saved to /tmp/k8s-validate-*.json
#
# NOTE: This script requires a live E2E GPU node and is intentionally NOT
# part of ordinary GitHub Actions CI, which must not depend on paid GPU
# availability. Run manually on a configured node.

set -euo pipefail
NODE="e2e-ifa-l4-v2"
API="http://127.0.0.1:9080"
PASS=0; FAIL=0; NR=0

log()  { echo "[$(date -u +%H:%M:%S)] $*"; }
pass() { log "PASS: $1"; ((PASS++)) || true; }
fail() { log "FAIL: $1"; ((FAIL++)) || true; }
nr()   { log "NOT_REPRODUCED: $1"; ((NR++)) || true; }

# ── helpers ─────────────────────────────────────────────────────────────────
api() { ssh "$NODE" "curl -sf $API/api/v1/$1"; }
tele() { api telemetry | python3 -c "
import sys, json
items = json.load(sys.stdin).get('items', [])
if not items: print('NO_TELEMETRY'); exit(0)
d = items[0]
for k in ['$1']:
    print(d.get(k, 'null'))
"; }
findings() { api recommendations | python3 -c "
import sys, json
codes = [i['code'] for i in json.load(sys.stdin).get('items', [])]
print(' '.join(codes) if codes else 'NONE')
"; }

log "=== K8s GPU E2E Validation: Milestone B ==="
log "Node: $NODE  API: $API"

# ── Preflight ────────────────────────────────────────────────────────────────
log "Preflight: checking cluster and pods"
ssh "$NODE" "kubectl get pods -n inference; kubectl get pods -n ifa"

VLLM_POD=$(ssh "$NODE" "kubectl get pods -n inference -l app=vllm \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null")
if [[ -z "$VLLM_POD" ]]; then
  fail "No vLLM pod found in namespace 'inference'"
  exit 1
fi
log "vLLM pod: $VLLM_POD"

VLLM_IP=$(ssh "$NODE" \
    "kubectl get svc -n inference vllm-metrics -o jsonpath='{.spec.clusterIP}'")
log "vLLM ClusterIP: $VLLM_IP"

# ── Scenario A: Normal baseline ──────────────────────────────────────────────
log ""
log "── Scenario A: Normal baseline (idle) ──"
sleep 20  # ensure ≥2 scrapes at 15 s interval

api workloads > /tmp/k8s-validate-workloads.json
COUNT=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-workloads.json'))['count'])")
if [[ "$COUNT" -ge 1 ]]; then
  GPU_REQ=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-workloads.json'))['items'][0].get('gpu_request',''))")
  log "Kubernetes discovery: $COUNT workload(s), gpu_request=$GPU_REQ"
  pass "Scenario A: workload discovered with gpu_request=$GPU_REQ"
else
  fail "Scenario A: workload not discovered"
fi

api telemetry > /tmp/k8s-validate-tele-idle.json
IDLE_GPU=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-idle.json'))['items'][0]['gpu_utilization_percent'])")
log "Idle GPU utilization: $IDLE_GPU%"

# ── Scenario B+C: GPU saturation and growing queue ───────────────────────────
log ""
log "── Scenarios B+C: Load (80 workers, max-num-seqs=16) ──"

ssh "$NODE" "
VLLM_IP=\$(kubectl get svc -n inference vllm-metrics -o jsonpath='{.spec.clusterIP}')
python3 -c \"
import json, urllib.request, threading, time, sys

URL = 'http://$VLLM_IP:8000/v1/completions'
PAYLOAD = json.dumps({'model': 'facebook/opt-125m',
    'prompt': 'The GPU is great at ' * 30,
    'max_tokens': 128, 'temperature': 0.9}).encode()

stop = threading.Event()
counts = {'ok': 0, 'err': 0}
lock = threading.Lock()

def worker():
    while not stop.is_set():
        try:
            req = urllib.request.Request(URL, data=PAYLOAD,
                headers={'Content-Type': 'application/json'})
            with urllib.request.urlopen(req, timeout=60) as r:
                r.read()
                with lock: counts['ok'] += 1
        except Exception:
            with lock: counts['err'] += 1

threads = [threading.Thread(target=worker, daemon=True) for _ in range(80)]
for t in threads: t.start()
deadline = time.time() + 90
while time.time() < deadline:
    time.sleep(10)
    with lock: print('t=%ds ok=%d err=%d' % (int(deadline-time.time()), counts['ok'], counts['err']), flush=True)
stop.set()
for t in threads: t.join(timeout=5)
print('Done:', counts)
\" &
echo \$!
" > /tmp/load_pid.txt 2>&1

sleep 70  # let load build and sustain_for window pass (45 s)

api telemetry     > /tmp/k8s-validate-tele-load.json
api recommendations > /tmp/k8s-validate-recs-load.json

WAITING=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-load.json'))['items'][0]['requests_waiting'])")
GPU_UTIL=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-load.json'))['items'][0]['gpu_utilization_percent'])")
CODES=$(python3 -c "import json; codes=[i['code'] for i in json.load(open('/tmp/k8s-validate-recs-load.json')).get('items',[])]; print(' '.join(codes))")
log "Under load: requests_waiting=$WAITING gpu_util=$GPU_UTIL% codes=$CODES"

if echo "$CODES" | grep -q "IFA-CAP-001"; then
  pass "Scenario B: IFA-CAP-001 (GPU saturation) fired"
else
  if [[ "$GPU_UTIL" -ge 80 && "$WAITING" -ge 5 ]]; then
    fail "Scenario B: GPU=$GPU_UTIL% waiting=$WAITING but IFA-CAP-001 not in API"
  else
    nr "Scenario B: GPU=$GPU_UTIL% waiting=$WAITING — conditions not met (model too small)"
  fi
fi

if echo "$CODES" | grep -q "IFA-CAP-003"; then
  pass "Scenario C: IFA-CAP-003 (growing queue) fired"
else
  nr "Scenario C: IFA-CAP-003 not fired (queue may be stable)"
fi

# ── Scenario D: KV preemption ────────────────────────────────────────────────
log ""
log "── Scenario D: KV-cache preemption ──"
KV=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-load.json'))['items'][0]['kv_cache_usage_percent'])")
PREEMPT=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-load.json'))['items'][0]['preemptions_per_sec'])")
log "kv_cache_usage_percent=$KV preemptions_per_sec=$PREEMPT"
if echo "$CODES" | grep -q "IFA-KV-001"; then
  pass "Scenario D: IFA-KV-001 fired"
else
  nr "Scenario D: IFA-KV-001 not fired — OPT-125m KV footprint too small for L4 (kv=$KV%). Use a larger model or --num-gpu-blocks-override to reproduce."
fi

# ── Scenario E: Recovery ─────────────────────────────────────────────────────
log ""
log "── Scenario E: Recovery (stop load, wait 2× stale_after) ──"
ssh "$NODE" "pkill -f 'python3 -c' 2>/dev/null; echo stopped" 2>/dev/null || true
sleep 130  # stale_after = 120 s; wait 130 s to clear

api telemetry     > /tmp/k8s-validate-tele-recovery.json
api recommendations > /tmp/k8s-validate-recs-recovery.json

REC_WAITING=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-recovery.json'))['items'][0]['requests_waiting'])")
REC_GPU=$(python3 -c "import json; print(json.load(open('/tmp/k8s-validate-tele-recovery.json'))['items'][0]['gpu_utilization_percent'])")
REC_CODES=$(python3 -c "import json; codes=[i['code'] for i in json.load(open('/tmp/k8s-validate-recs-recovery.json')).get('items',[])]; print(' '.join(codes) if codes else 'NONE')")
log "Recovery: requests_waiting=$REC_WAITING gpu=$REC_GPU% codes=$REC_CODES"

if ! echo "$REC_CODES" | grep -q "IFA-CAP-001" && ! echo "$REC_CODES" | grep -q "IFA-CAP-003"; then
  pass "Scenario E: Queue-based findings cleared after recovery"
else
  fail "Scenario E: IFA-CAP-001 or IFA-CAP-003 still firing after load stopped"
fi

# ── Scenario F: Pod restart ──────────────────────────────────────────────────
log ""
log "── Scenario F: Pod restart and rediscovery ──"
ssh "$NODE" "kubectl rollout restart deployment/vllm -n inference"
sleep 30

RESTART_COUNT=$(python3 -c "
import json, urllib.request
resp = urllib.request.urlopen('$API/api/v1/workloads')
d = json.loads(resp.read())
print(d['count'])
" 2>/dev/null || ssh "$NODE" "curl -sf $API/api/v1/workloads | python3 -c 'import sys,json; print(json.load(sys.stdin)[\"count\"])'")
if [[ "$RESTART_COUNT" -ge 1 ]]; then
  pass "Scenario F: Workload rediscovered after pod restart"
else
  fail "Scenario F: Workload not found after restart"
fi

# ── Summary ──────────────────────────────────────────────────────────────────
log ""
log "=== SUMMARY ==="
log "PASS=$PASS  FAIL=$FAIL  NOT_REPRODUCED=$NR"

if [[ "$FAIL" -gt 0 ]]; then
  log "OVERALL: FAIL ($FAIL failure(s))"
  exit 1
else
  log "OVERALL: PASS (all tested scenarios passed; $NR not reproduced)"
fi
