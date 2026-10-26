# Runtime support

What IFA reads from each runtime, what it deliberately does not, and how far each
adapter has actually been validated.

## Validation status

| Adapter | Implemented against | Tested against | Run against a live server |
|---|---|---|---|
| vLLM | `vllm/v1/metrics/loggers.py` (metric names, types, labels, histogram bucket boundaries) | Fixtures built from those definitions, including a V0-era payload and a two-model payload; plus seven verbatim live captures — three from CPU (idle, loaded, capacity-queued) and four from two separate NVIDIA L4 sessions (KV-cache exhaustion, settled exhaustion, recovery); plus K8s API responses | **Yes** — vLLM 0.28.0 on ARM64 CPU (`facebook/opt-125m`), on two NVIDIA L4 GPUs (JarvisLabs and E2E Networks) under KV-cache exhaustion, and inside a K3s cluster with DCGM producing 4 live rule firings |
| Triton | The published metrics documentation | Fixtures for the default and summary-latency configurations; plus three CPU captures and three GPU E2E captures from Triton 25.12 | **Yes** — Triton 25.12, ARM64 CPU (Python echo model) and one NVIDIA L4 GPU (TorchScript `gpu_matmul`, 44,432 requests, all 10 metric families confirmed) |
| DCGM Exporter | Published field IDs | Fixtures including multi-GPU and partial payloads; plus five verbatim live captures from two NVIDIA L4 sessions: a matched refresh-lag pair (JarvisLabs bundled exporter) and idle/load/recovery (NVIDIA's official `dcgm-exporter:3.3.9-3.6.1-ubuntu22.04`) | **Yes** — both a DCGM-compatible bundled exporter (JarvisLabs) and NVIDIA's official `dcgm-exporter` container image on one NVIDIA L4 (E2E Networks, driver 570.133.20) |

All three adapters have now been run against a live endpoint, including GPU
hardware. The remaining gaps are:

- **Multi-GPU behaviour** has not been validated on hardware. All L4 sessions
  used a single-GPU node; a single-card capture proves nothing about
  max-across-devices aggregation, multi-GPU KV-cache pooling, or per-device
  attribution.
- **Live Kubernetes E2E with KV-cache preemption** — `IFA-KV-001` was validated
  in the dynamic kind integration test and via raw counter deltas in the E2E L4
  session, but was not reproduced through IFA's telemetry API inside K3s because
  `facebook/opt-125m` is too small to fill the L4's 20 GiB KV cache (≈ 0.23%
  usage). A larger model or constrained `--num-gpu-blocks-override` would
  reproduce it.
- **GPU-backed Triton in Kubernetes** — GPU-backed Triton was validated as a
  Docker container (direct `ifa check`). It has not been deployed as a
  Kubernetes workload, so the Kubernetes discovery → Triton adapter → API path
  has not been exercised.

See
[`internal/runtime/dcgm/testdata/e2e/README.md`](../internal/runtime/dcgm/testdata/e2e/README.md),
[`internal/runtime/triton/testdata/e2e/README.md`](../internal/runtime/triton/testdata/e2e/README.md),
and
[`scripts/gpu-validation/evidence/k8s/README.md`](../scripts/gpu-validation/evidence/k8s/README.md)
for full provenance.

---

## vLLM

### Three properties that shape the adapter

**1. Every series is labelled.** vLLM tags each metric with `model_name` and, in
the V1 engine, `engine` (the engine-core index):

```
vllm:num_requests_running{model_name="meta-llama/Llama-3.1-8B-Instruct",engine="0"} 11.0
```

A lookup by bare metric name matches nothing. This is the single most common way
a vLLM integration silently returns zeros, and it is why `ifa check` exists.

**2. Latencies are histograms, not summaries.** There is no `quantile` label to
read. Percentiles are interpolated from cumulative `_bucket` series using the
same linear interpolation Prometheus applies in `histogram_quantile`, which means
their precision is bounded by vLLM's bucket boundaries. Concretely: vLLM's
queue-time histogram starts at 0.3s, so any queue-time p95 below 300ms is
reported as roughly 285ms — the interpolation inside the first bucket. IFA cannot
do better than the data.

A quantile that lands in the `+Inf` bucket cannot be interpolated at all. IFA
returns the largest finite boundary, which **understates** the true value.

