# HTTP API

Base URL is wherever the control plane listens, `:8080` by default.

**Every endpoint is a GET.** Anything else returns `405` with an `Allow` header.
The API cannot change IFA's state or the cluster's, and that is enforced in one
place ([`getOnly`](../internal/api/server.go)) rather than being a property each
handler has to remember.

## Versioning

Current version is `v1`, served under `/api/v1/`.

The unversioned paths — `/healthz`, `/telemetry`, `/recommendations`,
`/workloads` — predate `/api/v1` and are kept. They return the **bare arrays**
the original API returned, not the versioned envelope, so clients written
against them keep working. They will not gain new features.

Within `v1`: new fields may be added; existing fields will not change type or
meaning; rule codes are permanent. A breaking change means `v2` alongside `v1`.

## Envelope

List endpoints under `/api/v1/` return:

```json
{
  "items": [ ... ],
  "count": 7,
  "generated_at": "2026-08-29T12:12:37Z"
}
```

Errors, on every endpoint:

```json
{
  "error": {
    "code": "invalid_parameter",
    "message": "severity must be one of \"info\", \"warning\", \"critical\""
  }
}
```

| `error.code` | HTTP | Meaning |
|---|---|---|
| `invalid_parameter` | 400 | A query parameter was malformed or out of range |
| `method_not_allowed` | 405 | Not a GET or HEAD |
| `not_found` | 404 | Unknown path |

An unrecognised value for a filter is a `400`, not an empty result. A typo in
`?severity=critcal` returning `[]` reads as "no critical findings", which is the
worst possible answer.

## `null` means "not measured"

Every measurement in a telemetry payload can be `null`, and `null` is not `0`.
It means the runtime did not report that signal for that scrape — no DCGM
endpoint configured, a metric absent on that runtime version, or a counter whose
rate cannot yet be computed. Rules do not fire on `null`, and neither should
your dashboards. See [ADR 0005](adr/0005-optional-measurements.md).

---

## `GET /api/v1/healthz`

Liveness, plus the configuration the process actually loaded — useful for
confirming that the ConfigMap you edited is the one running.

```json
{
  "status": "ok",
  "uptime_seconds": 261,
  "config": {
    "version": "v0.2.0-alpha.1",
    "commit": "f863b6240628",
    "collector_mode": "demo",
    "target_count": 7,
    "kubernetes": "disabled",
    "database": "disabled"
  }
}
```

Always `200` while the process is running.

## `GET /api/v1/readyz`

Readiness. `503` with `{"status":"starting"}` until the first collection cycle
completes, `200` with `{"status":"ready"}` afterwards.

Readiness deliberately does **not** depend on scrapes succeeding. An instance
that cannot reach its targets is exactly the instance an operator needs to be
able to query; taking it out of service would hide the problem it is reporting.

## `GET /api/v1/telemetry`

Latest snapshot per workload, ordered by `namespace/name`.

Filters: `namespace`, `workload`, `runtime`.

```json
{
  "timestamp": "2026-08-29T12:12:37.225900959Z",
  "cluster_name": "demo",
  "namespace": "inference",
  "workload_name": "chat-llama3-8b",
  "runtime": "vllm",
  "model_name": "meta-llama/Llama-3.1-8B-Instruct",

  "request_rate_per_sec": 22.04,
  "tokens_per_second": 931.8,
  "prompt_tokens_per_sec": 2692.2,

  "p50_latency_ms": 1812.1,
  "p95_latency_ms": 4464.2,
  "p99_latency_ms": 4959.4,
  "ttft_p50_ms": 145.0,
  "ttft_p95_ms": 243.2,
  "ttft_p99_ms": 365.5,
  "queue_time_p95_ms": 285,

  "requests_running": 8.5,
  "requests_waiting": 0.1,
  "waiting_for_capacity": 0.1,
  "waiting_deferred": 0,

  "gpu_utilization_percent": 68,
  "gpu_memory_used_percent": 71.6,
  "kv_cache_usage_percent": 58.08,
  "preemptions_per_sec": 0,
  "prefix_cache_hit_rate_percent": 62.0,

  "error_rate_percent": null,
  "abort_rate_percent": 0,

  "replicas": 3,
  "ready_replicas": 3,
  "max_replicas": 10,

  "scrape_duration_ms": 1
}
```

`error_rate_percent` is `null` here because the target is vLLM, which exposes no
failure counter. See [RUNTIMES.md](RUNTIMES.md).

**Units.** Latencies are milliseconds. Percentages are 0–100 — note that both
vLLM's KV-cache metric and Triton's GPU-utilisation metric are fractions in
[0,1] at the source and are converted. Rates are per second. Timestamps are
RFC 3339 UTC, taken by IFA at scrape time, never from the exporter.

