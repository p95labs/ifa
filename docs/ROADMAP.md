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
| Counter-rate and queue-growth rules | Implemented — never evaluated against live time-separated deltas |
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

### 1. Dynamic, multi-scrape validation

The largest remaining gap. Every live capture so far is a *snapshot*, and the
kind integration test serves one static payload on every scrape, so every
counter delta is zero. That leaves an entire class of rules evaluated only
against synthetic data:

- `IFA-CAP-003` (queue growing) — needs a positive queue-depth trend across samples
- `IFA-KV-001` (KV preemption) — needs a `preemptions_per_sec` rate, not a counter value
- `IFA-EFF-002` (low token throughput) — needs `generation_tokens_total` deltas
- Counter-reset handling on a real pod restart

Closing it means capturing a *sequence* of time-separated payloads from a live
server under changing load, and serving them in order to the collector, rather
than one payload repeatedly. The GPU session that produced the KV-exhaustion
capture is the natural source: the preemption counter was observed climbing, so
the rate exists in the data and has simply never been measured across two scrapes.

### 2. Per-pod GPU attribution

A DCGM Exporter endpoint reports the GPUs on a *node*, not the GPUs belonging to
one pod. On a node running several inference workloads, the utilisation IFA
attributes to a target may belong to something else. DCGM's Kubernetes-aware
labels carry pod and namespace; using them needs a mapping from workload to pods
that the informer cache already has.

Until this lands, GPU findings are trustworthy on dedicated GPU nodes and
approximate on shared ones. The docs say so; the API does not yet.

### 3. GPU-backed Triton, and NVIDIA's own dcgm-exporter

Two named gaps that a single GPU session would close. Triton has been validated
on CPU only, where it publishes no `nv_gpu_*` families at all, so the
`nv_gpu_utilization` ×100 conversion has never been checked against a real
reading. And the DCGM validation used a DCGM-*compatible* exporter bundled by
the GPU host, not NVIDIA's official `dcgm-exporter` image — a narrower field set
that happened to include every field the adapter reads. If you run either,
`ifa check <url>` produces the report that would close it.

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

Not close. GPU-backed vLLM validation and real-cluster validation are now done;
what remains would need, at minimum: dynamic multi-scrape validation of the
counter-rate rules; GPU-backed Triton; authentication on the API; per-workload
thresholds; signed images and an SBOM; and a stable API used by somebody other
than the author.
