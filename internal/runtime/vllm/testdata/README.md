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

## GPU-backed live capture

### Provenance

- **GPU:** NVIDIA L4, 23034 MiB total memory
- **Driver:** 580.126.20
- **vLLM image:** `vllm/vllm-openai:v0.28.0`
- **Model:** `facebook/opt-125m`
- **Flags:** `--dtype half --gpu-memory-utilization 0.8 --max-model-len 2048 --num-gpu-blocks-override 1024`
  (KV cache deliberately constrained to 16,384 tokens / 1024 blocks to make
  exhaustion reachable without exotic prompt lengths)
- **Platform:** JarvisLabs.ai GPU VM, Ubuntu 22.04.5, CUDA 13.0
- **Load generator:** a continuous-refill script -- CONCURRENCY worker threads,
  each immediately issuing a new request as soon as its previous one
  completes, for a fixed wall-clock DURATION. This differs from a one-shot
  batch of N requests: a batch drains as requests finish, while continuous
  refill sustains pressure for as long as DURATION lasts. Prompt: a fixed
  223-token prompt (verified via vLLM's `/tokenize` endpoint) repeated to
  build up input length; `max_tokens` fixed per run; `ignore_eos: true` so
  every request runs to its full `max_tokens` rather than stopping early.
  The specific run that produced this capture used 100 concurrent workers,
  `max_tokens=400`, captured 13 seconds into the run.

### State details

**`vllm_gpu_l4_kv_exhausted.txt`** -- real KV-cache exhaustion under sustained
concurrent load

- `num_requests_running = 84`
- `num_requests_waiting = 16`
- `num_requests_waiting_by_reason{reason="capacity"} = 16`
- `num_requests_waiting_by_reason{reason="deferred"} = 0`
- raw `kv_cache_usage_perc = 0.9990224828934506` (approximately 99.9%)
- `num_preemptions_total = 53366.0` (a cumulative counter; nonzero and
  actively climbing across the session that produced this capture, which is
  the signal that matters -- the absolute value includes preemptions from
  earlier runs in the same long-lived vLLM process)

This is the first vLLM capture in this repository showing genuine KV-cache
saturation with capacity-driven queuing and active preemption together in one
payload, captured against real GPU hardware rather than synthesized.

### Checksum

```
2a958c77963a64225737276b2411e473  vllm_gpu_l4_kv_exhausted.txt
```

## Validation scope

These CPU captures validate the vLLM adapter against a real CPU-backed vLLM
0.28.0 server. The GPU-backed capture above additionally validates real
KV-cache exhaustion, capacity-driven queuing, and active preemption end to
end. Together they do not establish:

- Real device-side GPU KV-cache behavior beyond the single exhaustion capture
  above (e.g. sustained exhaustion over long time windows, recovery behavior
  after load drops, multi-GPU KV-cache pooling)
- Live V0 `gpu_cache_usage_perc` behavior
- DCGM behavior on the official NVIDIA `dcgm-exporter` image (this session's
  DCGM capture, in `internal/runtime/dcgm/testdata/`, used a JarvisLabs-bundled
  DCGM-compatible exporter -- see that directory's README for the distinction)
- Live Triton behavior
- Full Kubernetes discovery to scrape to recommendation integration
