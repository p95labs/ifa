# Documentation

Start with the [README](../README.md) and `make demo`.

## Using it

| | |
|---|---|
| [DEPLOYMENT.md](DEPLOYMENT.md) | Helm install, NetworkPolicy, offline clusters, optional history |
| [CONFIGURATION.md](CONFIGURATION.md) | How the config file behaves; the annotated reference is [`config.yaml`](../config.yaml) |
| [RUNTIMES.md](RUNTIMES.md) | What is read from vLLM, Triton and DCGM — and what has actually been validated |
| [API.md](API.md) | HTTP reference with real request and response examples |
| [OPERATIONS.md](OPERATIONS.md) | What to alert on, and how to diagnose IFA when it is the thing that is wrong |

## Understanding it

| | |
|---|---|
| [RECOMMENDATIONS.md](RECOMMENDATIONS.md) | All 19 rules: triggers, reasoning, suppression, tuning |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Components, data flow, concurrency, failure behaviour, scale |
| [SECURITY_MODEL.md](SECURITY_MODEL.md) | Threat model, and what is *not* defended against |
| [RBAC_PERMISSIONS.md](RBAC_PERMISSIONS.md) | Every Kubernetes permission, annotated |
| [ROADMAP.md](ROADMAP.md) | What is validated, what is merely implemented, and what is next |

## Decisions

Short records of the choices that shaped the design, with the reasoning:

- [0001 — Read-only, no remediation](adr/0001-read-only.md)
- [0002 — Runtime adapters as pure functions](adr/0002-runtime-adapters.md)
- [0003 — TimescaleDB optional and non-blocking](adr/0003-optional-timescaledb.md)
- [0004 — Deterministic rules, not learned models](adr/0004-deterministic-rules.md)
- [0005 — Measurements are optional values](adr/0005-optional-measurements.md)
- [0006 — Alert deduplication via state transitions](adr/0006-alert-deduplication.md)

## Contributing

[CONTRIBUTING.md](../CONTRIBUTING.md) for the process,
[DEVELOPMENT.md](DEVELOPMENT.md) for the local workflow and for how to add a rule
or a runtime adapter.
