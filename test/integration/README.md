# Integration tests

Two separate kind-cluster integration test suites run in CI on every push and
pull request:

| Suite | Script | CI job name | Validates |
|---|---|---|---|
| Static fixture | `scripts/kind-integration.sh` | `Kind integration test` | Kubernetes discovery, Helm install, metrics scraping, latency diagnosis, rule suppression |
| Dynamic multi-scrape | `scripts/kind-dynamic-integration.sh` | `Kind dynamic integration test` | Counter-to-rate conversion, rate-dependent rules, counter reset, missing telemetry and recovery |

Both are **required** branch-protection checks on `main`.

---

## Static fixture integration test

### What is tested

`scripts/kind-integration.sh` spins up a local `kind` cluster, installs IFA via
Helm, deploys a single Kubernetes workload that serves a committed verbatim vLLM
metrics capture over HTTP, and asserts the following end-to-end path:

```
vllm_v1_queued.txt  →  busybox httpd  →  collector scrape
  →  vLLM adapter  →  telemetry store  →  recommender (e.Span())
  →  /api/v1/recommendations  →  IFA-LAT-002 with evidence
                              →  IFA-LAT-003 suppressed
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

### Infrastructure

| Component | Version / image |
|---|---|
| kind | v0.24.0 |
| Kubernetes node | `kindest/node:v1.31.0@sha256:53df588e04085fd41ae12de0c3fe4c72f7013bba32a20e7325357a1ac94ba865` |
| Fixture server | `busybox-fixture:1.36` (built from `busybox:1.36` via `docker buildx build --load`) |
| Fixture | `internal/runtime/vllm/testdata/vllm_v1_queued.txt` |

### Helm overrides (`test/integration/kind-values.yaml`)

All diagnosis-semantic thresholds (`ttft_p95_ms`, `queue_share_of_ttft_pct`,
`queue_waiting_requests`, etc.) are left at their production defaults. Only
CI-speed-specific values are overridden.

| Key | Integration value | Production default | Reason |
|---|---|---|---|
| `image.repository` / `image.tag` | `ifa` / `integration-test` | `ghcr.io/p95labs/ifa` / chart `appVersion` | The locally built image, not the published one |
| `image.pullPolicy` | `Never` | `IfNotPresent` | Image is loaded via `kind load`, not pulled from a registry |
| `collector.interval` | `5s` | `15s` | Reduces wall-clock wait to fill the window; rule semantics unchanged |
| `collector.timeout` | `4s` | `10s` | Must stay below `interval` to satisfy the collector's startup check |
| `logging.level` / `format` | `debug` / `text` | `info` / `json` | Readable pod logs when an assertion fails |
| `recommender.thresholds.sustain_for` | `10s` | `45s` | Reduces wall-clock wait; `e.Span()` reaches 10 s after the 3rd cycle |

Only `collector.interval`, `collector.timeout` and `sustain_for` affect timing,
and none of them affects a diagnosis. Every threshold the rules compare against
is inherited unchanged.

### Known limitations

#### Static fixture — no counter-delta / rate-dependent behavior

The busybox httpd server returns the **same captured Prometheus payload on every
scrape**. Monotonically increasing counters (`generation_tokens_total`,
`num_preemptions_total`, etc.) do not change between scrapes, so computed rates
remain zero or unmeasured.

This test therefore does not validate counter-rate rules. Those behaviours are
covered by the dynamic multi-scrape integration test below.

#### GPU metrics absent

The queued fixture was captured from a CPU-only vLLM server. GPU utilisation,
GPU memory, and GPU-dependent rules (`IFA-CAP-001`, `IFA-CAP-002`,
`IFA-EFF-003`) are not tested here.

---

## Dynamic multi-scrape integration test

### What is tested

`scripts/kind-dynamic-integration.sh` spins up a separate kind cluster, installs
IFA via Helm, and deploys `dynserver` — a small Go HTTP server
(`test/integration/dynserver/cmd/`) that serves deterministic synthetic
Prometheus exposition at scenario-specific endpoints. IFA's collector scrapes the
dynserver exactly as it would scrape a real workload.

Every scenario is exercised through the full IFA pipeline:

```
dynserver (/scenario/{id}/metrics)
  →  collector HTTP scrape  →  runtime adapter  →  rate tracker
  →  telemetry store  →  rule engine
  →  /api/v1/telemetry  →  /api/v1/recommendations
