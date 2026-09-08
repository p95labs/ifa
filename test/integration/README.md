# Kind integration test

## What is tested

`scripts/kind-integration.sh` spins up a local `kind` cluster, installs IFA via
Helm, deploys a single Kubernetes workload that serves a committed verbatim vLLM
metrics capture over HTTP, and asserts the following end-to-end path:

```
vllm_v1_queued.txt  →  busybox httpd  →  collector scrape
  →  vLLM adapter  →  telemetry store  →  recommender (Sustained())
  →  /api/v1/recommendations  →  IFA-LAT-003 with evidence
```

Assertions made (in order):

| # | Endpoint | What is checked |
|---|---|---|
| 1 | `/api/v1/readyz` | IFA API is ready |
| 2 | `/api/v1/workloads` | Kubernetes watcher discovered the fixture Deployment |
| 3 | `/api/v1/telemetry` | `requests_waiting == 12` (vLLM adapter parsed the fixture) |
| 4 | `/metrics` | `ifa_scrapes_total` is present (Prometheus self-metrics) |
| 5 | `/api/v1/recommendations` | IFA-LAT-002 present; `ttft_p95_ms` evidence `observed ≈ 32 000 ms` (ε = 1 ms), `threshold == 1 000`, `window_seconds >= 10` |
| 6 | `/api/v1/recommendations` | IFA-LAT-003 absent (suppressed by IFA-LAT-002) |

Assertions 5–6 verify **IFA-LAT-002 ("Time to first token dominated by prefill")**
and the suppression of IFA-LAT-003 ("End-to-end p95 latency above target").

**Why IFA-LAT-002 fires at stock thresholds:**
- `TTFT p95 = 32 000 ms` from the fixture's `time_to_first_token_seconds` histogram
  (count=8, p95 rank=7.6, interpolated in the [20 s, 40 s] bucket → exactly 32 s).
  This far exceeds the default `ttft_p95_ms = 1 000 ms`.
- Queue share ≈ 44.53 % (= queue-time p95 14 250 ms / TTFT p95 32 000 ms × 100)
  is below the default `queue_share_of_ttft_pct = 50 %`, so the prefill-bound
  diagnosis fires rather than the admission-bound one (IFA-LAT-001).

**Why IFA-LAT-003 is absent:**
IFA-LAT-002 declares `Supersedes: [IFA-LAT-003]`. Assertion 6 confirms the
suppression logic works end-to-end; if both codes appear simultaneously the
engine's de-duplication has regressed.

**Why `window_seconds >= 10`:**
IFA-LAT-002 sets `Window: e.Span()` in its finding. With `sustain_for: 10 s` and
`collector.interval: 5 s`, `e.Span()` (= `span(e.recent(SustainFor))`) reaches
10 s after the third collection cycle. Asserting `window_seconds >= 10` therefore
proves that at least three real scrape cycles elapsed and that the window-tracking
code path (`recent()`) is working correctly.

## Infrastructure

| Component | Version / image |
|---|---|
| kind | v0.24.0 |
| Kubernetes node | `kindest/node:v1.31.0@sha256:53df588e04085fd41ae12de0c3fe4c72f7013bba32a20e7325357a1ac94ba865` |
| Fixture server | `busybox-fixture:1.36` (built from `busybox:1.36` via `docker buildx build --load`) |
| Fixture | `internal/runtime/vllm/testdata/vllm_v1_queued.txt` |

## Helm overrides (`test/integration/kind-values.yaml`)

All diagnosis-semantic thresholds (`ttft_p95_ms`, `queue_share_of_ttft_pct`,
`queue_waiting_requests`, etc.) are left at their production defaults. Only
CI-speed-specific values are overridden.

| Key | Integration value | Production default | Reason |
|---|---|---|---|
| `image.pullPolicy` | `Never` | `IfNotPresent` | Image is loaded via `kind load`, not pulled from a registry |
| `collector.interval` | `5s` | `15s` | Reduces wall-clock wait to fill the window; rule semantics unchanged |
| `recommender.thresholds.sustain_for` | `10s` | `45s` | Reduces wall-clock wait; `e.Span()` reaches 10 s after the 3rd cycle |

## Known limitations

### Static fixture — no counter-delta / rate-dependent behavior

The busybox httpd server returns the **same captured Prometheus payload on every
scrape**. Monotonically increasing counters (`generation_tokens_total`,
`num_preemptions_total`, etc.) do not change between scrapes, so computed rates
remain zero or unmeasured.

Therefore this integration test **does not** validate:

- `IFA-CAP-003` (queue growing) — requires a positive queue-depth trend across
  multiple samples; a static fixture always shows delta = 0.
- `IFA-EFF-002` (low token throughput) — requires `tokens_per_second` derived
  from `generation_tokens_total` counter deltas.
- `IFA-KV-001` (KV preemption) — requires a non-zero `preemptions_per_sec` rate
  from `num_preemptions_total` counter deltas.
- Any other finding that depends on a rate computed from consecutive counter
  values.

A future test could serve different fixture captures on alternate scrapes to
exercise rate-dependent rules. That is explicitly out of scope for this test.

### GPU metrics absent

The queued fixture was captured from a CPU-only vLLM server. GPU utilisation,
GPU memory, and GPU-dependent rules (`IFA-CAP-001`, `IFA-CAP-002`,
`IFA-EFF-003`) are not tested here.

## CI status

The `kind-integration` job is present in `.github/workflows/ci.yml` and runs on
every push and pull request. It is **not** currently a required branch-protection
check. The team intends to observe it across several PRs for stability before
promoting it to required status.