**3. There is no failure counter.** vLLM exposes
`vllm:request_success_total{finished_reason=...}` and nothing that counts
failures. An error rate cannot be derived from vLLM alone, so IFA leaves
`error_rate_percent` unmeasured rather than computing something plausible from
the success counter. The abort share is reported instead — `finished_reason="abort"`
is what vLLM records when a caller goes away, which is a real client-visible
signal.

### Metrics read

| vLLM metric | Type | Maps to |
|---|---|---|
| `vllm:num_requests_running` | gauge | `requests_running` |
| `vllm:num_requests_waiting` | gauge | `requests_waiting` |
| `vllm:num_requests_waiting_by_reason` | gauge | `waiting_for_capacity`, `waiting_deferred` |
| `vllm:kv_cache_usage_perc` | gauge | `kv_cache_usage_percent` (×100) |
| `vllm:num_preemptions_total` | counter | `preemptions_per_sec` |
| `vllm:time_to_first_token_seconds` | histogram | `ttft_p50/p95/p99_ms` |
| `vllm:e2e_request_latency_seconds` | histogram | `p50/p95/p99_latency_ms` |
| `vllm:request_queue_time_seconds` | histogram | `queue_time_p95_ms` |
| `vllm:generation_tokens_total` | counter | `tokens_per_second` |
| `vllm:prompt_tokens_total` | counter | `prompt_tokens_per_sec` |
| `vllm:prefix_cache_queries_total` / `_hits_total` | counters | `prefix_cache_hit_rate_percent` |
| `vllm:request_success_total` | counter | `request_rate_per_sec`, `abort_rate_percent` |

### Version differences

- **`gpu_cache_usage_perc` → `kv_cache_usage_perc`.** V1 renamed it. IFA reads
  the V1 name and falls back to the old one, and does not report the metric as
  missing when the fallback succeeded.
- **`num_requests_waiting_by_reason`** is V1-only. Absent on older builds, and
  treated as optional — `IFA-CAP-004` simply does not run.
- **`request_queue_time_seconds`** is V1-only and optional. Without it,
  `IFA-LAT-001` and `IFA-LAT-002` cannot attribute a slow TTFT and both stay
  dormant rather than guessing.
- **`engine` label.** V1 runs several engine cores. IFA sums request counts
  across them and takes the maximum of utilisation gauges, because summing a
  percentage across engines is meaningless.

### Live validation — CPU backend

The adapter was run against a real vLLM server with the following configuration:

- **Image:** `vllm/vllm-openai-cpu:latest-arm64`
  (`sha256:dbc1b4da66bbc0cb3a3f6859cd9046833f9540d34322b192625868f888e8e094`)
- **Version:** 0.28.0
- **Backend:** CPU, Linux ARM64 Docker VM
- **Model:** `facebook/opt-125m`
- **Baseline flags:** `--dtype=bfloat16 --gpu-memory-utilization 0.4`
- **Capacity-queued run also used:** `--max-num-seqs 2`

`ifa check` against the live `/metrics` endpoint reported 122 metric families,
0 unparseable lines, and all 13 required metrics present:

```
vllm:num_requests_running       found
vllm:num_requests_waiting       found
vllm:kv_cache_usage_perc        found
vllm:time_to_first_token_seconds found
vllm:e2e_request_latency_seconds found
vllm:generation_tokens_total    found
vllm:prompt_tokens_total        found
vllm:request_success_total      found
vllm:num_preemptions_total      found
vllm:request_queue_time_seconds found
vllm:prefix_cache_queries_total found
vllm:prefix_cache_hits_total    found
vllm:num_requests_waiting_by_reason found
```

Three verbatim `/metrics` payloads were captured:

| Fixture | State | Key observed values |
|---|---|---|
| `testdata/vllm_v1_captured.txt` | Post-traffic idle | requests_running=0, requests_waiting=0, TTFT p95≈39.9 ms |
| `testdata/vllm_v1_under_load.txt` | Active loaded | requests_running=10, requests_waiting=0, raw `kv_cache_usage_perc`≈0.02857 |
| `testdata/vllm_v1_queued.txt` | Capacity-queued (`--max-num-seqs 2`) | requests_waiting=12, `num_requests_waiting_by_reason{reason="capacity"}`=12, `reason="deferred"`=0, raw `kv_cache_usage_perc`≈0.00462 |

**KV-cache unit.** vLLM exposes `kv_cache_usage_perc` as a fraction where `1`
means 100% usage. The adapter multiplies by 100 before storing. The live values
above were low (under 3%) — the KV rules fire around 90% — so high-KV-cache
pressure was not exercised by these captures. The GPU-backed run below does
exercise it.