```

Test control is explicit: the script POSTs to `/scenario/{id}/advance` to move
the dynserver to the next state, then waits for the telemetry timestamp to
advance (confirming IFA processed the new state), then asserts on the structured
API response. There are no uncontrolled fixed sleeps; every assertion has a
bounded timeout with diagnostic output on failure.

### Test layers

| Layer | Where | What |
|---|---|---|
| Unit tests | `go test ./test/integration/dynserver/...` | Payload generator correctness — parsability, counter monotonicity, histogram accuracy, DCGM values — without a cluster |
| Dynamic integration | `scripts/kind-dynamic-integration.sh` | Six scenarios through the full IFA pipeline in a real kind cluster |

### Scenarios

#### Scenario A — Counter-to-rate conversion

Validates that the collector (`internal/collector/collector.go` / `rate.go`)
correctly converts cumulative counters to per-second rates.

| Step | dynserver state | Expected IFA behaviour |
|---|---|---|
| 0 | `gen_tokens=1000`, `prompt=500`, `requests=50` (first observation) | `tokens_per_second`, `prompt_tokens_per_sec`, `request_rate_per_sec` all **unmeasured** (`ok=false`) |
| 1 | Counters +1000/+200/+10 | All rates **positive**; `tokens_per_second ≈ 200/s` within 4× CI-timing tolerance |
| 2 | Same delta repeated | Rates remain positive; delta12 == delta01 confirmed |
| 3 | Counters unchanged | `tokens_per_second = 0`, `prompt_tokens_per_sec = 0` (**measured zero**, not unmeasured) |

**What this proves:** The rate tracker's first-observation guard works; positive
deltas produce positive rates; zero delta after a valid baseline produces a
measured zero, not an unmeasured result.

#### Scenario B — Growing request queue (IFA-CAP-003)

Exercises the queue-growth trend rule, which requires `minSamples=6` across
`SustainFor * TrendWindow = 10 s × 3 = 30 s` of history.

| States | Queue depth | Expected IFA behaviour |
|---|---|---|
| 0–5 | 5, 7, 9, 11, 13, 15 (growing) | IFA-CAP-003 fires after state 5; early-third mean ≈ 6, late-third mean ≈ 14, trend delta = 8 > 1 threshold |
| 6–8 | 15, 15, 15 (stable) | IFA-CAP-003 stops firing once the trend window contains only stable values |

The finding's `window_seconds` and `evidence[requests_waiting_trend].observed`
fields are asserted.

**What this proves:** The rule requires sustained growth, not just a deep queue.
A deep but stable queue does not trigger IFA-CAP-003.

#### Scenario C — KV-cache preemption (IFA-KV-001 / IFA-KV-002 suppression)

Exercises preemption-rate detection and rule suppression.

| State | KV | Preemptions | Expected IFA behaviour |
|---|---|---|---|
| 0 | 95% | 0 | IFA-KV-001 absent (no baseline); IFA-KV-002 not yet sustained |
| 1–2 | 95% | 0 | IFA-KV-002 fires after 3 scrapes (KV > 90% sustained 10 s); IFA-KV-001 absent |
| 3 | 95% | 5 | Rate = (5−0)/5 s = 1.0/s > 0.01 threshold → IFA-KV-001 fires; **IFA-KV-002 suppressed** |
| 4 | 95% | 25 | Rate = (25−5)/5 s = 4.0/s → IFA-KV-001 continues; IFA-KV-002 still suppressed |

**What this proves:** High KV-cache occupancy alone does not trigger IFA-KV-001;
a positive preemption counter delta is required. Once IFA-KV-001 fires it
correctly supersedes IFA-KV-002 per the `Supersedes` declaration.

#### Scenario D — Low token throughput + synthetic DCGM GPU metrics (IFA-EFF-002)

Exercises the efficiency rule that requires both measured GPU utilisation (from a
DCGM endpoint) and measured low token throughput (from counter deltas).

The synthetic DCGM endpoint at `/scenario/d-eff/dcgm` reports 92% GPU
utilisation and ~80% framebuffer usage. This is labelled as simulated
GPU-metrics integration testing; it does not validate physical GPU behaviour.

| Step | Workload | Tokens | GPU | Expected |
|---|---|---|---|---|
| Initial | d-eff (has DCGM URL) | Baseline only | 92% (DCGM) | IFA-EFF-002 absent (no rate yet) |
| State 1 | d-eff | +500/5s = 100/s < 200 threshold | 92% | **IFA-EFF-002 fires** |
| State 2 | d-eff | +5000/5s = 1000/s > 200 | 92% | IFA-EFF-002 **absent** (throughput above threshold) |
| State 1 | d-nodcgm (no DCGM URL) | 100/s | **unmeasured** | IFA-EFF-002 absent (GPU not measured) |

Evidence fields are verified to show `gpu_utilization_percent > 85` (source:
DCGM endpoint) and `tokens_per_second < 200` (source: counter delta).

**What this proves:** IFA-EFF-002 requires both conditions simultaneously. GPU
utilisation must come from a configured DCGM endpoint, not from KV-cache
occupancy.

#### Scenario E — Counter reset handling

Validates the rate tracker's reset-detection path.

| State | gen_tokens | preemptions | Expected |
|---|---|---|---|
| 0 | 5000 | 100 | Baseline established |
| 1 | 3000 | 80 | **Reset detected** — both counters decreased; rates **unmeasured** |
| 2 | 4000 | 90 | New baseline: rate = (4000−3000)/5s = 200/s; **positive** |
| 3 | 5000 | 100 | Rate continues correctly |

**What this proves:** A decreasing counter (simulating a process restart) is
treated as a reset, not as a negative rate or a zero rate. The new value becomes
the baseline for subsequent calculations.

#### Scenario F — Missing telemetry and recovery

Validates how IFA handles unavailable or malformed metrics.

| State | Server response | Expected |
|---|---|---|
| 0 | Healthy vLLM payload | Baseline telemetry established |
| 1 | HTTP 503 | `ifa_scrape_errors_total` increments; no telemetry snapshot written |
| 2 | HTTP 200, unrecognised metric families only | `metrics_missing` populated; required gauges **not silently zero** |
| 3 | Healthy vLLM payload (recovery) | Telemetry resumes; no history-dependent rules fire prematurely |

**What this proves:** Failed or malformed scrapes do not silently produce healthy
zero telemetry. Recovery restores the collection pipeline; rules require
re-accumulation of valid history before firing.

### Infrastructure

| Component | Version / image |
|---|---|
| kind | v0.24.0 |
| Kubernetes node | `kindest/node:v1.31.0@sha256:53df588e04085fd41ae12de0c3fe4c72f7013bba32a20e7325357a1ac94ba865` |
| dynserver | Built from `deploy/docker/Dockerfile.dynserver`; loaded into kind via `kind load docker-image` |
| IFA | Built from `deploy/docker/Dockerfile`; loaded into kind |

### Helm overrides (`test/integration/kind-dynamic-values.yaml`)

Same CI-speed overrides as the static test (`interval=5s`, `timeout=4s`,
`sustain_for=10s`). Kubernetes discovery is disabled; all targets are declared
explicitly in `config.collector.targets`. No production thresholds are changed.

### What synthetic testing does and does not prove

**Does prove:**
- The full IFA pipeline (scraping → rate tracking → rule evaluation → API)
  correctly handles changing counters, counter resets, missing metrics, and HTTP
  errors.
- Rules fire and stop firing at the right states.
- Evidence fields contain the values derived from actual counter deltas.
- Scrape-error metrics increment on HTTP failures.

**Does not prove:**
- Counter-rate rules fire correctly against a live vLLM or GPU workload.
- The dynserver token and queue values reflect any real inference pattern.
- The synthetic DCGM data (92% GPU util) validates any physical GPU behaviour.
- Multi-GPU aggregation on real hardware.

All dynserver payloads carry `# dynserver-integration-test: synthetic` in their
comment headers. The DCGM payload carries `Synthetic integration-test data — not
a physical GPU capture` in its HELP strings.

### What distinguishes the test layers

| Layer | Uses real scrapes | Changing telemetry | Real Kubernetes | Real inference server | Real GPU |
|---|:---:|:---:|:---:|:---:|:---:|
| Unit tests | ✗ | ✗ | ✗ | ✗ | ✗ |
| Static fixture integration | ✓ | ✗ | ✓ | ✗ (busybox) | ✗ |
| Dynamic multi-scrape integration | ✓ | ✓ | ✓ | ✗ (dynserver) | ✗ |
| Live runtime validation | ✓ | ✓ | ✓ | ✓ | varies |
| Physical GPU validation | ✓ | ✓ | ✓ | ✓ | ✓ |

---

## CI status

| Job name | Script | Required check |
|---|---|---|
| `Kind integration test` | `scripts/kind-integration.sh` | **Yes** |
| `Kind dynamic integration test` | `scripts/kind-dynamic-integration.sh` | **Yes** |

Both jobs are defined in `.github/workflows/ci.yml` and run on every push and
pull request to `main`. A PR cannot merge while either is failing.
