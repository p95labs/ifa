# Security model

## What IFA is, from a security standpoint

A process that makes outbound HTTP GETs to endpoints an operator listed, watches
three Kubernetes resource types read-only, holds the results in memory, and
serves them over an optionally authenticated HTTP API.

## Read-only by construction

Not by policy — by the absence of any code that could do otherwise.

- **Kubernetes.** The client is used for `List` and `Watch` on deployments, pods
  and horizontalpodautoscalers. The ClusterRole in the chart grants `get`,
  `list`, `watch` and nothing else; you can read it in
  [`deploy/helm/ifa/templates/rbac.yaml`](../deploy/helm/ifa/templates/rbac.yaml)
  in under a minute. See [RBAC_PERMISSIONS.md](RBAC_PERMISSIONS.md).
- **The API.** Every handler is registered through a wrapper that rejects
  anything but GET and HEAD. Adding a mutating endpoint means changing that
  wrapper, which is a visible diff.
- **Scrape targets.** IFA only ever issues GETs, and only to URLs in its
  configuration.

## What IFA never sees

Prompt text, completion text, request and response bodies, HTTP headers, user
identifiers, API keys.

This is a property of the interface, not a filter: IFA reads Prometheus counters
and gauges. Those are aggregate numbers. There is no code path that reads a
request body, because there is no request body to read.

The exceptions worth stating plainly: **model names** are read from the
`model_name` label and appear in the API, and **workload and namespace names**
appear throughout. If your model or namespace names are themselves sensitive,
the API is as sensitive as they are.

## Outbound connections

Only these, and no others:

1. The `metrics_url` and `dcgm_url` of each configured target.
2. The Kubernetes API server, when discovery is enabled.
3. The database, when history is enabled.

No licence check, no usage telemetry, no update check, no crash reporting. IFA
runs in an air-gapped cluster without configuration beyond mirroring the image;
see [DEPLOYMENT.md](DEPLOYMENT.md).

## Container and pod posture

The image is `gcr.io/distroless/static-debian12:nonroot` — no shell, no package
manager, no `exec` target. The chart sets:

```yaml
runAsNonRoot: true
runAsUser: 65532
readOnlyRootFilesystem: true
allowPrivilegeEscalation: false
capabilities: { drop: [ALL] }
seccompProfile: { type: RuntimeDefault }
```

`/tmp` is an in-memory `emptyDir` because the root filesystem is read-only. The
image runs as uid 65532 to match the `securityContext`; a mismatch there is the
usual cause of a `CreateContainerConfigError`.

## Handling untrusted input

Scrape payloads are attacker-influenced: whoever controls a watched pod controls
what IFA parses.

- **Bounded reads.** Every response is capped (`max_body_bytes`, default 8 MiB)
  and read through an `io.LimitReader`; a body that exceeds the cap is refused
  rather than truncated and misinterpreted.
- **Bounded time.** Every scrape carries a hard timeout, and the timeout is
  validated to be shorter than the interval so a hung target cannot cause
  scrapes to pile up.
- **No redirect following.** A target cannot send the collector somewhere it was
  not configured to go.
- **Scheme restriction.** Target URLs must be `http` or `https` with a host,
  checked at config load. Scrape targets come from a ConfigMap, which more
  people can usually edit than can edit the Deployment.
- **Parser.** No regular expressions, no reflection, no recursion. Allocation is
  proportional to input, and input is capped. Malformed lines are counted and
  skipped; a single bad line from an unrelated exporter does not blind the
  collector to the rest of the payload.
- **Nothing parsed is executed.** Values become floats and labels become map
  keys. Nothing is interpolated into a query — the database sink uses bound
  parameters exclusively — or written to disk.

## API authentication

Data endpoints (`/api/v1/telemetry`, `/api/v1/recommendations`,
`/api/v1/workloads`, `/api/v1/rules`, and their legacy equivalents) can require
a Bearer token. Set `api_auth.enabled: true` and supply the token via
`IFA_API_TOKEN` (never in the ConfigMap).

