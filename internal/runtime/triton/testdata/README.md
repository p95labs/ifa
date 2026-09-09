# Triton adapter testdata

This directory contains synthetic payloads used in `triton_test.go` (inline
constants) and verbatim Prometheus payloads captured from a real Triton server.

---

## Synthetic fixtures

The synthetic payloads are embedded as string constants in `triton_test.go`
rather than stored as files. They cover the default (counter-only) configuration
and the optional summary-latency configuration with GPU metrics present.

---

## Live captures

Three files are verbatim `/metrics` responses captured from a real Triton
Inference Server running on CPU. They are used by `TestCapturedPayload` to
verify the adapter against authentic exposition.

### Provenance

- **Image:** `nvcr.io/nvidia/tritonserver:25.12-pyt-python-py3`
  (`sha256:40fd29c56c1b69b5b27514d781e0af52e757b16a3c97a5540e15820d2ddb451d`)
- **Version:** Triton 25.12 (NVIDIA Release 25.12)
- **Backend:** Python backend (`backend: "python"`), KIND_CPU instance group
- **Platform:** Linux ARM64 Docker VM (Apple Silicon, Docker Desktop)
- **Model:** `echo` — minimal passthrough that copies INPUT to OUTPUT unchanged
- **Baseline flags:** none (default metrics, no GPU present)
- **Summary-latencies capture additionally used:** `--metrics-config summary_latencies=true`
- **Requests sent for non-idle captures:** 200–280 sequential HTTP infer calls

### State details

**`triton_captured_idle.txt`** — server started, model loaded, zero requests sent

- `nv_inference_request_success{model="echo",version="1"} 0`
- `nv_inference_pending_request_count{model="echo",version="1"} 0`
- No `nv_gpu_*` metrics (CPU-only server, no GPU detected by Triton)
- 16 metric families, 0 unparseable lines
- `ifa check` reported: 5 required metrics found, 3 required missing (all GPU)

**`triton_captured_loaded.txt`** — post-traffic idle (280 sequential requests completed)

- `nv_inference_request_success{model="echo",version="1"} 280`
- `nv_inference_request_duration_us{model="echo",version="1"} 121428`
- `nv_inference_queue_duration_us{model="echo",version="1"} 8417`
- All failure reason counters = 0
- `nv_inference_pending_request_count` = 0 (echo completes before next poll)
- No `nv_gpu_*` metrics

**`triton_captured_summary.txt`** — started with `--metrics-config summary_latencies=true`, 200 sequential requests

- `nv_inference_request_success{model="echo",version="1"} 200`
- `nv_inference_request_summary_us{...,quantile="0.5"}` = 504 µs → 0.504 ms
- `nv_inference_request_summary_us{...,quantile="0.95"}` = 921 µs → 0.921 ms
- `nv_inference_request_summary_us{...,quantile="0.99"}` = 1156 µs → 1.156 ms
- `nv_inference_queue_summary_us{...,quantile="0.95"}` = 102 µs → 0.102 ms
- 31 metric families, 0 unparseable lines
- No `nv_gpu_*` metrics

### GPU metrics on a CPU server

Triton emits `nv_gpu_utilization`, `nv_gpu_memory_used_bytes`, and
`nv_gpu_memory_total_bytes` only when NVIDIA GPU devices are detected at
startup. On a CPU-only host these three families are completely absent from the
payload. The adapter maps absence to an unmeasured `Metric` (`OK == false`),
not to zero. All three captures confirm this: `GPUUtilizationPct.OK == false`
and `GPUMemoryUsedPct.OK == false`.

`ifa check` reports these as `MISSING` (not zero), which means GPU-dependent
rules do not fire on CPU deployments.

### Pending-request count limitation

The `echo` model completes requests in under 1 ms. The
`nv_inference_pending_request_count` gauge never rises above 0 between the poll
interval and the infer call. A non-zero pending count would require either a
slow model (deliberate `time.sleep` in the Python backend) or a dynamic-batcher
configuration that holds requests. Neither was exercised; the gauge value of 0
across all three captures is the expected result for a trivially fast model.

### Verbatim fixture policy

The live capture files are stored verbatim and must not be normalized, trimmed,
reordered, filtered, or otherwise modified. They represent the authentic shape
of a real Triton `/metrics` response, including metrics the adapter ignores
(`nv_cpu_utilization`, `nv_model_load_duration_secs`, `nv_pinned_memory_pool_*`,
and the `nv_inference_compute_*_summary_us` families when summary latencies are
enabled).

If a capture must be regenerated the replacement must be another raw `/metrics`
response and the provenance above must be updated.

---

## Validation scope

These captures validate the Triton adapter against a real CPU-backed
Triton 25.12 server. They do not establish:

- GPU-backed Triton behavior (`nv_gpu_*` path not exercised on live hardware)
- The `nv_gpu_utilization` ×100 conversion correctness (no GPU present to verify)
- Non-zero pending request counts under real queue pressure
- Multi-model label isolation on a live server (only one model loaded)
- Dynamic-batcher behavior
- TensorRT, PyTorch, or ONNX backend behavior
- Full Kubernetes discovery → scrape → recommendation integration
