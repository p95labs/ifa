# vLLM adapter testdata

This directory contains two categories of fixture: synthetic payloads constructed
from vLLM's own metric definitions, and verbatim Prometheus payloads captured from
a real vLLM server.

---

## Synthetic fixtures

These files are controlled test inputs used for deterministic assertions about
parser behavior, version compatibility, multi-model handling, and edge cases.
They are constructed from vLLM's source and documentation, not from a live server.

| File | Purpose |
|---|---|
| `vllm_v1_healthy.txt` | Complete V1 payload; used for full metric extraction and counter assertions |
| `vllm_v0_legacy.txt` | Pre-V1 payload without the `engine` label and with the old `gpu_cache_usage_perc` name |
| `vllm_two_models.txt` | Single process serving two models; validates per-model label filtering |

---

## Live captures

These three files are verbatim `/metrics` payloads captured from a real vLLM
server. They are used by `TestCapturedPayload` to verify the adapter against
authentic exposition.

### Provenance

- **Image:** `vllm/vllm-openai-cpu:latest-arm64`
  (`sha256:dbc1b4da66bbc0cb3a3f6859cd9046833f9540d34322b192625868f888e8e094`)
- **Version:** 0.28.0
- **Backend:** CPU
- **Platform:** Linux ARM64 Docker VM
- **Model:** `facebook/opt-125m`
- **Baseline flags:** `--dtype=bfloat16 --gpu-memory-utilization 0.4`
- **Queued capture additionally used:** `--max-num-seqs 2`

### State details

**`vllm_v1_captured.txt`** — post-traffic idle

- `num_requests_running = 0`
- `num_requests_waiting = 0`
- `kv_cache_usage_perc = 0`
- TTFT p95 parsed at approximately 39.9 ms

**`vllm_v1_under_load.txt`** — active loaded

- `num_requests_running = 10`
- `num_requests_waiting = 0`
- raw `kv_cache_usage_perc = 0.02857142857142858`

**`vllm_v1_queued.txt`** — capacity-queued (`--max-num-seqs 2`)

- `num_requests_running = 2`
- `num_requests_waiting = 12`
- `num_requests_waiting_by_reason{reason="capacity"} = 12`
- `num_requests_waiting_by_reason{reason="deferred"} = 0`
- raw `kv_cache_usage_perc = 0.004618937644341847`

### KV-cache units

vLLM exposes `kv_cache_usage_perc` as a fraction where `1` means 100% usage.
The adapter converts the raw value to percentage units by multiplying by 100:

- `0.02857142857142858` → approximately `2.857%`
- `0.004618937644341847` → approximately `0.462%`

### Verbatim fixture policy

The live capture files are stored verbatim and must not be normalized, trimmed,
reordered, filtered, or otherwise cleaned merely to reduce size or remove noise.

Metrics such as Python runtime, process, garbage-collector, and other
exporter-emitted series are intentionally retained. They preserve the authentic
shape of the live `/metrics` endpoint and provide realistic coverage that the
adapter safely ignores unrelated exposition families.

If a capture must be regenerated, the replacement must be another raw
`/metrics` response, and the provenance recorded above must be updated to
reflect the new environment.

---

## Validation scope

These captures validate the vLLM adapter against a real CPU-backed vLLM 0.28.0
server. They do not establish:

- GPU-backed vLLM behavior
- Real device-side GPU KV-cache behavior
- Live V0 `gpu_cache_usage_perc` behavior
- High-KV-cache pressure behavior; observed values were low (under 3%)
- Preemption behavior; `vllm:num_preemptions_total` was present but preemption was not deliberately exercised
- DCGM behavior on real GPU hardware
- Live Triton behavior
- Full Kubernetes discovery → scrape → recommendation integration
