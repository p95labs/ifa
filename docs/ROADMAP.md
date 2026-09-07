# Roadmap

Status language is deliberate. "Implemented" means the code exists and is unit
tested. "Validated" means it has been run against the real thing — and almost
nothing here has earned that word yet, which is the single most important fact
on this page.

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
| vLLM adapter | Live-server validated — CPU backend, vLLM 0.28.0, `facebook/opt-125m`; GPU-backed vLLM not yet validated |
| Triton adapter | Implemented |
| DCGM adapter | Implemented |
| HTTP API, `/api/v1` and legacy paths | Implemented |
| `ifa` CLI including `ifa check` | Implemented |
| Self-metrics | Implemented |
| Kubernetes discovery via informers | Implemented |
| Helm chart, RBAC, NetworkPolicy, security contexts | Implemented |
| TimescaleDB history | Implemented |
| Demo and its end-to-end test | Locally validated |

"Locally validated" for the parser, rules and demo means `make demo` and
`TestDemoScenariosProduceTheirIntendedDiagnosis` run the whole pipeline over a
real socket against vLLM-shaped exposition, and each simulated failure mode
produces its intended diagnosis.

The vLLM adapter has been validated against a live CPU-backed server (see below). Full Kubernetes integration validation remains open.

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

This validates the vLLM adapter against a real CPU-backed server only. GPU-backed
vLLM, real DCGM hardware, live Triton, and the Kubernetes discovery → scrape →
recommendation path all remain unvalidated.

### 1. A kind-based integration test in CI

Install the chart into a kind cluster, run a small CPU-mode vLLM, assert that
IFA discovers it, scrapes it, and produces findings. This is what would move
Kubernetes discovery and the chart from "implemented" to "integration
validated", and it would catch the class of bug — an RBAC rule that is one verb
short, a Service port that does not match — that unit tests structurally cannot.

### 2. Per-pod GPU attribution

A DCGM Exporter endpoint reports the GPUs on a *node*, not the GPUs belonging to
one pod. On a node running several inference workloads, the utilisation IFA
attributes to a target may belong to something else. DCGM's Kubernetes-aware
labels carry pod and namespace; using them needs a mapping from workload to pods
that the informer cache already has.

Until this lands, GPU findings are trustworthy on dedicated GPU nodes and
approximate on shared ones. The docs say so; the API does not yet.

### 3. Per-workload thresholds

Thresholds are global. A batch scoring pipeline and an interactive chat endpoint
have genuinely different definitions of "slow", and sharing one set means either
the batch workload is permanently on fire or the chat endpoint's real problems
are under the line. A per-target override block, falling back to the global set,
is the obvious shape.

### 4. A finding lifecycle

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

Not close. It would need, at minimum: integration validation against GPU-backed
vLLM and a real cluster; authentication on the API; per-workload thresholds;
signed images and an SBOM; and a stable API used by somebody other than the
author.