### Live validation — GPU backend (one NVIDIA L4)

A second run drove a GPU-backed server into genuine KV-cache exhaustion:

- **Image:** `vllm/vllm-openai:v0.28.0`
- **GPU:** one NVIDIA L4 (23034 MiB), driver 580.126.20, CUDA 13.0
- **Platform:** JarvisLabs.ai GPU VM, Ubuntu 22.04.5
- **Model:** `facebook/opt-125m`
- **Flags:** `--dtype half --gpu-memory-utilization 0.8 --max-model-len 2048
  --num-gpu-blocks-override 1024` — the KV cache was deliberately constrained to
  1024 blocks so exhaustion is reachable without exotic prompt lengths
- **Load:** continuous refill, not a one-shot batch — 100 worker threads each
  issuing a new request the moment the previous one completed, with a fixed
  223-token prompt and `ignore_eos: true` so every request ran to its full
  `max_tokens`. A batch drains as requests finish; continuous refill sustains
  pressure. The capture was taken 13 seconds into the run.

| Fixture | State | Key observed values |
|---|---|---|
| `testdata/vllm_gpu_l4_kv_exhausted.txt` | KV cache exhausted under sustained load | requests_running=84, requests_waiting=16, all 16 `reason="capacity"`, raw `kv_cache_usage_perc`≈0.99902 (**≈99.9%**), `num_preemptions_total`=53366 and climbing |

This is the only capture in the repository showing KV-cache saturation,
capacity-driven queueing and active preemption together in one payload, taken
against real GPU hardware rather than synthesized. GPU utilisation for the same
session was captured separately from a DCGM-compatible exporter at
`DCGM_FI_DEV_GPU_UTIL = 100`.

**What this validation does not establish:**

- **Multi-GPU behaviour.** One L4 is one card. A single-GPU capture proves
  nothing about max-across-devices aggregation or multi-GPU KV-cache pooling.
- **A preemption rate.** `num_preemptions_total` was observed nonzero and
  climbing, which is the signal that matters, but a *rate* needs two
  time-separated scrapes and none was measured. The absolute value also includes
  preemptions from earlier runs in the same long-lived vLLM process.
- Sustained exhaustion over long windows, or recovery behaviour after load drops
- Live V0 `gpu_cache_usage_perc` behavior
- NVIDIA's official `dcgm-exporter` image — the GPU utilisation above came from
  a DCGM-compatible exporter bundled by the host
- GPU-backed Triton behavior

### Requirements

- Metrics must be enabled. vLLM exposes nothing useful when started with
  `--disable-log-stats`.
- Set `model_name` in the target config when one server hosts several models.
  Without it the adapter aggregates across all of them, producing a queue depth
  and a cache utilisation that belong to no actual workload.

### GPU metrics

vLLM reports none. KV-cache occupancy is *not* GPU utilisation, and substituting
one for the other — tempting, since both are percentages — would make
`IFA-CAP-002` (queue with an idle GPU) undetectable, which is the single most
useful thing IFA catches. Configure `dcgm_url` for real GPU metrics, or accept
that the GPU rules do not run.

---

## Triton Inference Server

### Two details that are easy to get wrong

**`nv_gpu_utilization` is a rate in [0.0, 1.0], not a percentage.** Treating it
as a percentage turns a fully loaded GPU into "0.91% utilised" and makes the
idle-GPU rule fire permanently against every Triton deployment. IFA multiplies
by 100.

**The default latency metrics are cumulative counters, not percentiles.**
`nv_inference_request_duration_us` divided by the request count is a *mean*.
Publishing a mean as a p95 makes every latency threshold meaningless, so IFA
does not: without summary latencies enabled, the percentile fields stay
unmeasured and the latency rules stay dormant.

To get percentiles, start Triton with:

```
--metrics-config summary_latencies=true
```

which adds `nv_inference_request_summary_us` and `nv_inference_queue_summary_us`
with `quantile` labels. IFA reads 0.5, 0.95 and 0.99 from them.

### Metrics read

