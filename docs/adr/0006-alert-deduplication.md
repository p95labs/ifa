# 0006 — Alert deduplication via state transitions

**Status:** accepted

## Context

IFA evaluates recommendations on a fixed collection interval. A condition that
has been firing for an hour produces a `Recommendation` on every cycle, which
means a sender that posts whatever it is handed would deliver the same alert
every 15 seconds (the default interval) until the condition clears.

Continuous re-posting has two practical costs: it fills an operator's inbox in
minutes, making the channel useless, and it makes it impossible to tell whether
a new alert is the same condition continuing or a fresh occurrence.

There is also an architectural choice forced by alerting: the recommender today
is **lazy** — `Analyze` runs only when the API is queried. To alert, evaluation
must be **eager**, running after every collection cycle regardless of whether
anyone is watching the API. The two evaluation instants are then different
(background cycle vs. live request) and can briefly disagree. This is acceptable
provided the distinction is explicit.

## Decision

The alerting sender maintains a map of open alerts keyed by `Recommendation.ID`.
It fires a webhook only on transitions:

- **Firing** — the finding appears and was not open.
- **Escalated** — the finding was open at a lower severity and rose.
- **Resolved** — a finding that was open is absent from the current evaluation.

A condition that persists unchanged produces a single firing alert and nothing
further. A condition that clears produces a resolved alert. Escalation is a
separate event rather than a resolve+re-fire so the receiver can correlate them.

The recommender is made eager by a new `OnCycle func()` hook in
`collector.Options` that fires after each completed scrape cycle. When alerting
is enabled, `main` registers a callback that calls `Analyze` and then
`Sender.Notify`. The API server continues to call `Analyze` lazily on each
request; the two paths share the same recommender but are otherwise independent.

Delivery is off the hot path: the sender has its own goroutine and a bounded
in-memory queue, so a slow or unavailable webhook cannot stall collection.

## Why

Transition-based deduplication matches how operators work with on-call tooling.
PagerDuty, Alertmanager, and OpsGenie all model alerts as open/closed state
machines, not as event streams. Sending transitions keeps IFA's output
compatible with those tools regardless of how they are configured downstream.

Keying on `Recommendation.ID` — which is stable for a given rule firing on a
given workload — is correct because the ID is the thing the operator acts on.
Two separate workloads with the same rule fire two separate alerts because their
IDs differ; one workload that triggers two rules fires two alerts for the same
reason.

The lazy-vs-eager split is worth naming because it is a source of potential
confusion. The choice is deliberate: the API stays responsive under load because
it does not block on a background ticker, and alerting fires at predictable
intervals because it does not depend on API traffic. The brief window where the
two paths disagree is bounded by one collection interval.

## Consequences

- A persistent condition produces O(1) alerts rather than O(evaluations).
- Resolved alerts are sent, so an operator knows when to close an incident
  without checking the API manually.
- Enabling alerting changes the recommender from lazy to eager. Evaluation cost
  per cycle is ~1 ms (`BenchmarkAnalyze`), which is negligible alongside the
  scrape cost but is worth noting for environments with many rules.
- The sender queue is bounded (`default: 256`). If the webhook endpoint is down
  for long enough that the queue fills, alerts are dropped and counted in
  `ifa_alerts_dropped_total`. An operator can detect a stalled alerting path via
  that counter before the endpoint comes back.
- The in-memory deduplication state is lost on restart. IFA will re-fire all
  open conditions as new alerts on the next cycle after restart. This is
  preferable to persisting state — a restart is a natural boundary, and
  suppressing alerts across a restart would hide conditions that started while
  IFA was down.

## Validation

All of the consequences above have been verified end to end by
`go test -tags integration -race ./internal/alerting/...`, which runs in CI
on every push in the `webhook-alerting` job. The test suite uses a full
in-process pipeline (collector → store → recommender → OnCycle → sender →
httptest.Server receiver) so alerts fire automatically without any direct
call to `Sender.Notify` from test code.

Verified specifically:

- **O(1) alerts for a persistent condition** — `TestIntegration_Alerting_Deduplication`
  runs many cycles and asserts exactly one firing event arrives.
- **Resolved alerts delivered** — `TestIntegration_Alerting_FiringAndResolution`
  clears the condition and waits for the `resolved` event.
- **Queue bounded at capacity** — `TestIntegration_Alerting_QueueSaturation`
  confirms `Notify` returns immediately and `ifa_alerts_dropped_total`
  increments.
- **Re-fire after restart** — `TestIntegration_Alerting_RestartRefiresOpenAlerts`
  recreates the sender and confirms the same finding fires again.
- **Escalation** — `TestIntegration_Alerting_Escalation` calls `Notify` with
  the same ID at warning then critical severity and confirms an `escalated`
  event with `previous_severity` set. Escalation is tested at the Sender
  integration layer because no built-in rule changes a finding's severity while
  preserving its ID.
- **Retry policy** — `TestIntegration_Alerting_HTTPFailureModes` confirms 4xx
  is not retried and 429/5xx gets one retry.
