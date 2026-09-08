// Package alerting delivers recommendations to an HTTP endpoint.
//
// Two properties shape the whole package.
//
// The rule engine is stateless: it re-derives every finding from the current
// telemetry each time it runs. A finding that holds for an hour is therefore
// produced by every evaluation for that hour, and a sender that posted whatever
// it was handed would deliver the same alert every collection interval. The
// deduplication state here is what turns a stream of repeated evaluations into
// transitions. See docs/adr/0006-alert-deduplication.md.
//
// Delivery is off the collector's path entirely. A webhook endpoint is a third
// party that IFA does not control, and a diagnostics tool that stops collecting
// telemetry because Slack is slow has failed at its actual job. Alerts go onto a
// bounded queue and are sent by one goroutine, following the same
// bounded-queue-and-single-writer shape as internal/storage/timescale.
package alerting

import (
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

// SchemaVersion identifies the payload shape. It is part of the contract with
// whatever is on the other end of the webhook: a subscriber can refuse a
// version it does not understand instead of silently misreading a field.
const SchemaVersion = 1

// Event describes what happened to a finding, rather than what the finding is.
//
// A subscriber that only wants to be woken up filters on EventFiring and
// EventEscalated. One that maintains its own view of open findings needs
// EventResolved too.
type Event string

const (
	// EventFiring is sent the first time a finding appears.
	EventFiring Event = "firing"
	// EventEscalated is sent when a finding that was already open increases in
	// severity — a warning that has become critical. The payload carries the
	// severity it held before.
	EventEscalated Event = "escalated"
	// EventResolved is sent when a finding that was open is no longer produced.
	// It carries the finding's identity and last known severity, but no
	// evidence: there is no current measurement to report, and inventing one
	// would be a lie about the moment the alert was sent.
	EventResolved Event = "resolved"
)

// Payload is the JSON body POSTed to the webhook.
//
// It is declared here as its own type rather than by marshalling
// telemetry.Recommendation directly. Those two things have different audiences:
// the internal type is free to change shape as rules evolve, and the wire format
// is a contract with someone else's pipeline. Marshalling the internal type
// would make any refactor of it a silent breaking change for every subscriber.
type Payload struct {
	SchemaVersion int   `json:"schema_version"`
	Event         Event `json:"event"`

	// ID is stable for as long as the underlying condition holds, and is
	// derived from the rule code and the workload. It is the key a subscriber
	// should deduplicate or correlate on.
	ID       string `json:"id"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	// PreviousSeverity is set only on EventEscalated and EventResolved.
	PreviousSeverity string `json:"previous_severity,omitempty"`

	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Workload  string `json:"workload"`
	Runtime   string `json:"runtime,omitempty"`

	Title string `json:"title"`
	// Explanation and SuggestedAction are empty on EventResolved.
	Explanation     string `json:"explanation,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`

	Evidence []Evidence `json:"evidence,omitempty"`

	// ObservedAt is the timestamp of the telemetry that triggered the rule.
	// Zero on EventResolved.
	ObservedAt time.Time `json:"observed_at,omitempty"`
	// SentAt is when IFA built the payload. The gap between the two is scrape
	// latency plus queue time, which is worth being able to see.
	SentAt        time.Time `json:"sent_at"`
	WindowSeconds int       `json:"window_seconds,omitempty"`
}

// Evidence is one measurement behind a finding, with the threshold it was
// compared against. It mirrors telemetry.Evidence field for field, and is
// duplicated for the same reason Payload is.
type Evidence struct {
	Metric     string  `json:"metric"`
	Source     string  `json:"source,omitempty"`
	Observed   float64 `json:"observed"`
	Threshold  float64 `json:"threshold,omitempty"`
	Comparison string  `json:"comparison,omitempty"`
	Unit       string  `json:"unit,omitempty"`
}

// firingPayload builds the payload for a new or escalated finding.
func firingPayload(rec telemetry.Recommendation, event Event, previous telemetry.Severity, cluster string, now time.Time) Payload {
	p := Payload{
		SchemaVersion:   SchemaVersion,
		Event:           event,
		ID:              rec.ID,
		Code:            rec.Code,
		Severity:        string(rec.Severity),
		Cluster:         cluster,
		Namespace:       rec.Namespace,
		Workload:        rec.WorkloadName,
		Runtime:         string(rec.Runtime),
		Title:           rec.Title,
		Explanation:     rec.Explanation,
		SuggestedAction: rec.SuggestedAction,
		ObservedAt:      rec.ObservedAt,
		SentAt:          now,
		WindowSeconds:   rec.WindowSeconds,
	}
	if event == EventEscalated {
		p.PreviousSeverity = string(previous)
	}
	for _, e := range rec.Evidence {
		p.Evidence = append(p.Evidence, Evidence{
			Metric:     e.Metric,
			Source:     e.Source,
			Observed:   e.Observed,
			Threshold:  e.Threshold,
			Comparison: e.Comparison,
			Unit:       e.Unit,
		})
	}
	return p
}

// resolvedPayload builds the payload for a finding that has stopped being
// produced, from the identity retained while it was open.
func resolvedPayload(a openAlert, cluster string, now time.Time) Payload {
	return Payload{
		SchemaVersion:    SchemaVersion,
		Event:            EventResolved,
		ID:               a.id,
		Code:             a.code,
		Severity:         string(telemetry.SeverityInfo),
		PreviousSeverity: string(a.severity),
		Cluster:          cluster,
		Namespace:        a.namespace,
		Workload:         a.workload,
		Runtime:          a.runtime,
		Title:            a.title,
		SentAt:           now,
	}
}

// openAlert is the identity retained for a finding that has been sent and not
// yet resolved.
//
// It holds the fields a resolved payload needs rather than the whole
// Recommendation, because the Recommendation is gone by the time the resolution
// is noticed — the rule stopped producing it, which is how the resolution is
// detected in the first place.
type openAlert struct {
	id        string
	code      string
	severity  telemetry.Severity
	namespace string
	workload  string
	runtime   string
	title     string
}

func newOpenAlert(rec telemetry.Recommendation) openAlert {
	return openAlert{
		id:        rec.ID,
		code:      rec.Code,
		severity:  rec.Severity,
		namespace: rec.Namespace,
		workload:  rec.WorkloadName,
		runtime:   string(rec.Runtime),
		title:     rec.Title,
	}
}
