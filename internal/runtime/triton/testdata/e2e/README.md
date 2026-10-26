# Triton GPU E2E testdata — NVIDIA L4 (E2E Networks)

This directory contains verbatim `/metrics` responses captured from a real
NVIDIA Triton Inference Server 25.12 running a GPU-backed model on an
NVIDIA L4. They are the first captures to exercise the `nv_gpu_utilization`,
`nv_gpu_memory_used_bytes`, and `nv_gpu_memory_total_bytes` paths in the
Triton adapter against genuine GPU hardware.

---

## Hardware and software environment

| Item | Value |
|---|---|
| **GPU** | NVIDIA L4, 24 GB GDDR6 (23034 MiB usable) |
| **GPU UUID** | `GPU-6e8f8724-3d7e-c654-de2b-1ffcf4487a6f` |
| **PCI bus** | `00000000:01:01.0` |
| **Driver** | 570.133.20 |
| **CUDA** | 12.8 (forward-compat mode with driver 570 + CUDA 13.1 container) |
| **Platform** | E2E Networks GPU node (`p95labs-ifa-l4-v2`), Delhi NCR |
| **OS** | Ubuntu 22.04.5 LTS |
| **Triton image** | `nvcr.io/nvidia/tritonserver:25.12-pyt-python-py3` |
| **Backend** | `pytorch` (LibTorch) |
| **Model** | `gpu_matmul` — 20 sequential 128×128 FP32 matrix multiplications |
| **Instance group** | `KIND_GPU`, count=1, device 0 |
| **Dynamic batching** | `preferred_batch_size: [4, 8]`, `max_queue_delay_microseconds: 5000` |
| **Metrics flags** | `--allow-metrics=true --metrics-config summary_latencies=true` |

### Model: gpu_matmul

`gpu_matmul` is a TorchScript model (`jit.script`) that performs 20 sequential
128×128 FP32 matrix multiplications on CUDA device 0. The `KIND_GPU` instance
group forces Triton to assign it to the GPU; there is no CPU fallback. The
model loads onto CUDA at Triton startup, which is why `nv_gpu_memory_used_bytes`
is non-zero even at idle (the model weights and CUDA memory pool occupy ~290 MiB).

The model was exported with `torch 2.14.1+cpu` on the node and served by
LibTorch (Triton's PyTorch backend), which uses the GPU at runtime. This is
confirmed by:
- `TRITONBACKEND_ModelInstanceInitialize: gpu_matmul_0_0 (GPU device 0)` in
  the Triton startup log.
- `nv_gpu_memory_used_bytes` rising from 270 MiB (idle) to 292 MiB (under load).
- `nv_gpu_utilization` rising from 0 to 0.02 under 40 concurrent workers.
- DCGM exporter independently reporting `DCGM_FI_DEV_GPU_UTIL = 2` and
  temperature rising from 38°C to 47°C under load.

The model's individual computations complete quickly (p50 ≈ 91 ms end-to-end,
of which ~64 ms is queue time). GPU utilization therefore stays low (2%) even
under 40 workers: the L4 executes each batch in microseconds but the Python
HTTP round-trip dominates latency.

---

## Captures

### `triton_gpu_l4_idle.txt`

Server started, `gpu_matmul` model loaded on GPU, zero requests sent.

- `nv_inference_request_success{model="gpu_matmul"} 0`
- `nv_inference_pending_request_count{model="gpu_matmul"} 0`
- `nv_gpu_utilization = 0` (fraction → `GPUUtilizationPct = 0 %`)
- `nv_gpu_memory_total_bytes = 24152899584` (24 GB L4)
- `nv_gpu_memory_used_bytes = 270532608` (~258 MiB — model loaded on GPU)
- `GPUMemoryUsedPct ≈ 1.12 %`
- All 10 expected metrics present; 0 missing; 0 unparseable lines
- Summary latency families absent (not enabled in Triton flags)

### `triton_gpu_l4_under_load.txt`

40 concurrent workers, 90 s duration, dynamic batching active.
Captured ≈ 38 s into the run.

- `nv_inference_request_success{model="gpu_matmul"} 30637`
- `nv_inference_pending_request_count{model="gpu_matmul"} 30`
  (dynamic batching is holding 30 requests waiting for a preferred batch)
