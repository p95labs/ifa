# DCGM adapter testdata

This directory contains verbatim `/metrics` payloads captured from a real
DCGM-compatible exporter running against physical GPU hardware, alongside the
synthetic fixtures already present in `dcgm_test.go`.

---

## Live captures

### Provenance

- **GPU:** NVIDIA L4, 23034 MiB total memory
- **Driver:** 580.126.20
- **Exporter:** `/opt/jarvis/guest-monitor/bin/dcgm-exporter -a 127.0.0.1:9400 -r 127.0.0.1:5555`
  (JarvisLabs' bundled DCGM-compatible exporter — **not** the official NVIDIA
  `dcgm-exporter` container image. It exposes 25 `DCGM_FI_DEV_*` series, a
  narrower set than the canonical image, but the fields the adapter reads
  (`GPU_UTIL`, `MEM_COPY_UTIL`, `FB_USED`, `FB_FREE`) are present and correctly
  formatted.)
- **Platform:** JarvisLabs.ai GPU VM, Ubuntu 22.04.5, CUDA 13.0
- **Workload generating GPU activity:** vLLM 0.28.0 serving `facebook/opt-125m`
  (see `internal/runtime/vllm/testdata/README.md` for the vLLM-side provenance
  of the same session)

### State details

**`dcgm_l4_under_load_settled.txt`** — GPU under sustained real load, exporter
settled

- `DCGM_FI_DEV_GPU_UTIL{gpu="0",...} = 100`
- `DCGM_FI_DEV_FB_USED{gpu="0",...} = 1550` (MiB)
- Captured while a 60-concurrency, `ignore_eos=true` completion workload had
  been running continuously for at least 20 seconds.

**`dcgm_l4_refresh_lag_stale.txt`** — GPU under identical real load, exporter
not yet settled

- `DCGM_FI_DEV_GPU_UTIL{gpu="0",...} = 0`
- `DCGM_FI_DEV_FB_USED{gpu="0",...} = 1548` (MiB - in the same ~1.5 GiB
  range as the settled capture, confirming the GPU itself was active in both;
  only the utilisation reading was stale)
- Captured under the same load pattern as the settled file above, but sampled
  too soon after load start.

These two files are a matched pair and are kept side by side deliberately: the
adapter's job is to parse whatever the exporter currently reports, correctly,
in both cases. The difference between them is not a parsing bug — it is a
measured characteristic of this specific exporter, documented below.

### The exporter's refresh lag

Repeated measurement (dense polling — see reproduction recipe below) showed
this exporter takes approximately **18-20 seconds** from a real GPU state
change (idle to 100% utilised) to reflect that change in `DCGM_FI_DEV_GPU_UTIL`.
Once settled, the reading was stable across 127 consecutive samples taken at
roughly 0.8s intervals during continued load, with no flicker back to a stale
value.

This means: an observation window shorter than the exporter's own refresh lag
will show `gpu_utilization_percent = 0` even while the GPU is genuinely
saturated. This is a property of this exporter, not of IFA's parser or
collector — confirmed by reading the raw `/metrics` endpoint directly,
independent of IFA, at the same timestamps. Any deployment against a similarly
lagged DCGM source needs `collector.interval` and any test/observation window
sized comfortably above the source's own refresh lag, or GPU rules will
under-report transiently after a load change.

### Reproduction recipe

1. Start vLLM serving a small model (e.g. `facebook/opt-125m`) with GPU
   inference enabled.
2. Confirm a DCGM-compatible exporter is reachable and reporting the target
   GPU (`curl <dcgm-url>/metrics | grep DCGM_FI_DEV_GPU_UTIL`).
3. Drive continuous concurrent load against vLLM's `/v1/completions` endpoint
   — e.g. 60 workers, each looping requests with `ignore_eos: true` and a
   fixed `max_tokens`, for at least 60 seconds continuously (see
   `internal/runtime/vllm/testdata/README.md` for the exact load-generation
   approach used for this session's vLLM-side captures).
4. Sample the DCGM endpoint's raw `/metrics` output at short, even intervals
   (e.g. every 0.5-1s) throughout. Expect an initial run of stale/idle-looking
   samples before the exporter catches up — this is the lag being measured,
   not a failed test.
5. Capture one raw payload from early in the run (stale) and one from at
   least ~20 seconds into sustained load (settled) for the matched-pair
   fixtures.

### Checksums

```
cab12ff2c0b25619242af629086c6673  dcgm_l4_refresh_lag_stale.txt
89118f15ceacfb6206778e31d2d964a6  dcgm_l4_under_load_settled.txt
```

### Verbatim fixture policy

Same policy as the vLLM adapter's captures: these files are stored verbatim
and must not be normalized, trimmed, reordered, or filtered. If a capture must
be regenerated, the replacement must be another raw `/metrics` response, and
the provenance above must be updated to reflect the new environment.

---

## Validation scope

These captures validate the DCGM adapter's parsing (`Parse()`) and the
collector's overlay logic (`applyGPU()`) against a real, physical NVIDIA L4
GPU and a real (if non-canonical) DCGM-compatible exporter. They do not
establish:

- Multi-GPU aggregation on real hardware (only one physical GPU was available)
- Behavior against the official NVIDIA `dcgm-exporter` container image
- GPU temperature or SM clock field behavior under real thermal load
- Any exporter's refresh characteristics other than the one measured here -
  the ~18-20s lag is specific to this exporter and must not be assumed to
  generalize to other DCGM sources
