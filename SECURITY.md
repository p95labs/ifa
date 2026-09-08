# Security Policy

## Status

Alpha software. It has not been through a security audit and the HTTP API is
unauthenticated. Deploy it where that is acceptable, and read
[docs/SECURITY_MODEL.md](docs/SECURITY_MODEL.md) — including the section on what
it does *not* defend against — before putting it in a production namespace.

## Supported versions

The most recent release only. There are no maintained release branches yet.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository
([Security → Report a vulnerability](https://github.com/p95labs/ifa/security/advisories/new)).
Please do not open a public issue for anything exploitable.

Include what you did, what happened, and what an attacker would gain. A proof of
concept helps but is not required.

This is a single-maintainer project: expect an acknowledgement within a few days
rather than within hours, and a fix on a best-effort timeline. If that is not
fast enough for your disclosure policy, say so in the report and we will agree
something.

## Scope

**In scope:** anything that lets IFA write to the Kubernetes API or its scrape
targets; anything that lets a scrape target execute code, escape the container,
or exhaust the host; credential leakage; a path where prompt or response content
reaches IFA.

**Known and out of scope**, because they are documented design limitations
rather than vulnerabilities:

- The HTTP API is unauthenticated and unencrypted. Access control is
  network-level. See the NetworkPolicy in the chart.
- There is no rate limiting on the API.
- A rule can produce a wrong finding. IFA is read-only precisely because it will
  sometimes be wrong; every finding carries the evidence behind it so you can
  check.
- Released images are not signed and no SBOM is published yet.

If you think one of those is worse than the documentation implies, that is worth
a report.

## What IFA guarantees

Verifiable rather than promised — the RBAC is one file and every handler goes
through one wrapper:

- No `create`, `update`, `patch` or `delete` on any Kubernetes resource, and no
  pod subresources.
- No exec into containers.
- No prompt bodies, response bodies, request headers or user identifiers.
- No outbound connections beyond the configured scrape targets, the Kubernetes
  API and the optional database. No licence check, no telemetry, no update
  check.

[docs/RBAC_PERMISSIONS.md](docs/RBAC_PERMISSIONS.md) lists every permission with
the `kubectl auth can-i` commands to confirm them yourself.
