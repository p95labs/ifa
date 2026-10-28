# Deployment

## Helm

```bash
helm install ifa deploy/helm/ifa \
  --namespace inference --create-namespace \
  --set image.tag=0.2.0-alpha.1 \
  --values my-values.yaml
```

**The image tag has no `v` prefix.** The git tag is `v0.2.0-alpha.1`; the
published GHCR tag is `0.2.0-alpha.1`. Passing `image.tag=v0.2.0-alpha.1`
produces an `ImagePullBackOff`. Leaving `image.tag` unset falls back to the
chart's `appVersion`, which is already the correct form — pin it anyway, so an
upgrade of the chart does not silently change the rule set.

A minimal `my-values.yaml`:

```yaml
config:
  cluster_name: prod-eu-west
  kubernetes:
    enabled: true
    namespace: inference
    label_selector: inference.io/runtime
  collector:
    targets:
      - name: chat-llama3-8b
        namespace: inference
        runtime: vllm
        model_name: meta-llama/Llama-3.1-8B-Instruct
        metrics_url: http://chat-llama3-8b.inference.svc:8000/metrics
        dcgm_url: http://dcgm-exporter.gpu-operator.svc:9400/metrics
```

The chart installs one Deployment, a Service, a ServiceAccount and a Role. There
is no DaemonSet and no node agent: everything IFA reads comes from HTTP endpoints
that already exist in the cluster, so there is nothing for a per-node component
to do.

`helm install` runs `values.schema.json`, so a target with a `file://` URL or a
runtime that does not exist is rejected before it reaches the cluster.

### Values worth knowing about

| Value | Default | Notes |
|---|---|---|
| `image.tag` | chart `appVersion` | Pin it. A diagnostics tool that silently changes its rule set is worse than one that needs an upgrade. |
| `rbac.scope` | `namespace` | `cluster` only when watching all namespaces — see [RBAC_PERMISSIONS.md](RBAC_PERMISSIONS.md) |
| `networkPolicy.enabled` | `false` | Read the section below before enabling |
| `serviceMonitor.enabled` | `false` | Needs the Prometheus Operator CRDs |
| `database.existingSecret` | `""` | Secret holding the DSN under `database.secretKey`. Set `config.database.enabled` too |
| `alerting.existingSecret` | `""` | Secret holding the webhook URL under `alerting.secretKey`. Set `config.alerting.enabled` too. The URL *is* the credential for Slack, Teams and PagerDuty — do not put it in `config.alerting.webhook_url`, which is rendered into a ConfigMap |
| `podDisruptionBudget.enabled` | `false` | Only meaningful with `replicaCount > 1` |

### Replicas

The Deployment uses the `Recreate` strategy and defaults to one replica.

IFA holds telemetry in memory. Two replicas do not share it: each builds its own
window, so the same workload can produce different findings depending on which
pod answers, and a rolling update briefly serves from a replica with an empty
store. One replica, recreated on update, is the honest configuration. Losing it
for a few seconds costs you a few seconds of diagnostics.

### Without Helm

```bash
helm template ifa deploy/helm/ifa -f my-values.yaml > ifa.yaml
kubectl apply -f ifa.yaml
```

Raw manifests are not checked in separately — a second copy of the same YAML
drifts from the chart within a release or two.

## NetworkPolicy

Off by default, because a policy that is subtly wrong silently stops IFA
scraping the workloads it exists to watch, which looks exactly like every target
being down.

**The mistake to avoid:** IFA scrapes the inference runtimes *directly*, on their
own metrics ports. It does not go through Prometheus. A policy that allows egress
only to DNS, the API server and Prometheus — the obvious first draft, and what an
earlier version of this chart shipped — makes every target unreachable.

```yaml
networkPolicy:
  enabled: true
  scrapeNamespaces: [inference]      # where the vLLM/Triton pods live
  scrapePorts: [8000, 8002]          # their metrics ports, not Prometheus's
  dcgmNamespaces: [gpu-operator]
  dcgmPort: 9400
  databaseNamespace: timescaledb     # only if history is enabled
  ingressNamespaces: [monitoring]    # who may reach IFA's API
```