- `nv_gpu_utilization = 0.02` (fraction) → `GPUUtilizationPct = 2 %`
  **This is the key data point for the `×100` conversion test.**
  The raw metric is 0.02, not 2; misinterpreting it as 2% already would require
  treating it as a percentage directly. The adapter multiplies by 100, giving 2 %.
  DCGM independently reported `DCGM_FI_DEV_GPU_UTIL = 2` at the same time,
  corroborating the correct interpretation.
- `nv_gpu_memory_used_bytes = 306184192` (~292 MiB, +22 MiB from idle)
- Summary latencies (all per `gpu_matmul`):
  - `nv_inference_request_summary_us{quantile="0.5"} 91127` → `P50 = 91.127 ms`
  - `nv_inference_request_summary_us{quantile="0.95"} 109233` → `P95 = 109.233 ms`
  - `nv_inference_request_summary_us{quantile="0.99"} 117378` → `P99 = 117.378 ms`
  - `nv_inference_queue_summary_us{quantile="0.95"} 78308` → `QueueP95 = 78.308 ms`
- 0 unparseable lines; 0 missing metrics

### `triton_gpu_l4_recovery.txt`

All workers stopped; captured ~10 s after the 90 s run completed.

- `nv_inference_request_success{model="gpu_matmul"} 44434` (total for the run)
- `nv_inference_pending_request_count{model="gpu_matmul"} 0`
- `nv_gpu_utilization = 0` → `GPUUtilizationPct = 0 %`
- GPU memory still shows model loaded (~292 MiB); it does not change post-run
- DCGM independently reported `DCGM_FI_DEV_GPU_UTIL = 0`, temperature still 47°C

---

## GPU utilization conversion validation

The `nv_gpu_utilization` metric is documented by NVIDIA as a rate in `[0.0, 1.0)`.
Treating it as a percentage makes a 2%-loaded GPU appear 200× more idle than it is
in the opposite direction, but more importantly makes Triton-reported utilisation
inconsistent with DCGM, which reports percentages directly.

This session confirms:
- Triton reports `nv_gpu_utilization = 0.02`.
- DCGM reports `DCGM_FI_DEV_GPU_UTIL = 2` at the same time.
- IFA's adapter multiplies by 100: `0.02 × 100 = 2 %`.
- The `TestGPUUtilisationIsConvertedFromRateToPercent` unit test (synthetic,
  value 0.91) and `TestCapturedPayloadGPU/e2e-gpu-under-load` (live, value 0.02)
  together guard this conversion.

---

## Load workload

```
workers:  40 concurrent HTTP threads
duration: 90 seconds
endpoint: http://127.0.0.1:8000/v2/models/gpu_matmul/infer
payload:  {"inputs":[{"name":"INPUT__0","shape":[1,128,128],"datatype":"FP32",
                      "data":[0.5, ...×16384]}]}
result:   44,432 successful requests, 0 errors
```

---

## Validation scope

These captures establish:
- `nv_gpu_utilization` fraction-to-percentage conversion is correct on live GPU
- `nv_gpu_memory_used_bytes` / `nv_gpu_memory_total_bytes` path returns correct percentage
- `nv_inference_pending_request_count` rises to non-zero under dynamic batching
- Summary latency percentiles (p50, p95, p99, queue_p95) parsed correctly
- All 10 expected adapter metrics present on a GPU-backed server
- Recovery: pending returns to 0 and GPU utilization returns to 0 after load stops

They do not establish:
- GPU-backed Triton behavior with TensorRT or ONNX Runtime backends
- Multi-GPU aggregation (`nv_gpu_utilization` max across devices)
- High GPU utilization (>85%) saturating the L4 with this lightweight model
- Multi-model label isolation on a live GPU server (single model loaded)
- Interaction with IFA diagnostic rules under real Triton GPU conditions
  (requires the full Kubernetes E2E path — see Phase 3 of the mission)

---

## Verbatim fixture policy

These files are stored verbatim and must not be normalized, trimmed, reordered,
filtered, or otherwise modified. They represent the authentic shape of a real
Triton 25.12 `/metrics` response with GPU hardware present, including all metric
families the adapter ignores.

If a capture must be regenerated, the replacement must be another raw `/metrics`
response from a GPU-backed Triton instance, and the provenance above must be updated.
