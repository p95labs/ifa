# Deployment

## Helm

```bash
helm install ifa deploy/helm/ifa \
  --namespace inference --create-namespace \
  --set image.tag=v0.2.0 \
  --values my-values.yaml
```

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
| `database.existingSecret` | `""` | Secret holding the DSN under `database.secretKey` |
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
./scripts/package-offline.sh v0.2.0
```

produces, in `dist/`:

- `ifa-image-v0.2.0.tar` — `docker save` output
- `ifa-0.2.0.tgz` — the packaged chart
- `SHA256SUMS`

On the target:

```bash
docker load -i ifa-image-v0.2.0.tar
# or, for containerd:  ctr -n k8s.io images import ifa-image-v0.2.0.tar

# Push to an internal registry, then:
helm install ifa ifa-0.2.0.tgz \
  --namespace inference --create-namespace \
  --set image.registry=registry.internal.example.com \
  --set image.repository=ghcr.io/p95labs/ifa \
  --set image.tag=v0.2.0 \
  --values my-values.yaml
```

If images are loaded directly onto nodes rather than pushed to a registry, set
`image.pullPolicy: Never`.

There is no "air-gapped mode" flag. Air-gapped operation is a property of the
deployment — no egress, mirrored images, a NetworkPolicy — not a runtime setting,
and a configuration field that claimed to enforce it while enforcing nothing
would be security theatre.

## Optional history

```bash
kubectl -n inference create secret generic ifa-db \
  --from-literal=dsn='postgres://ifa:...@timescaledb.timescaledb.svc:5432/ifa?sslmode=require'

psql "$DSN" -f migrations/001_init.sql
```

```yaml
database:
  existingSecret: ifa-db
config:
  database:
    enabled: true
    queue_size: 1024
```

IFA never runs migrations. Granting the control plane DDL rights on a database
it only ever appends to would widen its blast radius for no benefit.

The rule engine never reads from the database — it only reads the in-memory
window — so an outage costs history, not diagnostics. Watch
`ifa_database_dropped_total` to know whether history is complete. Startup *does*
fail if the database is enabled and unreachable, because enabling history and
silently not writing it is worse than a clear failure.

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
