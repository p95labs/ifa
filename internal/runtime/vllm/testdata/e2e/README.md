# vLLM E2E Networks NVIDIA L4 live captures

Verbatim `/metrics` payloads captured from a live `vllm/vllm-openai:v0.28.0`
container serving `facebook/opt-125m` on a real NVIDIA L4 GPU (E2E Networks).

---

## Provenance

| Item | Value |
|---|---|
| **GPU** | NVIDIA L4, 24570 MiB (≈24 GB) GDDR6 |
| **GPU UUID** | `GPU-be743051-a593-df24-9b65-c86c5e39e529` |
| **Driver** | 570.133.20 |
| **CUDA** | 12.8 |
| **Platform** | E2E Networks GPU node `p95labs-ifa-l4-validation`, Delhi NCR, Ubuntu 22.04.5 LTS |
| **vLLM image** | `vllm/vllm-openai:v0.28.0` (digest `sha256:61fc8a896b...`) |
| **Model** | `facebook/opt-125m` |
| **KV cache** | `--num-gpu-blocks-override 1024` (16 384 tokens; constrained to make exhaustion reachable) |
| **GPU memory utilization** | `--gpu-memory-utilization 0.8` |
| **Capture date** | 2026-10-23 |

---

## Load generation

```
python3 load_gen.py 80 90
```

- 80 concurrent worker threads, each issuing a new `/v1/completions` request
  immediately after the previous one completes.
- `ignore_eos=True`, `max_tokens=512`, `temperature=0.0`.
- Prompt: `"The quick brown fox jumps over the lazy dog. " * 8` (72 tokens).
- 2890 requests completed in 90 s with 0 errors (~32.1 req/s throughput).

---

## Captures

### `vllm_e2e_l4_idle.txt` — idle before load

Captured before the load generator started.

| Metric | Value |
|---|---|
| `num_requests_running` | 0 |
| `num_requests_waiting` | 0 |
| `kv_cache_usage_perc` | 0.0 |
| `num_preemptions_total` | 0 |
| `generation_tokens_total` | 0 |

### `vllm_e2e_l4_kv_exhausted.txt` — KV cache exhausted under load (~14 s)

Captured approximately 14 seconds after load start.

| Metric | Value |
|---|---|
| `num_requests_running` | 63 |
| `num_requests_waiting` | 17 |
| `num_requests_waiting_by_reason{reason="capacity"}` | 17 |
| `kv_cache_usage_perc` | **1.0 (100%)** |
| `num_preemptions_total` | 6 479 |
| `prompt_tokens_total` | 74 784 |
| `generation_tokens_total` | 446 445 |

### `vllm_e2e_l4_kv_exhausted_settled.txt` — KV cache exhausted, settled (~56 s)

Captured approximately 56 seconds into the run. Used as the "settled" reference.

| Metric | Value |
|---|---|
| `num_requests_running` | 73 |
| `num_requests_waiting` | 6 |
| `kv_cache_usage_perc` | **1.0 (100%)** |
| `num_preemptions_total` | 18 762 |
| `prompt_tokens_total` | 200 490 |

### `vllm_e2e_l4_recovery.txt` — recovery after load stopped

Captured 12 seconds after all load generator workers stopped.

| Metric | Value |
|---|---|
| `num_requests_running` | 0 |
| `num_requests_waiting` | 0 |
| `kv_cache_usage_perc` | 0.0 |
| `num_preemptions_total` | 23 033 (counter frozen — no new preemptions) |
| `generation_tokens_total` | 1 504 335 |

---

## Independently measured counter rates

Rates computed from consecutive raw snapshots (Δt = 14 s between each):

| Interval | Preemptions/s | Prompt tokens/s |
|---|---|---|
| S1 → S2 (t=14s → t=28s) | **285.9** | **2982.7** |
| S2 → S3 (t=28s → t=42s) | **297.2** | **2999.6** |
| S3 → S4 (t=42s → t=56s) | **294.3** | 2999.6 |
| S4 → S5 (t=56s → t=70s) | **278.5** | 2988.6 |

These rates confirm that `num_preemptions_total` was actively climbing at
~280–300 preemptions per second throughout the sustained load period, and that
`prompt_tokens_total` grew at ~3 000 tokens per second.

---

## Diagnostic rules validated

| Rule | Condition in captures | Result |
|---|---|---|
| `IFA-KV-001` | `kv_cache_usage_perc ≈ 1.0`, `num_preemptions_total` actively growing | **CONFIRMED** |
| `IFA-KV-002` | Suppressed by active preemptions during load | **CORRECTLY SUPPRESSED** |
| `IFA-CAP-001` | GPU util 100% (DCGM), waiting requests > 0 | **CONFIRMED** (DCGM side) |
| Recovery | `num_requests_running=0`, `waiting=0`, `kv_cache=0` within 12 s | **CONFIRMED** |

---

## Verbatim fixture policy

These files are stored verbatim and must not be normalized, trimmed,
reordered, or filtered. If a capture is regenerated, the provenance above
must be updated to reflect the new environment.
