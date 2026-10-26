# Kubernetes GPU E2E Evidence — Milestone B

Verbatim API responses from IFA running inside a single-node K3s cluster against
a real NVIDIA L4, collected 2026-10-26.

## Infrastructure

| Item | Value |
|---|---|
| GPU node | E2E Networks, Delhi NCR (`p95labs-ifa-l4-v2`) |
| GPU | 1× NVIDIA L4, 24570 MiB |
| Driver | 570.133.20 |
| CUDA | 12.8 |
| OS | Ubuntu 22.04.5 LTS |
| Kubernetes | K3s v1.36.5+k3s1 (single-node) |
| vLLM image | `vllm/vllm-openai:v0.28.0` |
| Model | `facebook/opt-125m` |
| DCGM exporter | `nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04` |
| IFA image | Built from `main` (`cd6689f8`), cross-compiled linux/amd64 |
| IFA Helm chart | `deploy/helm/ifa` version `0.2.0-alpha.1` |

## How these were captured

IFA installed in namespace `ifa`, target `inference/opt-125m` (vLLM + DCGM URL).
Port-forward to `svc/ifa-gpu:8080`. All API calls from inside the node against
`http://127.0.0.1:9080/api/v1/...`.

## Files

| File | When captured | Notes |
|---|---|---|
| `workloads.json` | Under load | Kubernetes discovery — `vllm` deployment, `gpu_request=1` |
| `telemetry_under_load.json` | Under sustained load (80 workers, max-num-seqs=16) | `requests_waiting=63`, `gpu_utilization_percent=91` |
| `recommendations_under_load.json` | Under sustained load | `IFA-CAP-001`, `IFA-CAP-003`, `IFA-EFF-003`, `IFA-LAT-001` all firing |
| `telemetry_recovery.json` | 90 s after load stopped | All counters at zero, GPU idle |
| `recommendations_recovery.json` | After recovery | `IFA-CAP-001` and `IFA-CAP-003` gone; `IFA-EFF-003` and `IFA-LAT-001` still in retention window |

## Scenario results

| Scenario | Rule | Result | Evidence |
|---|---|---|---|
| A — Normal baseline (idle) | (no rule expected) | **PASS** | Workload discovered, `gpu_util=0`, `requests_waiting=0`, `IFA-EFF-003` correctly fires for pre-allocated KV memory |
| B — Sustained GPU saturation | `IFA-CAP-001` | **PASS** | `gpu_util=91%` + `requests_waiting=63` sustained 60 s; finding in API with evidence block |
| C — Growing queue | `IFA-CAP-003` | **PASS** | Queue grew 63 reqs across window; CRITICAL severity finding in API |
| D — KV-cache preemption | `IFA-KV-001` | **NOT REPRODUCED** | OPT-125m is too small to fill the L4's 20 GiB KV cache (`kv_cache_usage_percent ≈ 0.23%`); a model with a larger KV footprint per request is needed. The counter-based rate rule was validated separately in the dynamic kind integration test. |
| E — Recovery | `IFA-CAP-001`, `IFA-CAP-003` expire | **PASS** | Load stopped; `requests_waiting=0`, `gpu_util=0`; queue-based findings absent after `stale_after=120s` |
| F — Pod restart and rediscovery | (workloads API) | **PASS** | `kubectl rollout restart`; vLLM pod replaced; workload re-appears with `ready_replicas=1`; telemetry resumes without fabricated rates |

## Key telemetry values (under load)

From `telemetry_under_load.json`:

```
request_rate_per_sec   = 91.2
tokens_per_second      = (not captured — OPT-125m V1 exposes ttft via separate histogram)
requests_running       = 15  (capped at --max-num-seqs 16)
requests_waiting       = 63
gpu_utilization_pct    = 91   (DCGM: DCGM_FI_DEV_GPU_UTIL)
gpu_memory_used_pct    = 92.9 (DCGM: FB_USED / (FB_USED + FB_FREE))
kv_cache_usage_pct     = 0.23
preemptions_per_sec    = 0    (no preemptions with this workload)
queue_time_p95_ms      = 787  (from vllm:request_queue_time_seconds histogram)
ttft_p95_ms            = 888  (from vllm:time_to_first_token_seconds histogram)
```

## Findings produced (under load)

From `recommendations_under_load.json`:

```
IFA-CAP-003  CRITICAL  Queue is growing, not just deep
IFA-CAP-001  WARNING   Requests queueing while the GPU is saturated
IFA-EFF-003  WARNING   GPU memory near capacity (pre-allocated KV cache)
IFA-LAT-001  WARNING   Time to first token dominated by queueing (TTFT 888ms, 89% queue)
```

## Limitations

- **Single GPU, single node.** Multi-GPU aggregation and cross-node attribution
  remain untested on real hardware.
- **OPT-125m KV footprint.** The model is too small to exhaust the L4's 20 GiB
  KV cache under normal conditions. `IFA-KV-001` was not exercised in this
  cluster session; it was validated separately (see ROADMAP.md and the dynamic
  kind integration test).
- **vLLM v0.28.0 TTFT/E2E metrics.** Available only after at least one request
  completes. `IFA-OBS-001` fires briefly at startup until the first request
  populates those histograms.
- **No HPA configured.** `IFA-SCL-001` and `IFA-SCL-002` (autoscaler rules)
  were not exercised.
