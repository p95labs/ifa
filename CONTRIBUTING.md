# Contributing

IFA is alpha. The API, the data model and the rule set are all still moving, so
open an issue before starting anything non-trivial — it saves you the work if it
conflicts with where the project is going. [docs/ROADMAP.md](docs/ROADMAP.md)
says where that is.

## The most useful contribution right now

Not code: **`ifa check` output from a real vLLM or Triton server.**

Every adapter here is built and tested against fixtures constructed from each
runtime's own metric definitions. That catches a lot, but a fixture built from a
spec and a real payload are not the same artifact — the first version of this
project's vLLM adapter passed all of its tests and could not read a real server,
because real vLLM labels every series and the fixture did not.

```bash
ifa check http://your-vllm:8000/metrics -runtime vllm -model your-model
```

Paste the output and the runtime version into an issue. It takes ten seconds and
it is the difference between "implemented" and "validated" on the roadmap.

## Hard constraints

Changes that violate these will not be merged.

- **Read-only.** IFA never creates, updates, patches or deletes anything in
  Kubernetes, and never execs into a pod. The permission set is `get`, `list`,
  `watch`. See [ADR 0001](docs/adr/0001-read-only.md).
- **No remediation.** Output is advice for a human. Automation belongs on top of
  the API, not inside the process.
- **No payload collection.** No prompt bodies, response bodies, request headers
  or user identifiers. IFA reads aggregate counters and gauges.
- **No invented data.** If a runtime does not expose a signal, the field stays
  unmeasured. Do not derive an error rate from a success counter, publish a mean
  as a percentile, or substitute one percentage for another because the units
  match. This is the single easiest way to make the project untrustworthy.
- **Small dependency set.** The runtime dependencies are client-go, pgx and
  yaml. Adding one needs a reason in the pull request.

## Getting set up

Go 1.25 or newer, and nothing else for building, testing and running the demo.

```bash
git clone https://github.com/p95labs/ifa
cd ifa
make demo      # see it work
make verify    # what CI runs
```

[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) covers the layout, and has worked
examples for the two most common changes: adding a rule, and adding a runtime
adapter.

## What a good change looks like

**A new rule** combines signals where one is ambiguous, requires the condition to
persist, carries the numbers that triggered it, and suggests something a human
can do. It is tested at the boundary, one step below the boundary, with a
required input unmeasured, and against the healthy control. It is documented in
[docs/RECOMMENDATIONS.md](docs/RECOMMENDATIONS.md) in the same pull request.

**A new runtime adapter** is a pure function with a fixture built from that
runtime's own metric definitions, including the labels it really emits. It
reports missing metrics rather than defaulting them to zero.

**A bug fix** comes with the test that fails without it. If the bug was a
misreading of a runtime's semantics — a fraction treated as a percentage, a
counter treated as a gauge — say so in a comment where the fix is, so the next
person does not re-derive it.

## Pull requests

- One logical change per PR.
- `make verify` passes.
- Commit messages explain why, not what; the diff already says what.
- Comments in code explain why, not what. If a comment restates the line below
  it, delete one of them.

## Code of conduct

[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Licence

Contributions are accepted under Apache 2.0.