## `GET /api/v1/recommendations`

Runs the rule engine against current telemetry and returns findings, ordered by
severity then rule code.

Filters: `namespace`, `workload`, `runtime`, `severity` (`info` | `warning` |
`critical`), `code`.

```json
{
  "id": "IFA-SCL-001:inference/batch-scoring",
  "code": "IFA-SCL-001",
  "severity": "critical",
  "namespace": "inference",
  "workload_name": "batch-scoring",
  "runtime": "vllm",
  "title": "Saturated at the replica ceiling",
  "explanation": "33 requests have been queueing for 5s while the deployment sits at its ceiling of 6 replicas. Horizontal scaling has nothing left to give, so the queue will not drain until traffic falls or each replica does more.",
  "suggested_action": "Raise the maximum replica count if there is cluster capacity, or reduce per-request cost. If GPU nodes are the constraint, this is a cluster-capacity decision rather than a workload one.",
  "evidence": [
    { "metric": "replicas", "observed": 6, "unit": "replicas" },
    { "metric": "max_replicas", "observed": 6, "unit": "replicas" },
    {
      "metric": "requests_waiting",
      "source": "vllm:num_requests_waiting",
      "observed": 33.2,
      "threshold": 8,
      "comparison": ">",
      "unit": "requests"
    }
  ],
  "observed_at": "2026-08-29T12:12:37.227236405Z",
  "window_seconds": 5
}
```

**`id` is stable.** It is `code:namespace/workload`, derived from the rule and
the workload rather than from evaluation order, so two calls while the same
condition holds return the same ID and clients can deduplicate on it. It does
not change when other workloads appear or disappear.

**`evidence`** entries with a `threshold` and a `comparison` are what triggered
the rule; entries without are supporting context. `source` names the runtime
metric the value came from, so you can go and query it yourself.

**`window_seconds`** is the span of telemetry the rule sustained over. Absent on
rules that evaluate a single sample.

## `GET /api/v1/workloads`

Kubernetes-discovered workloads. Empty array when discovery is disabled — never
`null`.

```json
{
  "name": "chat-llama3-8b",
  "namespace": "inference",
  "runtime": "vllm",
  "model_name": "meta-llama/Llama-3.1-8B-Instruct",
  "replicas": 3,
  "ready_replicas": 3,
  "restart_count": 0,
  "gpu_request": "1",
  "labels": { "inference.io/runtime": "vllm" },
  "last_updated": "2026-08-29T12:12:37Z"
}
```

## `GET /api/v1/rules`

The rule catalogue and the thresholds in force. Threshold keys match the
configuration file exactly, and durations are rendered as durations, so the
response can be compared against your `config.yaml` directly.

```json
{
  "items": [
    {
      "code": "IFA-KV-001",
      "title": "KV cache exhausted: requests are being preempted",
      "severity": "critical",
      "summary": "The runtime is evicting in-flight requests to reclaim KV-cache blocks...",
      "supersedes": ["IFA-KV-002"]
    }
  ],
  "count": 19,
  "thresholds": {
    "sustain_for": "45s",
    "kv_cache_high_pct": 90,
    "preemptions_per_sec": 0.01
  }
}
```

## `GET /metrics`

IFA's own operational metrics, in Prometheus text format. Unversioned by
convention.

| Metric | Type | Notes |
|---|---|---|
| `ifa_build_info` | gauge | Labels: `version`, `commit`, `go_version` |
| `ifa_uptime_seconds` | gauge | |
| `ifa_scrapes_total` | counter | By `workload`, `runtime` |
| `ifa_scrape_errors_total` | counter | By `workload`, `runtime` |
| `ifa_scrape_duration_milliseconds` | gauge | Most recent successful scrape |
| `ifa_last_successful_scrape_timestamp_seconds` | gauge | **Alert on this**, not on the error counter — a target that stopped being scraped entirely increments no errors |
| `ifa_target_missing_metrics` | gauge | Non-zero means some rules cannot run for that workload |
| `ifa_store_snapshots` | gauge | Retained in memory |
| `ifa_recommendations_total` | counter | By `severity` |
| `ifa_active_recommendations` | gauge | From the most recent analysis |
| `ifa_database_writes_total` | counter | Only when the database is enabled |
| `ifa_database_write_failures_total` | counter | " |
| `ifa_database_dropped_total` | counter | " — non-zero means history is incomplete; diagnostics are unaffected |

## Authentication

There is none. This is the API's significant limitation, and it is why the
NetworkPolicy in the chart restricts ingress. See
[SECURITY_MODEL.md](SECURITY_MODEL.md).
