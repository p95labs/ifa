# Changelog

Notable changes per release. This project follows [semantic
versioning](https://semver.org/) with the caveat that, before 1.0, minor versions
may break things — and this release does.

## [0.2.0] — unreleased

A correctness release. The headline is that the vLLM integration did not work
against a real vLLM server, and now does.

### Fixed

- **The vLLM adapter could not read a real vLLM server.** It matched unlabelled
  metric names (`vllm:num_requests_running`) and summary quantiles
  (`{quantile="0.95"}`). Real vLLM labels every series with `model_name` and
  `engine`, and exposes latency as histograms with no quantile label, so every
  metric read as absent and every derived value was zero. The adapter is rebuilt
  on a real exposition parser with histogram quantile estimation, and its
  fixtures are constructed from vLLM's own metric definitions.
- **`vllm:request_failure_total` does not exist.** The error rate was derived
  from a metric vLLM has never emitted. vLLM exposes no failure counter, so
  `error_rate_percent` is now unmeasured for vLLM targets and the abort share is
  reported instead.
- **Triton's `nv_gpu_utilization` is a rate in [0,1], not a percentage.** It was
  read as a percentage, which reported a fully loaded GPU as 0.91% utilised and
  would have fired the idle-GPU rule permanently against every Triton
  deployment.
- **Triton latency percentiles were means.** Cumulative duration counters
  divided by a request count were published as p50 and p95. Percentiles are now
  read only from Triton's summary metrics, and left unmeasured otherwise.
- **The control plane crashed on startup with `database.enabled: true`.** The
  connection pool was created but never attached to the store, leaving a nil
  pointer that panicked on the first snapshot.
- **Sustained conditions could never fire.** The evaluation window was compared
  against the sustain duration using samples at or after the cutoff, so its span
  was always fractionally short. Every rule that required a condition to persist
  was silently dead.
- **Unbounded goroutine growth on database failure.** Each snapshot started a
  goroutine to insert it; with the database unreachable these accumulated until
  the process died. Writes now go through a single writer behind a bounded
  queue, dropping and counting on overflow.
- **A failed scrape wrote a zeroed snapshot**, which let rules diagnose a
  workload the collector could not reach and prevented the staleness rule from
  firing. Failed scrapes now write nothing.
- **Counter resets reported a rate of zero** rather than "not measured", so a
  pod restart made a workload look idle and fired the low-throughput rule at the
  worst possible moment.
- **Configuration was never validated.** `Validate` existed and was not called.
- **The `/metrics` endpoint was a stub** returning `# TODO: Implement`, while a
  fully written metrics registry sat unused in the tree.
- **The NetworkPolicy blocked the product's own scrapes.** It allowed egress to
  Prometheus but not to the inference runtimes IFA actually scrapes.
- **`ifa check <url> -runtime vllm` ignored the flag**, because Go's flag package
  stops parsing at the first positional argument.

### Added

- `internal/promtext`: a Prometheus text-format parser handling label sets,
  histogram bucket interpolation, summary quantiles, `NaN`/`Inf`, and malformed
  input.
- **Cross-signal rules.** 19 rules replacing 11 single-threshold ones. The queue
  rules now distinguish a saturated GPU from an idle one; KV-cache pressure is
  reported from the preemption rate rather than from utilisation alone; slow
  TTFT is split into queueing-bound and prefill-bound.
- **Rule suppression.** A specific diagnosis silences the generic symptom it
  explains.
- **Sustained evaluation.** Conditions must hold for `sustain_for` before a rule
  fires.
- **Structured findings.** Permanent rule codes, stable IDs (`code:namespace/workload`),
  and evidence carrying the observed value, threshold, comparison and source metric.
- **`ifa check`**, which scrapes a runtime endpoint and reports which of the
  metrics IFA needs are actually present — including the models a payload
  contains when the configured `model_name` matches none.
- **`make demo`**: seven simulated workloads serving real vLLM-format exposition
  over HTTP, driven through the real collector and rule engine, with an
  end-to-end test asserting each reproduces its intended diagnosis.
- `/api/v1` with an envelope, filters, a JSON error shape, `/readyz` distinct
  from `/healthz`, and `/api/v1/rules` serving the rule catalogue.
- Real self-metrics: per-target scrape counts, errors, duration, last-success
  timestamp, missing-metric counts, and database queue statistics.
- Kubernetes discovery via shared informers, including HPA ceilings and pod
  restart counts.
- Pod and container `securityContext`, a config checksum annotation, NOTES.txt,
  `values.schema.json`, an optional ServiceMonitor and PodDisruptionBudget, and
  namespace-scoped RBAC by default.
- CI covering gofmt, vet, race tests, a demo smoke test, chart lint plus
  kubeconform, a container build, shellcheck and govulncheck.
- ADRs for the five decisions that shape the design.
- **Triton live validation.** The Triton adapter was run against a real
  Triton 25.12 server (ARM64 CPU backend, Python echo model). Three verbatim
  `/metrics` payloads were captured (idle, post-traffic, summary-latencies-enabled)
  and are stored in `internal/runtime/triton/testdata/`. `TestCapturedPayload`
  asserts that GPU fields are unmeasured (`OK == false`) on a CPU-only server, that
  0 lines are unparseable, and that summary-latency percentiles match the raw
  quantile values in the capture. See `docs/RUNTIMES.md` for the full validation
  scope and limitations.

### Changed

- **Measurements are optional values, not floats.** A signal a runtime does not
  expose is now distinguishable from one reporting zero, everywhere: rules do
  not fire on it, the API serialises it as `null`, and the database stores
  `NULL`. See [ADR 0005](docs/adr/0005-optional-measurements.md).
- **Configuration format.** Durations are written as durations (`15s`) rather
  than `*_seconds` integers, scrape targets moved to `collector.targets`, and
  unknown keys are now a startup error rather than being silently ignored.
  Existing config files need updating.
- Telemetry retention is bounded by both sample count and age.
- The Go floor moved from 1.25 to 1.23, and CI, `go.mod` and the Dockerfile now
  agree — they previously specified three different versions, so CI on `main`
  could not build the project.

### Removed

- **The Ollama adapter.** Its only route to throughput numbers was to treat
  per-request response statistics from `/api/generate` as cumulative counters,
  which produces numbers that look plausible and mean nothing.
- **The node agent.** It logged a heartbeat and collected nothing, and shipped as
  a DaemonSet on every node.
- The `deployment_mode`, `egress`, `privacy` and `rbac` configuration blocks,
  which were validated but never read by any code path. The posture they
  described is real and is documented in
  [docs/SECURITY_MODEL.md](docs/SECURITY_MODEL.md); a config field that enforces
  nothing is security theatre.
- The `recommendations` database table, which nothing wrote to.
- `deploy/kubernetes/`, superseded by `helm template`.

### Configuration migration

| Before | Now |
|---|---|
| `collector.mode: prometheus` | removed — configuring targets is what enables collection |
| `collector.interval_seconds: 15` | `collector.interval: 15s` |
| `collector.prometheus_targets` | `collector.targets` |
| `workload_name:` in a target | `name:` |
| `kubernetes.sync_interval_seconds: 30` | `kubernetes.resync_period: 10m` |
| `recommender.thresholds.high_p95_latency_ms` | `e2e_p95_ms` |
| `recommender.thresholds.high_queue_depth` | `queue_waiting_requests` |
| `recommender.thresholds.low_gpu_util_pct` | `gpu_util_low_pct` |
| `deployment_mode`, `egress`, `privacy`, `rbac` | removed |

The unversioned API paths (`/telemetry`, `/recommendations`, `/workloads`,
`/healthz`) still work and still return bare arrays.

## [0.1.0] — 2026-08

Initial public alpha.
