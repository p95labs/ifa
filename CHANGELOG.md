# Changelog

Notable changes per release. This project follows [semantic
versioning](https://semver.org/) with the caveat that, before 1.0, minor versions
may break things — and this release does.

## [0.2.0-alpha.1] — 2026-10-21

A correctness release. The headline is that the vLLM integration did not work
against a real vLLM server, and now does — verified against one, on CPU and on
a GPU.

Artifacts for this release:

| | |
|---|---|
| Git tag | `v0.2.0-alpha.1` |
| Container image | `ghcr.io/p95labs/ifa:0.2.0-alpha.1` — note: **no `v` prefix**, unlike the git tag |
| Platforms | `linux/amd64`, `linux/arm64` |
| Chart `version` / `appVersion` | `0.2.0-alpha.1` |
| Binary archives | `ifa_v0.2.0-alpha.1_{linux,darwin}_{amd64,arm64}.tar.gz`, each containing `control-plane` and `ifa` |
| Checksums | `SHA256SUMS` |

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
- **Webhook alerting.** Findings are optionally POSTed to a webhook endpoint on
  *state transitions* — one alert when a condition starts, one when it clears —
  rather than on every evaluation. Delivery runs behind a single writer and a
  bounded queue, so a slow or dead endpoint cannot stall collection; overflow is
  dropped and counted. Filtered by `min_severity` (default `warning`). The URL is
  the credential for Slack, Teams and PagerDuty alike, so the chart takes it from
  a Secret (`alerting.existingSecret`) rather than the ConfigMap, and the startup
  log redacts it. This is webhook delivery, not an alerting platform: no routing,
  no escalation, no silences, no grouping. See
  [ADR 0006](docs/adr/0006-alert-deduplication.md) and
  [docs/OPERATIONS.md](docs/OPERATIONS.md).
- CI covering gofmt, vet, race tests, a demo smoke test, chart lint plus
  kubeconform, a container build, shellcheck, govulncheck, and an end-to-end
  kind cluster integration test. All seven jobs are required branch-protection
  checks on `main`.
- ADRs for the six decisions that shape the design.
- **Triton live validation.** The Triton adapter was run against a real
  Triton 25.12 server (ARM64 CPU backend, Python echo model). Three verbatim
  `/metrics` payloads were captured (idle, post-traffic, summary-latencies-enabled)
  and are stored in `internal/runtime/triton/testdata/`. `TestCapturedPayload`
  asserts that GPU fields are unmeasured (`OK == false`) on a CPU-only server, that
  0 lines are unparseable, and that summary-latency percentiles match the raw
  quantile values in the capture. GPU-backed Triton remains unvalidated. See
  `docs/RUNTIMES.md` for the full validation scope and limitations.
- **Kubernetes integration test.** `scripts/kind-integration.sh` installs the
  chart into a real kind cluster (Kubernetes 1.31), serves a committed vLLM
  capture from a busybox httpd pod, and asserts the full path — informer
  discovery, in-cluster scrape, parse, sustained evaluation, suppression, REST
  response — with six assertions at shipped default thresholds. It asserts
  `IFA-LAT-002` fires with `ttft_p95_ms ≈ 32 000` against the stock
  `threshold = 1000`, and that `IFA-LAT-003` is *absent*, which is what proves
  suppression works outside a unit test. Only collection interval and sustain
  window are overridden, purely for CI wall-clock.
- **GPU-backed vLLM and DCGM validation (one NVIDIA L4).** vLLM 0.28.0 was run
  on an NVIDIA L4 with the KV cache deliberately constrained and driven into
  genuine exhaustion under continuous-refill load: the capture shows
  `kv_cache_usage_perc ≈ 0.999` (**≈99.9%**), 84 requests running, 16 waiting and
  all 16 labelled `reason="capacity"`, with `num_preemptions_total` nonzero and
  climbing. GPU utilisation from the same session was captured at
  `DCGM_FI_DEV_GPU_UTIL = 100`. Until this, the KV rules — which fire around 90%
  — could only be evaluated against synthetic data, because the highest
  occupancy reachable on CPU was 2.86%. Captures are committed verbatim in
  `internal/runtime/vllm/testdata/` and `internal/runtime/dcgm/testdata/` with
  checksums and full provenance, and are asserted by regression tests.

  Four things this deliberately does not claim: one L4 is one card, so nothing
  is established about multi-GPU aggregation or KV-cache pooling; the captures
  are snapshots, so the preemption *rate* was never measured even though the
  counter was seen climbing; the exporter was a DCGM-*compatible* one bundled by
  the GPU host, **not** NVIDIA's official `dcgm-exporter` image; and GPU-backed
  Triton was not run.
- **A measured DCGM refresh lag.** The exporter above was measured reading
  18–20 seconds behind real GPU state, confirmed by polling its raw `/metrics`
  independently of IFA. A matched pair of captures — one stale, one settled,
  under identical load — is committed as fixtures. The operational consequence
  is documented: size `collector.interval` and `sustain_for` above your
  exporter's refresh lag, or a saturated GPU reads as idle and `IFA-CAP-002`
  misfires.

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
- **The Go version is now stated once and deliberate everywhere.** `go.mod`, the
  CI `GO_VERSION`, the release workflow and the Dockerfile previously specified
  three different versions, so CI on `main` could not build the project. The
  language floor is now 1.26; CI, the release workflow and the Dockerfile build
  with 1.27. Those two numbers differing is intentional, not drift: a Go binary
  embeds the standard library it was built with, so only a newer *toolchain*
  carries a `crypto/tls` or `net/http` fix. Pinning the build toolchain to the
  language floor would make the vulnerability scan unfixable the next time a
  standard library advisory lands.
- **The repository and Go module moved to `github.com/p95labs/ifa`.** Imports,
  the chart's `home`/`sources`, the image repository (`ghcr.io/p95labs/ifa`) and
  every documentation link were updated. The former paths are not redirected
  beyond GitHub's own repository redirect.
- `k8s.io/{api,apimachinery,client-go}` moved to v0.35.0. The kind integration
  test deliberately stays on a Kubernetes 1.31 node — client-go is supported
  against servers either side, and the API surface IFA uses has been stable
  throughout.

### Security

- **Closed 11 reachable vulnerabilities.** `golang.org/x/net` moved to v0.60.0
  (GO-2026-6603, -6610, -6611, -6612, -6617) and `golang.org/x/text` to v0.42.0
  (GO-2026-6629, which is fixed in v0.41.0; v0.42.0 is what x/net v0.60.0
  requires). The remainder — GO-2026-6605, -6607, -6608, -6613 and -6809, in
  `crypto/tls`, `crypto/x509`, `net/http` and `net/textproto` — are standard
  library issues that no dependency bump can reach, so the build toolchain moved
  to 1.27 instead. All 11 were reachable from this code: the collector's scrape
  path, the API server and the Kubernetes client all sit on `net/http`.
  `govulncheck` reports no vulnerabilities as of this release; it is a required
  CI check.

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

## [0.1.0-alpha.1] — 2026-09-09

First published release: binary archives for linux/amd64, linux/arm64,
darwin/amd64 and darwin/arm64 with SHA256 checksums, plus multi-arch
(linux/amd64, linux/arm64) container images on GHCR. Chart and `appVersion`
both `0.1.0-alpha.1`.

## [0.1.0-alpha] — 2026-08-01

Initial public alpha. Tagged only; no release artifacts were published for this
tag.