Enforcement needs a CNI that implements NetworkPolicy — Calico, Cilium, Antrea.
Flannel accepts the object and does not enforce it, which is indistinguishable
from a working policy when viewed through `kubectl`. Verify with a connectivity
test, then verify IFA is still scraping:

```bash
kubectl -n inference exec deploy/ifa -- true 2>/dev/null || \
  kubectl -n inference port-forward svc/ifa 8080:8080 &
curl -s localhost:8080/metrics | grep ifa_scrape_errors_total
```

(The image has no shell, so `exec` is not available — port-forward and read the
metrics instead.)

## Offline and air-gapped clusters

IFA makes no outbound connections of its own: no licence check, no telemetry, no
update check. The only things that need to cross the boundary are the container
image and the chart.

```bash
./scripts/package-offline.sh v0.2.0-alpha.1
```

produces, in `dist/`:

- `ifa-image-v0.2.0-alpha.1.tar` — `docker save` output
- `ifa-0.2.0-alpha.1.tgz` — the packaged chart (the script strips the leading
  `v`, because Helm chart versions must be bare SemVer)
- `SHA256SUMS`

Note that this script *builds* the image locally, so it tags it exactly as you
asked — `v` prefix included. That is why `image.tag` below keeps the `v` while
the registry install above does not: the tag you load is the tag you set.

On the target:

```bash
docker load -i ifa-image-v0.2.0-alpha.1.tar
# or, for containerd:  ctr -n k8s.io images import ifa-image-v0.2.0-alpha.1.tar

# Push to an internal registry, then:
helm install ifa ifa-0.2.0-alpha.1.tgz \
  --namespace inference --create-namespace \
  --set image.registry=registry.internal.example.com \
  --set image.repository=ghcr.io/p95labs/ifa \
  --set image.tag=v0.2.0-alpha.1 \
  --values my-values.yaml
```

If images are loaded directly onto nodes rather than pushed to a registry, set
`image.pullPolicy: Never`.

There is no "air-gapped mode" flag. Air-gapped operation is a property of the
deployment — no egress, mirrored images, a NetworkPolicy — not a runtime setting,
and a configuration field that claimed to enforce it while enforcing nothing
would be security theatre.

## Optional history

IFA can write every scrape snapshot to a TimescaleDB database for long-term
retention and ad-hoc SQL queries. This feature is validated against
**timescale/timescaledb:2.14.2-pg16** (PostgreSQL 16.2, TimescaleDB 2.14.2).

### What it does and does not do

- Every snapshot that the collector produces is forwarded to TimescaleDB over a
  bounded asynchronous queue (default depth: 1024). Inserts are non-blocking.
- The **rule engine reads only the in-memory store**, not the database. A
  database outage degrades history retention, not live diagnostics.
- IFA does **not** guarantee zero data loss during an outage. Snapshots that
  cannot be queued are dropped and counted in `ifa_database_dropped_total`.
  Snapshots that are queued but not yet written when the database goes away are
  also lost. This is intentional — an unbounded retry queue would consume
  unbounded memory and backfill stale telemetry behind fresh telemetry when the
  database recovers.

### Step 1 — Apply the migration

IFA never applies migrations itself. Granting the control plane DDL rights on a
database it only ever appends to would widen its blast radius for no benefit.

Apply `migrations/001_init.sql` as the database owner before enabling IFA:

```bash
psql "postgres://dbowner:...@timescaledb.svc:5432/ifa?sslmode=require" \
  -f migrations/001_init.sql
```

The migration is idempotent (`CREATE … IF NOT EXISTS`, `if_not_exists => TRUE`),
so re-running it on an existing database is safe. To verify it applied:

```sql
-- Confirm TimescaleDB extension is present.
SELECT extname, extversion FROM pg_extension WHERE extname = 'timescaledb';

-- Confirm telemetry_snapshots is a hypertable (not a plain table).
SELECT hypertable_name FROM timescaledb_information.hypertables
WHERE hypertable_name = 'telemetry_snapshots';

-- Confirm the workload-history index exists.
SELECT indexname FROM pg_indexes
WHERE tablename = 'telemetry_snapshots' AND indexname = 'idx_telemetry_workload';
```

