# Roadmap

Status language is deliberate. "Implemented" means the code exists and is unit
tested. "Validated" means it has been run against the real thing. Several
components have now earned that word; several have not, and the difference is
the single most important fact on this page.

| Term | Means |
|---|---|
| **Implemented** | Code exists, has tests, and is exercised by CI |
| **Locally validated** | Additionally exercised end to end against a simulated system |
| **Integration validated** | Additionally run against a real server or cluster |
| **Experimental** | Exists, but the design may change |
| **Planned** | Not started |

## Where things stand

| Area | Status |
|---|---|
| Prometheus exposition parsing — labels, histograms, summaries, edge cases | Locally validated |
| Rule engine — 19 rules, suppression, sustained conditions | Locally validated |
| vLLM adapter | Integration validated — vLLM 0.28.0 on a CPU backend (`facebook/opt-125m`) and on one NVIDIA L4 under KV-cache exhaustion |
| Triton adapter | Integration validated — CPU backend, Triton 25.12, Python echo model; GPU-backed Triton not yet validated |
| DCGM adapter | Integration validated — one NVIDIA L4 via a DCGM-*compatible* exporter; NVIDIA's own `dcgm-exporter` image not yet validated |
| HTTP API, `/api/v1` and legacy paths | Integration validated — exercised over in-cluster networking by the kind test |
| `ifa` CLI including `ifa check` | Integration validated — `ifa check` ran against live vLLM and Triton `/metrics` endpoints |
| Self-metrics | Integration validated — `ifa_scrapes_total` asserted in the kind test |
| Kubernetes discovery via informers | Integration validated — real API server in a kind cluster in CI |
| Helm chart, RBAC, NetworkPolicy, security contexts | Integration validated for the default install path; NetworkPolicy itself is rendered and schema-checked, never enforced in CI |
| Webhook alerting | Implemented |
| TimescaleDB history | Implemented |
| Counter-rate and queue-growth rules | Integration validated — exercised through actual collector scrapes in the dynamic kind integration test; see below |
| Demo and its end-to-end test | Locally validated |

"Locally validated" for the parser, rules and demo means `make demo` and
`TestDemoScenariosProduceTheirIntendedDiagnosis` run the whole pipeline over a
real socket against vLLM-shaped exposition, and each simulated failure mode
produces its intended diagnosis.

The vLLM adapter has been validated against a live CPU-backed server and against
one NVIDIA L4 (see below). The Triton adapter has been validated against a live
CPU-backed server running Triton 25.12. The DCGM adapter has been validated
against a DCGM-compatible exporter on the same L4. The kind integration test runs
end to end in CI against a real cluster and is a required branch-protection check.

## Next, in order

### Done: live vLLM validation (CPU backend)

The vLLM adapter has been run against a real server:

- vLLM 0.28.0, official ARM64 CPU Docker image
- Model: `facebook/opt-125m`
- `ifa check` parsed the live `/metrics` endpoint; all required metrics were
  present, zero unparseable lines
- Fixtures captured in three states: idle, loaded, and capacity-queued
- The capacity-queued state was produced with `--max-num-seqs 2`; the capture
  shows non-zero `num_requests_waiting` and
  `num_requests_waiting_by_reason{reason="capacity"}`
- `TestCapturedPayload` passes against the captured fixture

This run validates the vLLM adapter against a real CPU-backed server only. The
highest KV-cache occupancy reachable on CPU was under 3%, so it establishes
nothing about KV-cache pressure. The GPU-backed run below does.

### Done: GPU-backed vLLM and DCGM validation (one NVIDIA L4)

The vLLM adapter has been run against GPU hardware, and a DCGM-compatible
exporter was captured from the same session:

- NVIDIA L4 (23034 MiB), driver 580.126.20, CUDA 13.0, Ubuntu 22.04.5, on a
  JarvisLabs.ai GPU VM
- `vllm/vllm-openai:v0.28.0` serving `facebook/opt-125m`, with the KV cache
  deliberately constrained (`--num-gpu-blocks-override 1024`) so that exhaustion
  is reachable without exotic prompt lengths
- Load was applied by continuous refill — 100 worker threads each issuing a new
  request the moment the previous one completed, `ignore_eos: true` — rather
  than a one-shot batch, so pressure was sustained rather than draining
- The capture shows `kv_cache_usage_perc ≈ 0.999` (**approximately 99.9%**), 84
  requests running, 16 waiting, all 16 labelled
  `reason="capacity"`, and `num_preemptions_total` nonzero and actively climbing
