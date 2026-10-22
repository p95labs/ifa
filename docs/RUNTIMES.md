# Runtime support

What IFA reads from each runtime, what it deliberately does not, and how far each
adapter has actually been validated.

## Validation status

| Adapter | Implemented against | Tested against | Run against a live server |
|---|---|---|---|
| vLLM | `vllm/v1/metrics/loggers.py` (metric names, types, labels, histogram bucket boundaries) | Fixtures built from those definitions, including a V0-era payload and a two-model payload; plus four verbatim live captures — three from CPU (idle, loaded, capacity-queued) and one from an NVIDIA L4 at ≈99.9% KV-cache occupancy | **Yes** — vLLM 0.28.0 on ARM64 CPU (`facebook/opt-125m`) and on one NVIDIA L4 under KV-cache exhaustion |
| Triton | The published metrics documentation | Fixtures for the default and summary-latency configurations; plus three verbatim live captures (idle, loaded, summary-latencies-enabled) from Triton 25.12 | **Yes** — Triton 25.12, ARM64 CPU backend, Python echo model; GPU-backed Triton not yet validated |
| DCGM Exporter | Published field IDs | Fixtures including multi-GPU and partial payloads; plus a matched pair of verbatim live captures (settled and refresh-lagged) from an NVIDIA L4 | **Yes** — a DCGM-*compatible* exporter on one NVIDIA L4; NVIDIA's official `dcgm-exporter` image not yet validated |

All three adapters have now been run against a live endpoint. The gaps are
specific, and they are not "unvalidated" in general:

- **GPU-backed Triton** has not been run. Triton's live validation is CPU-only,
  where the server publishes no `nv_gpu_*` families at all, so the
  `nv_gpu_utilization` ×100 conversion is still unverified against a real reading.
- **NVIDIA's official `dcgm-exporter`** has not been run. The DCGM validation
  used a DCGM-compatible exporter bundled by the GPU host — a narrower field set
  that happens to include every field the adapter reads. See
  [`internal/runtime/dcgm/testdata/README.md`](../internal/runtime/dcgm/testdata/README.md).
- **Multi-GPU behaviour** has not been validated on hardware. One NVIDIA L4 was
  available; a single-GPU capture proves nothing about max-across-devices
  aggregation, multi-GPU KV-cache pooling, or per-device attribution.
- **Live counter rates** from a real inference server have not been measured.
  The capture set is single-snapshot-only. Counter-rate and queue-growth
  rules are now validated end-to-end in the dynamic kind integration test
  (see `scripts/kind-dynamic-integration.sh` and the Roadmap), but against
  a synthetic metrics server, not a live vLLM or Triton process.

If you run GPU-backed Triton or the official `dcgm-exporter`, `ifa check <url>`
produces the report that would close the first two.

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

**What this validation does not establish:**

- GPU-backed Triton behavior
- The `nv_gpu_utilization` ×100 conversion (no GPU to measure)
- Non-zero pending counts (echo model is too fast)
- Multi-model label isolation on a live server
- Dynamic-batcher behavior
- TensorRT, PyTorch, or ONNX backend behavior

The full Kubernetes discovery → scrape → recommendation path *is* validated, but
with the vLLM adapter: the kind integration test serves a vLLM capture. No
Triton target has been run through a cluster.

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
