# Architecture

## Goals and non-goals

**Goals.** Turn the telemetry an inference runtime already exposes into a small
number of explained, checkable diagnoses; do it without any ability to change
what it observes; and be usable by someone who has not read the source.

**Non-goals.** IFA is not a metrics store, a query engine, an alerting platform,
or a dashboard, and it does not remediate. Those exist and are good. IFA is the
layer that decides *which* problem you have, and it assumes Prometheus is doing
the rest.

It does POST findings to a webhook on a state transition, which is delivery, not
alerting: there is no routing, no escalation, no on-call schedule, no silences
and no grouping policy. Alertmanager and PagerDuty own those. See
[ADR 0006](adr/0006-alert-deduplication.md).

## Components

```
cmd/control-plane      wiring, lifecycle, signal handling
cmd/ifa                terminal client; talks only to the HTTP API

internal/promtext      Prometheus text-format parser: labels, histograms, summaries
internal/runtime       the adapter contract shared by every runtime
  ../vllm              vLLM adapter
  ../triton            Triton adapter
  ../dcgm              DCGM Exporter adapter
internal/collector     scrape loop, counter-to-rate conversion, GPU overlay
internal/telemetry     the shared measurement model and the in-memory store
internal/recommender   rule registry, evaluation, suppression
internal/k8s           workload discovery via shared informers
internal/api           read-only HTTP API
internal/alerting      optional webhook delivery on finding transitions
internal/metrics       IFA's own operational metrics
internal/storage/timescale   optional durable history
internal/demo          simulated inference fleet for the demo and the e2e test
```

## Data flow

One scrape cycle, per target:

1. **Fetch.** `collector` issues a bounded GET: a hard timeout, a response size
   cap, and no redirect following.
2. **Parse.** The runtime adapter turns the payload into a `Reading`: a partly
   filled `telemetry.Snapshot` of instantaneous values, plus raw counter values
   under runtime-agnostic keys, plus the list of expected metrics that were
   absent.
3. **Rate.** The collector differences each counter against the previous scrape.
   A first observation and a counter reset both produce "not measured", never
   zero.
4. **Overlay.** If a DCGM endpoint is configured, GPU utilisation and memory are
   read from it and taken as the maximum across devices.
5. **Join.** Replica counts and the HPA ceiling come from the informer cache.
6. **Store.** The snapshot goes into a bounded in-memory window and, if
   configured, onto the durable sink's queue.

Evaluation happens on request, not on a timer: `GET /api/v1/recommendations`
runs the rule set against the current store. Rules are cheap — the benchmark
covers fifty workloads with two minutes of history each — and evaluating on
demand means a finding always reflects the newest telemetry rather than whenever
the last background pass happened to run.

When webhook alerting is enabled there is one exception: the control plane also
evaluates on the collection interval, so that a transition can be noticed
without anyone having called the API. The sender compares each cycle's findings
against the previous set by stable ID and delivers only the differences — one
alert when a condition appears, one when it clears. Everything else is dropped,
which is why a condition firing for an hour produces two alerts and not two
hundred.

## Concurrency model

- **One goroutine per scrape**, bounded by a semaphore sized to
  `collector.concurrency`. Serial scraping makes the effective interval depend
  on how many targets are slow; unbounded scraping turns a large fleet into a
  thundering herd.
- **The telemetry store** is a `sync.RWMutex` around a map of slices. Writes come
  from scrape goroutines, reads from HTTP handlers. It is small enough that a
  more elaborate structure would be harder to reason about for no measurable
  gain.
- **The durable sink** owns exactly one writer goroutine behind a bounded
  channel. `Write` never blocks; when the queue is full it drops and counts.
  The earlier design started a goroutine per snapshot, which grows without limit
  whenever the database is unreachable — the observability tool taking down its
  own pod.
- **Informers** run their own goroutines inside client-go. IFA reads from the
  lister cache and never issues a request on the read path.
- **The rate tracker** is a mutex-guarded map keyed by workload and counter name.

Everything is cancelled through one `context.Context` derived from
`signal.NotifyContext`.

## Failure behaviour

The rule is that IFA degrades to "I cannot see this" rather than to "everything
looks fine".