### Step 2 — Create the Kubernetes Secret

The DSN carries a password. Store it in a Secret rather than a ConfigMap:

```bash
kubectl -n inference create secret generic ifa-db \
  --from-literal=dsn='postgres://ifa:...@timescaledb.svc:5432/ifa?sslmode=require'
```

### Step 3 — Enable in Helm values

```yaml
database:
  existingSecret: ifa-db   # name of the Secret above
  secretKey: dsn            # key inside the Secret (default: dsn)

config:
  database:
    enabled: true
    queue_size: 1024        # snapshots buffered before dropping; default 1024
```

IFA reads the DSN from the environment variable `IFA_DATABASE_DSN`, which the
Helm chart injects via `secretKeyRef`. The DSN never appears in the ConfigMap.

Startup **fails immediately** if the database is enabled and unreachable. This
is intentional: enabling history and silently not writing it is worse than a
clear failure at startup.

### Querying stored history

```sql
-- Most recent snapshots for one workload.
SELECT time, request_rate_per_sec, kv_cache_usage_percent, gpu_utilization_percent
FROM telemetry_snapshots
WHERE namespace = 'inference' AND workload_name = 'chat-llama3'
ORDER BY time DESC
LIMIT 100;

-- Average KV-cache usage per workload over the last hour.
-- AVG() skips NULLs, so unmeasured values do not drag the average to zero.
SELECT workload_name, AVG(kv_cache_usage_percent) AS avg_kv
FROM telemetry_snapshots
WHERE time > NOW() - INTERVAL '1 hour'
GROUP BY workload_name;
```

**NULL versus zero.** Every measurement column is nullable. `NULL` means "the
runtime did not report this metric for this scrape". Numeric `0` means "the
runtime reported it and it was zero". Always use `AVG(col)` rather than
`AVG(COALESCE(col, 0))` — the latter silently turns missing data into idle-looking
data and produces wrong averages.

### Monitoring database health

Three Prometheus counters track the sink:

| Metric | Meaning |
|---|---|
| `ifa_database_written_total` | Snapshots successfully persisted |
| `ifa_database_failed_total` | Insert errors (timeout, connection refused, etc.) |
| `ifa_database_dropped_total` | Snapshots discarded because the queue was full |

Non-zero `ifa_database_dropped_total` means the database is too slow or
unreachable and the queue has filled. Non-zero `ifa_database_failed_total` means
individual inserts are failing — check database connectivity and logs.

### Outage behaviour

When the database becomes unavailable after a successful startup:

1. Pending inserts time out (default: 5 s per insert) and are counted as failed.
2. New snapshots enqueue normally. If the queue fills before the database
   recovers, additional snapshots are dropped.
3. The **collector, rule engine, and REST API are unaffected** — they read the
   in-memory store, not the database.
4. When the database recovers, the next insert succeeds and the sink resumes
   without any restart.

Historical snapshots written before the outage remain in the database. Snapshots
that were dropped during the outage are permanently lost; IFA does not replay
them when the connection is restored.

### Reproducing the integration tests locally

```bash
# Requires Docker and Go (same version as CI).
bash scripts/timescale-integration.sh
```

The script starts a `timescale/timescaledb:2.14.2-pg16` container, applies the
migration, runs `go test -tags integration -race ./internal/storage/timescale/...`,
verifies the Helm Secret wiring, and cleans up. It exits 0 only when all tests
pass.

## Verifying an install

```bash
kubectl -n inference get pods -l app.kubernetes.io/name=ifa
kubectl -n inference port-forward svc/ifa 8080:8080

curl -s localhost:8080/api/v1/healthz | jq .config
curl -s localhost:8080/api/v1/telemetry | jq '.count'
curl -s localhost:8080/metrics | grep ifa_scrape_errors_total
```

`healthz` echoes the configuration the process actually loaded, which is the
fastest way to confirm the ConfigMap you edited is the one running.

If `count` is 0 after a scrape interval, [OPERATIONS.md](OPERATIONS.md) has the
diagnosis path.