- GPU utilisation was captured separately at `DCGM_FI_DEV_GPU_UTIL = 100`
- Both captures are committed verbatim as regression fixtures and asserted by
  `TestCapturedPayload` (vLLM) and the DCGM adapter tests

**What this does not establish.** One L4 is one card: it says nothing about
multi-GPU behaviour, multi-GPU KV-cache pooling, or GPU aggregation on real
hardware. The preemption *counter* was observed active; a preemption *rate*
needs two time-separated scrapes and was not measured. And the exporter used was
JarvisLabs' bundled DCGM-compatible exporter — **not** NVIDIA's official
`dcgm-exporter` container image, which remains unvalidated. That exporter was
also measured lagging real GPU state by 18–20 seconds; see
[`internal/runtime/dcgm/testdata/README.md`](../internal/runtime/dcgm/testdata/README.md).

GPU-backed Triton remains unvalidated.

### Done: kind integration test in CI

The chart is installed into a kind cluster in every CI run. A CPU-mode busybox
target stands in for an inference workload; IFA discovers it via the informer,
scrapes it, and the test asserts that the expected finding appears. This
caught a real bug: `inference.io/model` was stored as a Kubernetes label, and
label values cannot contain `/`, which every Hugging Face model ID does. Moving
it to an annotation (where values are unrestricted) fixed it — a real API server
found what a fake clientset had silently accepted.

### Done: live Triton validation (CPU backend)

The Triton adapter was run against a real Triton 25.12 server:

- `nvcr.io/nvidia/tritonserver:25.12-pyt-python-py3`, ARM64 Docker VM
- Model: `echo` (Python backend, CPU instance group)
- `ifa check` parsed the live `/metrics` endpoint; zero unparseable lines
- Three captures: idle, post-traffic (280 requests), and summary-latencies-enabled
- CPU-only server emits no `nv_gpu_*` families; the adapter correctly maps
  their absence to `OK == false` — rules depending on GPU metrics do not fire
- `TestCapturedPayload` passes against all three captured fixtures

GPU-backed Triton remains unvalidated. GPU-backed vLLM and a DCGM-compatible
exporter have since been validated on one NVIDIA L4; see above.

### Done: dynamic multi-scrape validation in CI

The counter-rate and queue-growth rules are now validated end-to-end through
actual successive IFA collector scrapes against a deterministic synthetic server
(`dynserver`, built from `test/integration/dynserver/cmd/`) deployed in a kind
cluster. The test runs in CI (`kind-dynamic-integration` job) on every push and
pull request to `main`.

Six scenarios are validated, each through the full pipeline:
Kubernetes workload → HTTP scrape → runtime adapter → rate tracker → telemetry
store → rule engine → REST API:

| Scenario | Rule validated | What is checked |
|---|---|---|
| A — Counter-to-rate | (collector layer) | First observation → unmeasured; counter growth → positive rates within tolerance; zero delta → zero rate |
| B — Growing queue | `IFA-CAP-003` | Rule fires only after `minSamples=6` across the trend window; evidence includes `queue_trend`; deep stable queue does not fire |
| C — KV preemption | `IFA-KV-001` / `IFA-KV-002` | High KV without preemption delta → IFA-KV-001 absent; positive delta → IFA-KV-001 fires and suppresses IFA-KV-002 |
| D — Low throughput | `IFA-EFF-002` | Synthetic DCGM GPU metrics (92% util) + low token rate → rule fires; GPU unmeasured (no DCGM URL) → absent; throughput above threshold → absent |
| E — Counter reset | (collector layer) | Decreasing counter → interval unmeasured; new baseline; subsequent increments yield correct rates |
| F — Missing telemetry | `IFA-OBS-001` path | HTTP 503 → no snapshot written; unrecognised exposition → `metrics_missing` populated; recovery → healthy telemetry resumes |

**What this does and does not prove.** The dynserver generates synthetic
vLLM-compatible Prometheus exposition; it is not a real vLLM process. The
counter values, queue depths, and KV-cache percentages are deterministic and
chosen to exercise rule thresholds, not measured from a GPU workload. The
synthetic DCGM endpoint reports a fixed GPU utilisation of 92%; it does not
reflect physical GPU state. All payloads are labelled `synthetic_source=dynserver-integration-test`.

These tests prove that the IFA *pipeline* (scraping → rate tracking → rule
evaluation → API) correctly handles changing telemetry, counter resets, and
missing data. They do not prove that the rules fire correctly against a real
inference workload, which requires a live server and remains a gap.

### Done: NVIDIA official dcgm-exporter and second NVIDIA L4 session (E2E Networks)

