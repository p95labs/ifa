# DCGM official exporter E2E Networks NVIDIA L4 captures

Verbatim `/metrics` payloads from NVIDIA's official `dcgm-exporter` container
image running on a real NVIDIA L4 GPU (E2E Networks). These captures close the
validation gap documented in `../README.md` — the existing fixtures used a
JarvisLabs-bundled DCGM-compatible exporter; these use the canonical NVIDIA
image.

---

## Provenance

| Item | Value |
|---|---|
| **GPU** | NVIDIA L4, 24570 MiB (≈24 GB) GDDR6 |
| **GPU UUID** | `GPU-be743051-a593-df24-9b65-c86c5e39e529` |
| **Driver** | 570.133.20 |
| **CUDA** | 12.8 |
| **Platform** | E2E Networks GPU node `p95labs-ifa-l4-validation`, Delhi NCR, Ubuntu 22.04.5 LTS |
| **Exporter image** | `nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04` (digest `sha256:3d4e0dfa5f...`) |
| **Run mode** | Privileged Docker container, `--net host`, embedded DCGM |
| **Exporter port** | 9400 |
| **Capture date** | 2026-10-23 |

The exporter was launched with:

```
docker run -d --name dcgm-exporter \
  --gpus all --privileged --net host \
  -v /run/dcgm:/run/dcgm \
  nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04
```

Startup log confirmed `DCGM successfully initialized!` before any captures
were taken.

---

## Captures

### `dcgm_official_e2e_l4_idle.txt` — idle GPU

Captured before the vLLM load generator started.

| Metric | Value |
|---|---|
| `DCGM_FI_DEV_GPU_UTIL` | **0%** |
| `DCGM_FI_DEV_MEM_COPY_UTIL` | 0% |
| `DCGM_FI_DEV_FB_USED` | 0 MiB |
| `DCGM_FI_DEV_FB_FREE` | 24 100 MiB |
| `DCGM_FI_DEV_GPU_TEMP` | 33°C |
| `DCGM_FI_DEV_SM_CLOCK` | 210 MHz |
| `DCGM_FI_DRIVER_VERSION` | `570.133.20` |

### `dcgm_official_e2e_l4_under_load.txt` — GPU under sustained inference load

Captured approximately 14 seconds into the 80-worker `facebook/opt-125m`
inference load. At this point the GPU was at 100% utilisation with the vLLM
KV cache fully exhausted (see `../../vllm/testdata/e2e/README.md`).

| Metric | Value |
|---|---|
| `DCGM_FI_DEV_GPU_UTIL` | **100%** |
| `DCGM_FI_DEV_MEM_COPY_UTIL` | 92% |
| `DCGM_FI_DEV_FB_USED` | **1 548 MiB** |
| `DCGM_FI_DEV_FB_FREE` | 22 552 MiB |
| `DCGM_FI_DEV_GPU_TEMP` | 46°C |
| `DCGM_FI_DRIVER_VERSION` | `570.133.20` |

Derived memory percentage: `1548 / (1548 + 22552) × 100 ≈ 6.42%`.

### `dcgm_official_e2e_l4_recovery.txt` — GPU after load stopped

Captured 12 seconds after the load generator finished.

| Metric | Value |
|---|---|
| `DCGM_FI_DEV_GPU_UTIL` | **0%** |
| `DCGM_FI_DEV_FB_USED` | 1 548 MiB (model weights remain loaded) |
| `DCGM_FI_DEV_GPU_TEMP` | 50°C (cooling from 58°C peak) |

---

## Key difference from the JarvisLabs captures

| Property | JarvisLabs (existing) | E2E Networks (this directory) |
|---|---|---|
| **Exporter** | Bundled `dcgm-exporter` binary (non-canonical) | Official `nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04` |
| **Driver** | 580.126.20 | 570.133.20 |
| **GPU UUID** | `GPU-eb752903-...` | `GPU-be743051-...` |
| **Refresh lag** | ~18–20 s measured | None observed — GPU_UTIL=100% within 14 s of load start |
| **Metric count** | 25 DCGM families | 21 DCGM families (default-counters.csv) |

The official exporter uses the default counter set
(`/etc/dcgm-exporter/default-counters.csv`). All fields the IFA DCGM adapter
reads (`GPU_UTIL`, `MEM_COPY_UTIL`, `FB_USED`, `FB_FREE`, `GPU_TEMP`,
`SM_CLOCK`) are present and correctly formatted.

No refresh lag was observed: the GPU utilisation reading reached 100% within
the first 14-second sampling window, unlike the ~18–20 s lag seen with the
JarvisLabs bundled exporter. This confirms the lag was a property of that
specific exporter, not of DCGM in general.

---

## Verbatim fixture policy

Same policy as the parent directory: stored verbatim, never normalized or
filtered. Provenance above must be updated if captures are regenerated.
