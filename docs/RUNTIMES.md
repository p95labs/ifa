# Runtime support

What IFA reads from each runtime, what it deliberately does not, and how far each
adapter has actually been validated.

## Validation status

| Adapter | Implemented against | Tested against | Run against a live server |
|---|---|---|---|
| vLLM | `vllm/v1/metrics/loggers.py` (metric names, types, labels, histogram bucket boundaries) | Fixtures built from those definitions, including a V0-era payload and a two-model payload; plus three verbatim live captures (idle, loaded, capacity-queued) from vLLM 0.28.0 | **Yes** — vLLM 0.28.0, ARM64 CPU backend, `facebook/opt-125m`; GPU-backed vLLM not yet validated |
| Triton | The published metrics documentation | Fixtures for the default and summary-latency configurations | **No** |
| DCGM Exporter | Published field IDs | Fixtures including multi-GPU and partial payloads | **No** |

The vLLM adapter has been validated against a live CPU-backed server; Triton and
DCGM have not. If you run either, `ifa check <url>` produces the report that
would close it.

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

### Live validation

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
above were low (under 3%), so high-KV-cache pressure was not exercised.

**What this validation does not establish:**

- GPU-backed vLLM behavior
- Real device-side GPU KV-cache behavior
- Live V0 `gpu_cache_usage_perc` behavior
- Preemption behavior — `vllm:num_preemptions_total` was present but preemption was not deliberately triggered
- DCGM on real GPU hardware
- Live Triton behavior
- Full Kubernetes discovery → scrape → recommendation integration

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