Health probes (`/api/v1/healthz`, `/api/v1/readyz`) and `/metrics` remain
unauthenticated — Kubernetes sends probes without credentials and Prometheus
scrapes without headers by default.

The middleware uses `crypto/subtle.ConstantTimeCompare` to prevent timing-based
token enumeration. Tokens passed as query parameters are silently ignored;
only the `Authorization: Bearer` header is accepted, because query strings
appear in access logs and browser history.

A 401 response reveals only `{"error":{"code":"unauthorized","message":"valid
Bearer token required"}}` — it does not distinguish a missing header from a
wrong token, to avoid helping an attacker enumerate.

The CLI reads `IFA_API_TOKEN` from the environment and adds the header
automatically. There is deliberately no `-token` flag: flags appear in process
listings and shell history.

## Secrets

Three credentials IFA can hold, all sourced from environment variables:

| Secret | Env var | Chart value |
|--------|---------|-------------|
| Database DSN | `IFA_DATABASE_DSN` | `database.existingSecret` |
| Webhook URL | `IFA_ALERTING_WEBHOOK_URL` | `alerting.existingSecret` |
| API bearer token | `IFA_API_TOKEN` | `apiAuth.existingSecret` |

All three are read from the environment (a Kubernetes Secret injected via
`secretKeyRef`), never from the ConfigMap. All three are redacted or absent
from startup logs.

## Server hardening

`ReadHeaderTimeout` is set (default 5s), which is the one HTTP timeout whose
absence lets a single idle connection hold a goroutine open indefinitely.
`ReadTimeout`, `WriteTimeout`, `IdleTimeout` and `MaxHeaderBytes` are also set.

---

## What this does not defend against

Stated plainly, because a security section that lists only strengths is not one.

**The API can be authenticated but not encrypted.** With `api_auth.enabled: true`
and a sufficiently strong token, data endpoints require a correct Bearer token.
They are still unencrypted: anyone who can observe traffic on the network
can read both the token and the data. Put TLS termination in front — an
ingress controller or a service mesh sidecar — before exposing the API outside
the cluster. Even inside the cluster, prefer a NetworkPolicy that limits which
namespaces can reach IFA's port. Operational metadata (workload names, model
names, namespaces, queue depths) is not a credential but it is reconnaissance.

**Denial of service against the API.** There is no rate limiting and no request
quota. `/api/v1/recommendations` runs the rule engine synchronously, so it is the
most expensive endpoint and the obvious target. Restrict who can reach it.

**A compromised scrape target can degrade IFA.** It cannot execute anything, but
it can serve 8 MiB of valid exposition on every scrape and consume parsing time,
or serve values engineered to trigger findings. Nothing IFA does in response
changes any state, so the blast radius is misleading output.

**A stolen ServiceAccount token grants read access.** The token can list
deployments, pods and HPAs in scope. Pod specs can contain environment variables,
so this is not nothing. Prefer `rbac.scope: namespace`.

**Findings can be wrong.** The rules are thresholds over interpolated
percentiles from someone else's metrics. Acting on a finding without checking
its evidence is acting on a guess. This is a large part of why IFA is read-only
([ADR 0001](adr/0001-read-only.md)) and why every finding carries the numbers
behind it.

**No audit log.** IFA does not record who queried it or what was returned.

**No rate limiting.** `/api/v1/recommendations` runs the rule engine
synchronously on every request. A client that polls it in a tight loop consumes
meaningful CPU. Restrict who can reach the port or put a rate-limiting proxy in
front.

**Supply-chain attestation.** The release workflow signs container images by
immutable digest using Sigstore Cosign keyless OIDC signing and attests a
Syft-generated SPDX JSON SBOM. The signing identity is
`https://github.com/p95labs/ifa/.github/workflows/release.yml` under
`https://token.actions.githubusercontent.com`. No private key is stored.

This applies to images published from a release tag (`v0.3.0` onwards).
The `v0.2.0-alpha.1` image predates this change and carries no signature.

Verification: see `scripts/verify-release.sh` and `docs/DEPLOYMENT.md`.


## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