| Triton metric | Type | Maps to |
|---|---|---|
| `nv_inference_pending_request_count` | gauge | `requests_waiting` |
| `nv_inference_request_success` | counter | `request_rate_per_sec` |
| `nv_inference_request_failure` | counter | `error_rate_percent` (summed across `reason`) |
| `nv_inference_request_summary_us` | summary | `p50/p95/p99_latency_ms` (optional) |
| `nv_inference_queue_summary_us` | summary | `queue_time_p95_ms` (optional) |
| `nv_gpu_utilization` | gauge | `gpu_utilization_percent` (×100) |
| `nv_gpu_memory_used_bytes` / `_total_bytes` | gauges | `gpu_memory_used_percent` |

Unlike vLLM, Triton *does* expose a failure counter, so `IFA-ERR-001` works.

Set `model_name` when the server hosts several models. `ifa check` lists the
models present in a payload when the configured one matches none, which is the
usual cause of a Triton target reporting nothing.

### Live validation

The adapter was run against a real Triton server with the following configuration:

- **Image:** `nvcr.io/nvidia/tritonserver:25.12-pyt-python-py3`
  (`sha256:40fd29c56c1b69b5b27514d781e0af52e757b16a3c97a5540e15820d2ddb451d`)
- **Version:** Triton 25.12
- **Backend:** CPU (Python backend, ARM64 Docker VM, no GPU)
- **Model:** `echo` — minimal Python passthrough
- **Baseline run:** default metrics, no GPU present
- **Summary-latencies run:** `--metrics-config summary_latencies=true`

`ifa check` against the live `/metrics` endpoint (default mode) reported 16 metric
families, 0 unparseable lines, and the following status:

```
nv_inference_request_success        found
nv_inference_request_failure        found
nv_inference_pending_request_count  found
nv_inference_request_duration_us    found
nv_inference_queue_duration_us      found
nv_gpu_utilization                  MISSING
nv_gpu_memory_used_bytes            MISSING
nv_gpu_memory_total_bytes           MISSING
nv_inference_request_summary_us     optional, absent
nv_inference_queue_summary_us       optional, absent
```

With `--metrics-config summary_latencies=true` the same check reported 31 families
and both summary families moved from `optional, absent` to `found`. The three GPU
families remained `MISSING` throughout — a CPU-only host emits none of them.

Three verbatim `/metrics` payloads were captured:

| Fixture | State | Key observed values |
|---|---|---|
| `testdata/triton_captured_idle.txt` | Server started, zero requests | request_success=0, pending=0, no GPU metrics |
| `testdata/triton_captured_loaded.txt` | 280 completed requests | request_success=280, duration_us=121428, queue_duration_us=8417, no GPU metrics |
| `testdata/triton_captured_summary.txt` | 200 requests, summary latencies enabled | p50=0.504 ms, p95=0.921 ms, p99=1.156 ms, queue_p95=0.102 ms, no GPU metrics |

**GPU fields on a CPU server.** All three captures contain no `nv_gpu_*` families.
The adapter maps their absence to `OK == false` (unmeasured), not zero. GPU-dependent
rules do not fire on CPU deployments. The `nv_gpu_utilization` ×100 conversion
cannot be verified without GPU hardware.

**What the CPU validation does not establish:**

- The `nv_gpu_utilization` ×100 conversion (no GPU to measure; verified in the
  GPU run below)
- Non-zero pending counts (echo model is too fast)
- Multi-model label isolation on a live server
- Dynamic-batcher behavior
- TensorRT or ONNX backend behavior

### Live validation — GPU backend (one NVIDIA L4)

The adapter was run against a real GPU-backed Triton server:

- **Image:** `nvcr.io/nvidia/tritonserver:25.12-pyt-python-py3`
- **GPU:** one NVIDIA L4 (24570 MiB), driver 570.133.20, CUDA 12.8
- **Platform:** E2E Networks, Delhi NCR (`p95labs-ifa-l4-v2`)
- **Backend:** LibTorch/PyTorch (`pytorch_libtorch`). The Python backend lacks
  PyTorch in the official image; a TorchScript model was generated on the host
  and served natively via LibTorch.
- **Model:** `gpu_matmul` — random 128×128 matrix multiply, batch size 1,
  TorchScript (`.pt`)
- **Load:** 40 concurrent workers for 90 s; `44,432` successful requests, 0
  errors

`ifa check` against the live `/metrics` endpoint reported **37 metric families,
0 unparseable lines**, and all 10 required Triton metric families found:

```
nv_inference_request_success         found
nv_inference_request_failure         found
nv_inference_pending_request_count   found
nv_inference_request_duration_us     found
nv_inference_queue_duration_us       found
nv_gpu_utilization                   found  (0.02 during load)
nv_gpu_memory_used_bytes             found  (306,184,192 under load)
nv_gpu_memory_total_bytes            found  (24,152,899,584)
nv_inference_request_summary_us      found
nv_inference_queue_summary_us        found
```