| Failure | Behaviour |
|---|---|
| Scrape target unreachable | No snapshot written. `ifa_scrape_errors_total` increments. Once the newest sample ages past `stale_after`, `IFA-OBS-002` fires. |
| Target returns garbage | Unparseable lines are counted and logged; whatever parsed is used. A consistently non-zero count means the exposition format moved. |
| Target exposes a subset | Missing metrics are recorded on the snapshot; `IFA-OBS-001` reports them; rules that need them stay dormant. |
| Counter reset (pod restart) | The interval reports "not measured" rather than a rate of zero, and re-baselines. |
| DCGM endpoint down | The inference telemetry is still recorded; GPU fields are unmeasured; GPU rules stay dormant. |
| Kubernetes API unavailable | Discovery is skipped with a warning; scraping and diagnosis continue; scaling rules stay dormant. Startup is *not* refused. |
| Database unavailable at startup | Startup fails. Enabling history and silently not writing it is worse than a clear failure. |
| Database fails later | Writes are dropped and counted in `ifa_database_dropped_total`. Diagnostics are unaffected — the rule engine only reads memory. |
| Alerting webhook slow or down | Two delivery attempts (one immediate, one after a backoff) for transient failures; a rejected payload — a 400 — is not retried. The bounded queue then drops and counts on overflow (`alert dropped: webhook queue is full`). Collection and the API are unaffected: `Notify` never blocks. |
| Invalid configuration | Startup fails with the offending field named. |
| Workload deleted | Its telemetry ages out and is pruned; it disappears from the API. |
| SIGTERM | HTTP server drains within `shutdown_timeout`, collector stops, sink drains its queue. |

## Trust boundaries

```
    ┌─────────────────────── untrusted input ────────────────────────┐
    │  Runtime /metrics payloads       DCGM /metrics payloads         │
    └────────────────────────────┬───────────────────────────────────┘
                                 │  size-capped, timeout-bounded,
                                 │  parsed without regexp or reflection
    ┌────────────────────────────▼───────────────────────────────────┐
    │  IFA process — no writes anywhere, no shell, no filesystem     │
    └────────────────────────────┬───────────────────────────────────┘
                                 │  get / list / watch only
    ┌────────────────────────────▼───────────────────────────────────┐
    │  Kubernetes API                                                 │
    └────────────────────────────────────────────────────────────────┘
```

Scrape payloads are attacker-influenced in the sense that whoever controls a
watched pod controls what IFA parses. The parser allocates proportionally to
input, the input is capped, and nothing parsed is ever executed, interpolated
into a query, or written anywhere. The configured target list is the trust
anchor: IFA only ever fetches URLs an operator put in the config, and those are
restricted to `http` and `https` at load time.

Details and the list of what is *not* defended against: [SECURITY_MODEL.md](SECURITY_MODEL.md).

## Design decisions

Recorded as short ADRs:

- [0001 — Read-only, no remediation](adr/0001-read-only.md)
- [0002 — Runtime adapters as pure functions](adr/0002-runtime-adapters.md)
- [0003 — TimescaleDB optional and non-blocking](adr/0003-optional-timescaledb.md)
- [0004 — Deterministic rules, not learned models](adr/0004-deterministic-rules.md)
- [0005 — Measurements are optional values](adr/0005-optional-measurements.md)
- [0006 — Alert deduplication via state transitions](adr/0006-alert-deduplication.md)

## Extension points

**A new runtime** means implementing `runtime.Adapter` — three methods, no state
— and adding one line to the registry in `internal/collector/collector.go`.
Adapters are pure functions from an exposition payload to a `Reading`, so they
are tested against a fixture with no HTTP, no clock and no store.

**A new rule** means one function returning a `Rule` and one line in
`DefaultRules()`. A rule declares its severity, the runtimes it applies to, and
any codes it supersedes; the engine handles ordering, ID generation and
suppression.

**A new sink** means implementing `telemetry.Sink` (one method) and attaching it
with `telemetry.WithSink`.

## Scale characteristics

Not benchmarked against a real fleet — this is what the design implies, not a
measurement.

- Memory is bounded by `retention_samples × workloads`. A snapshot is roughly
  600 bytes, so 200 workloads at the default 120 samples is on the order of
  15 MB.
- Parsing cost is linear in payload size. `BenchmarkParseV1` covers a realistic
  vLLM payload; run `make bench` for numbers on your hardware rather than
  trusting a figure written here.
- Rule evaluation is linear in workloads × rules × window length, and runs per
  API request. `BenchmarkAnalyze` covers fifty workloads with two minutes of
  history.
- Kubernetes load is one List plus a watch per resource type, not a List per
  interval.
- The likely first bottleneck is API-request rate, because evaluation is
  synchronous. If that becomes real, caching one analysis per scrape interval is
  the obvious fix; it has not been needed.