NVIDIA's official `dcgm-exporter` container image has been validated on a real
NVIDIA L4:

- E2E Networks GPU node (`p95labs-ifa-l4-validation`), Delhi NCR
- `nvcr.io/nvidia/k8s/dcgm-exporter:3.3.9-3.6.1-ubuntu22.04` (embedded DCGM,
  Docker `--gpus all --privileged`)
- Driver 570.133.20, CUDA 12.8, Ubuntu 22.04.5
- `DCGM successfully initialized!` on startup; all six fields the adapter reads
  are present and correctly formatted in the `default-counters.csv` field set
- Idle capture: `GPU_UTIL=0%`, `FB_USED=0 MiB`, `GPU_TEMP=33°C`
- Under sustained load (80-worker `facebook/opt-125m` KV-exhaustion run):
  `GPU_UTIL=100%`, `FB_USED=1548 MiB`, `GPU_TEMP=46→58°C`
- No refresh lag observed — 100% GPU_UTIL within the first 14 s sampling window,
  confirming the ~18–20 s lag in the earlier session was specific to the
  JarvisLabs bundled exporter, not DCGM in general
- Three verbatim captures committed: idle, under-load, recovery

A second vLLM GPU session was run in the same E2E environment:

- 80 workers, `ignore_eos=True`, `--num-gpu-blocks-override 1024`
- 2 890 requests completed in 90 s, 0 errors
- KV cache reached 100% within 14 s; `num_preemptions_total` grew at
  ~280–300 preemptions/s throughout
- Independently measured counter rates confirm the collector's rate arithmetic
  (see `internal/runtime/vllm/testdata/e2e/README.md`)
- Recovery confirmed: `num_requests_running=0`, `waiting=0`, `kv_cache=0`
  within 12 s of load stopping
- Four verbatim captures committed: idle, KV-exhausted, settled, recovery

### 1. Live dynamic validation with a real inference server

### 2. Per-pod GPU attribution

A DCGM Exporter endpoint reports the GPUs on a *node*, not the GPUs belonging to
one pod. On a node running several inference workloads, the utilisation IFA
attributes to a target may belong to something else. DCGM's Kubernetes-aware
labels carry pod and namespace; using them needs a mapping from workload to pods
that the informer cache already has.

Until this lands, GPU findings are trustworthy on dedicated GPU nodes and
approximate on shared ones. The docs say so; the API does not yet.

### 3. GPU-backed Triton

Triton has been validated on CPU only, where it publishes no `nv_gpu_*` families
at all, so the `nv_gpu_utilization` ×100 conversion has never been checked
against a real reading. If you run GPU-backed Triton, `ifa check <url>` produces
the report that would close this gap. NVIDIA's official `dcgm-exporter` has now
been validated separately (see above).

### 4. Per-workload thresholds

Thresholds are global. A batch scoring pipeline and an interactive chat endpoint
have genuinely different definitions of "slow", and sharing one set means either
the batch workload is permanently on fire or the chat endpoint's real problems
are under the line. A per-target override block, falling back to the global set,
is the obvious shape.

### 5. A finding lifecycle

Findings are stateless: each request re-evaluates from scratch. IDs are stable
while a condition holds, which is enough to deduplicate, but there is no
first-seen timestamp, no flap suppression, and no way to acknowledge one. Any of
those would need durable state and should not be built before someone actually
wants it.

## Considered and not planned

**Automatic remediation.** IFA holds no write permission and will not gain one.
See [ADR 0001](adr/0001-read-only.md).

**A built-in dashboard.** Grafana exists and is better at this. A Grafana
dashboard JSON that reads IFA's API would be a welcome contribution; a
hand-rolled UI in this repository would not be maintained.

**Machine-learned anomaly detection.** [ADR 0004](adr/0004-deterministic-rules.md).
There is nothing credible to train on, and an unexplainable finding is not
actionable at 3am.

**More runtimes for their own sake.** An Ollama adapter existed here and was
removed: its only route to throughput numbers was to treat per-request response
statistics as cumulative counters, which produces numbers that look plausible
and mean nothing. One adapter that is right beats three that are shallow.
SGLang and TGI both expose Prometheus metrics and would be genuine additions —
from someone who runs them and can check the output.

## Toward v1.0

Not close. GPU-backed vLLM validation, real-cluster validation, and dynamic
multi-scrape validation of the counter-rate rules are now done. What remains
would need, at minimum: live dynamic validation against a real inference server
under changing load; GPU-backed Triton; authentication on the API; per-workload
thresholds; signed images and an SBOM; and a stable API used by somebody other
than the author.