Three verbatim `/metrics` payloads were captured in
`internal/runtime/triton/testdata/e2e/`:

| Fixture | State | Key values |
|---|---|---|
| `triton_gpu_l4_idle.txt` | Server started, 0 requests | `nv_gpu_utilization=0`, memory_used=270 MiB, pending=0 |
| `triton_gpu_l4_under_load.txt` | 30,637 requests completed | `nv_gpu_utilization=0.02` (→IFA: 2%), pending=30, p95=109 ms, queue_p95=78 ms |
| `triton_gpu_l4_recovery.txt` | After load, 44,434 total | `nv_gpu_utilization=0`, pending=0 |

**`nv_gpu_utilization` ×100 conversion verified.** Triton reports `0.02`
(fraction in [0,1]); IFA converts to `2%`. DCGM independently reported `2%`
GPU utilisation for the same session, confirming the conversion is correct.
The concern that `0.9` could be misread as `0.9%` instead of `90%` is closed.

**What this validation does not establish:**

- Non-zero error rates (no deliberate failure injection)
- Multi-model label isolation on a live GPU server
- Dynamic-batcher behavior (model is too fast to queue)
- TensorRT or ONNX backend behavior
- GPU-backed Triton deployed as a Kubernetes workload (only Docker-direct
  `ifa check` was run; the Kubernetes discovery path was not exercised)

---

## DCGM Exporter

The only source of real accelerator utilisation. Fields read:

| Field | Maps to |
|---|---|
| `DCGM_FI_DEV_GPU_UTIL` | `gpu_utilization_percent` |
| `DCGM_FI_DEV_FB_USED` / `DCGM_FI_DEV_FB_FREE` | `gpu_memory_used_percent` |
| `DCGM_FI_DEV_MEM_COPY_UTIL`, `DCGM_FI_DEV_SM_CLOCK`, `DCGM_FI_DEV_GPU_TEMP` | parsed, not yet used by any rule |

DCGM reports used and free framebuffer, never a total. A device exposing only one
of them yields no percentage rather than a figure that is wrong by orders of
magnitude.

**Aggregation is max across devices.** One saturated GPU in a multi-GPU pod is
what an operator needs to see; averaging hides it.

**Attribution caveat.** A DCGM Exporter endpoint reports the GPUs on a *node*,
not the GPUs belonging to one pod. On a node running several inference workloads
the utilisation IFA attributes to a target is the node's busiest GPU, which may
belong to something else. On dedicated GPU nodes — the common arrangement for
serving — the two coincide. Per-pod attribution would need DCGM's
Kubernetes-aware labels and pod-to-GPU mapping, which is on the roadmap.

### Live validation — one NVIDIA L4, via a DCGM-compatible exporter

- **GPU:** one NVIDIA L4 (23034 MiB), driver 580.126.20, CUDA 13.0
- **Platform:** JarvisLabs.ai GPU VM, Ubuntu 22.04.5
- **Exporter:** `/opt/jarvis/guest-monitor/bin/dcgm-exporter` — JarvisLabs'
  bundled DCGM-compatible exporter, **not** NVIDIA's official `dcgm-exporter`
  container image. It exposes 25 `DCGM_FI_DEV_*` series, a narrower set than the
  canonical image; every field the adapter reads (`GPU_UTIL`, `MEM_COPY_UTIL`,
  `FB_USED`, `FB_FREE`) was present and correctly formatted.
- **GPU activity:** vLLM 0.28.0 serving `facebook/opt-125m` under sustained
  concurrent load — the same session as the vLLM GPU capture above

Two verbatim payloads were captured as a matched pair:

| Fixture | State | Key observed values |
|---|---|---|
| `testdata/dcgm_l4_under_load_settled.txt` | Under sustained load, exporter settled | `DCGM_FI_DEV_GPU_UTIL`=100, `FB_USED`=1550 MiB |
| `testdata/dcgm_l4_refresh_lag_stale.txt` | Identical load, exporter not yet settled | `DCGM_FI_DEV_GPU_UTIL`=0, `FB_USED`=1548 MiB |

