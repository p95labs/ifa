# Configuration

The annotated reference is [`config.yaml`](../config.yaml) in the repository
root; it is the file you edit and it documents every key inline. This page
covers the rules that govern the file as a whole.

## Loading

```bash
control-plane -config /etc/ifa/config.yaml
```

- A path given with `-config` **must exist**. The default path (`config.yaml`)
  need not: the built-in defaults are a valid, inert configuration.
- **Unknown keys are a startup error.** A mistyped `kv_cache_high_pc` that was
  silently ignored would leave you convinced you had tuned something you had
  not. The error names the offending line.
- Durations are written as durations — `15s`, `2m30s`, `1h` — not as
  `*_seconds` integers. A field called `interval_seconds` holding `300` is read
  as five minutes by nobody.
- Validation runs at startup. A configuration that would make rules unfireable
  is rejected there rather than producing silence later.

## Environment overrides

Only the values that genuinely differ per deployment:

| Variable | Overrides |
|---|---|
| `IFA_DATABASE_DSN` | `database.dsn` |
| `IFA_ALERTING_WEBHOOK_URL` | `alerting.webhook_url` |
| `IFA_LOG_LEVEL` | `logging.level` |
| `IFA_LOG_FORMAT` | `logging.format` |
| `IFA_ADDRESS` | `server.address` |
| `IFA_CLUSTER_NAME` | `cluster_name` |

The DSN and the webhook URL are here because both *are* credentials — the DSN
carries a password, and Slack, Teams and PagerDuty all embed the token in the
webhook path — so both belong in a Secret rather than in the ConfigMap the rest
of the configuration lives in. The chart wires them from
`database.existingSecret` and `alerting.existingSecret`. Everything else stays in
the file, where it can be reviewed in a pull request.

## Scrape targets

IFA watches nothing until targets are listed. There is no discovery-based
auto-targeting: a tool that starts scraping things nobody told it about is a
surprise, and in a cluster with mixed workloads it is a wrong one.

```yaml
collector:
  targets:
    - name: chat-llama3-8b        # identifies the workload in the API
      namespace: inference
      runtime: vllm               # vllm | triton
      model_name: meta-llama/Llama-3.1-8B-Instruct
      metrics_url: http://chat-llama3-8b.inference.svc:8000/metrics
      dcgm_url: http://dcgm-exporter.gpu-operator.svc:9400/metrics
      deployment: chat-llama3-8b  # defaults to `name`
```

`model_name` is **required when one server hosts several models.** vLLM labels
every series with it; without the filter the adapter aggregates across models and
produces a queue depth and a cache utilisation that belong to no actual
workload.

`dcgm_url` is optional but consequential: without it, GPU utilisation and memory
are not measured at all, and `IFA-CAP-001`, `IFA-CAP-002`, `IFA-EFF-002` and
`IFA-EFF-003` never run. IFA reports them as unmeasured rather than substituting
KV-cache occupancy, which is not GPU utilisation however similar the units look.

`deployment` is how replica counts are joined. Set it when the Deployment is
named differently from the target.

Before adding a target, check it:

```bash
ifa check http://chat-llama3-8b.inference.svc:8000/metrics -runtime vllm
```

## Interacting constraints

Several values have to agree with each other. Validation enforces all of these
at startup and names the fields involved.

**`collector.timeout` < `collector.interval`.** Otherwise a stuck target keeps a
scrape in flight past the point where the next one begins, and they overlap
indefinitely.

**`telemetry.retention_period` ≥ 3 × `recommender.thresholds.sustain_for`.**
Trend-based rules look back three sustain windows; if retention is shorter they
can never accumulate the history to fire.

**`collector.interval` ≪ `sustain_for`.** Not enforced, but a sustained rule
needs several samples inside its window, and `IFA-CAP-003` needs at least six.
A 60s interval with a 45s sustain window means no sustained rule ever fires. Keep
the interval below roughly `sustain_for / 6`.

**`gpu_util_low_pct` < `gpu_util_high_pct`.** With the bands inverted, a workload
is simultaneously "idle" and "saturated" and both queue rules fire on it, which
is worse than either firing alone.

## Thresholds

Every threshold, with the reasoning behind its default, is documented per rule
in [RECOMMENDATIONS.md](RECOMMENDATIONS.md). The defaults assume interactive
serving; that page has a starting point for batch workloads.

To see what a running instance is actually using:

```bash
curl -s localhost:8080/api/v1/rules | jq .thresholds
```

The keys and duration formats in that response match the config file exactly, so
you can diff them.

## Retention

```yaml
telemetry:
  retention_samples: 120
  retention_period: 15m
```

Bounded twice over, because the failure modes differ. The sample bound caps
memory on a fast-scraping deployment; the age bound stops a workload that
stopped reporting from keeping stale samples alive and being diagnosed on them.
Whichever binds first wins.

A snapshot is roughly 600 bytes, so 200 workloads at the default is on the order
of 15 MB.

## Helm

The chart carries the same structure under `config:` and renders it verbatim
into a ConfigMap. A test in this repository loads the chart's block through the
real config loader, so the two cannot drift apart without the build failing —
which matters because unknown keys are fatal, and a rename in Go that missed the
chart would produce a pod that crash-loops on startup.

The Deployment carries a checksum annotation over the ConfigMap, so `helm
upgrade` with a changed threshold restarts the pods. Without it the ConfigMap
updates and the running process keeps the old values.