**This exporter reads up to twenty seconds late.** Dense polling measured roughly
**18–20 seconds** between a real GPU state change (idle → 100%) and that change
appearing in `DCGM_FI_DEV_GPU_UTIL`. Once settled the reading was stable across
127 consecutive samples at ~0.8s intervals. The framebuffer figure was in the
same ~1.5 GiB range in both captures, confirming the GPU was genuinely busy in
both — only the utilisation reading was stale. This was confirmed by reading the
raw `/metrics` endpoint directly, independent of IFA, so it is a property of the
exporter rather than of IFA's parser or collector.

The operational consequence: **size `collector.interval` and `sustain_for`
comfortably above your DCGM source's own refresh lag.** An observation window
shorter than the lag reports `gpu_utilization_percent = 0` while the GPU is
saturated, which is exactly the condition `IFA-CAP-002` ("queueing while the GPU
is idle") looks for. The ~18–20s figure is specific to this exporter and must
not be assumed to generalise; NVIDIA's own `dcgm-exporter` has not been measured.

**What this validation does not establish:**

- Behaviour against NVIDIA's official `dcgm-exporter` container image
- Multi-GPU aggregation on real hardware — only one physical GPU was available,
  so the max-across-devices logic is still fixture-only
- GPU temperature or SM clock behaviour under real thermal load
- Refresh characteristics of any other DCGM source

---

## Kubernetes end-to-end validation

The full Kubernetes pipeline (discovery → scrape → rate tracking → rule
evaluation → REST API) has been validated on real GPU hardware inside K3s.

### Configuration validated

- K3s v1.36.5+k3s1, single-node cluster (E2E Networks NVIDIA L4)
- NVIDIA device plugin + CDI runtime (`nvidia.com/gpu: 1` advertised)
- vLLM 0.28.0 pod with `resources.limits: {nvidia.com/gpu: "1"}`
- Official NVIDIA DCGM exporter DaemonSet (without GPU resource request,
  privileged mode)
- IFA installed via `deploy/helm/ifa` with explicit `collector.targets`
  referencing in-cluster ClusterIP URLs for vLLM and DCGM

### Rules confirmed against live GPU telemetry

| Rule | Confirmed | Conditions |
|---|---|---|
| `IFA-EFF-003` | **Yes** | GPU memory 92–94% (DCGM), threshold 90% |
| `IFA-CAP-001` | **Yes** | `requests_waiting=63` + `gpu_util=91%`, sustained 60 s |
| `IFA-CAP-003` | **Yes** | Queue grew 63 reqs/window, CRITICAL severity |
| `IFA-LAT-001` | **Yes** | TTFT p95=888 ms, 89% queue share, threshold 500 ms |
| `IFA-OBS-001` | **Yes** | TTFT/E2E metrics absent before first request |
| `IFA-KV-001` | Not reproduced | OPT-125m KV footprint too small (≈ 0.23% of 20 GiB) |

### Known configuration constraints

**Service naming.** Do not name the vLLM Kubernetes Service `vllm`: Kubernetes
injects `VLLM_PORT=tcp://<ip>:<port>` from the service name, which collides
with vLLM v0.28.0's own internal `VLLM_PORT` environment variable and causes
startup failure. Name the service `vllm-metrics` or any other name that does
not match the process's own env-var namespace.

**DCGM without GPU resource.** The DCGM DaemonSet needs `--privileged` and
access to NVML/UVM devices but must NOT request `nvidia.com/gpu`, because the
node has one GPU and vLLM must receive it. Use the Docker run flags
`--gpus all --privileged` or, in Kubernetes, add privilege without a resource
request.

**K3s containerd runtime path.** The NVIDIA runtime configuration must be
written to `/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/`,
not `/etc/containerd/config.d/`. K3s does not import the generic containerd
drop-in directory.

See `scripts/gpu-validation/evidence/k8s/README.md` for the full session
provenance and raw API captures.

---

## Adding a runtime

`internal/runtime.Adapter` is three methods:

```go
type Adapter interface {
    Runtime() telemetry.Runtime
    ExpectedMetrics() []string
    Parse(payload string, modelName string) (Reading, error)
}
```

`Parse` is a pure function: no I/O, no clock, no state. Rate conversion,
counter-reset handling and staleness live in the collector, so a new adapter gets
them for free and cannot get them subtly wrong. Register it in the `adapters` map
in `internal/collector/collector.go`.

Build the test fixture from the runtime's own metric definitions — its source or
its documentation — not from the metric names alone. That is precisely how the
original vLLM adapter here passed its tests while being unable to read a real
vLLM server.
