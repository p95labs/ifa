// Package recommender turns telemetry into explained diagnoses.
//
// The design goal is that every output can be checked by the person reading it.
// A finding names the rule that produced it, the numbers that triggered it, and
// the thresholds they were compared against, so an operator can disagree with
// the conclusion without having to read the source.
//
// Three properties distinguish this from a set of Prometheus alerting rules,
// which is the obvious alternative and a perfectly good one:
//
//   - Rules combine signals. "Queue is deep" is not actionable; "queue is deep
//     while the GPU is idle" and "queue is deep while the GPU is saturated"
//     have opposite fixes, and the engine says which one it is.
//   - Rules require conditions to persist. A single scrape showing a deep queue
//     is normal scheduler behaviour.
//   - Rules refuse to fire on absent data. A runtime that does not export GPU
//     utilisation produces no GPU findings rather than findings about zero.
//
// Rules never mutate anything. The output is advice for a human.
package recommender

import (
	"fmt"
	"sort"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

// Code identifies a rule. Codes are permanent and documented in
// docs/RECOMMENDATIONS.md; renaming one is a breaking change for anyone who has
// built a filter or a runbook around it.
type Code string

// Rule is one diagnostic check.
type Rule struct {
	Code     Code
	Title    string
	Severity telemetry.Severity
	// Summary is a one-line description of what the rule detects. It is served
	// from /api/v1/rules and is the source for the rule catalogue in the docs.
	Summary string
	// Runtimes restricts the rule to specific inference runtimes. Empty means
	// it applies to all of them.
	Runtimes []telemetry.Runtime
	// Supersedes lists codes whose findings this rule replaces on the same
	// workload. It is how a specific diagnosis silences the generic symptom it
	// explains, so an operator reads one finding instead of five.
	Supersedes []Code
	// Eval returns a finding, or nil when the rule does not apply.
	Eval func(*Eval) *Finding
}

// Finding is a rule's output before it is decorated with workload identity.
type Finding struct {
	Explanation string
	Action      string
	Evidence    []telemetry.Evidence
	// Severity overrides the rule's default when a rule distinguishes degrees
	// of the same condition. Empty means use the rule's severity.
	Severity telemetry.Severity
	// Window is the span of telemetry the finding was sustained over.
	Window time.Duration
}

// Eval is the context handed to a rule.
type Eval struct {
	// Latest is the most recent snapshot for the workload.
	Latest telemetry.Snapshot
	// Window holds the retained snapshots for the workload, oldest first, with
	// Latest as the final element.
	Window []telemetry.Snapshot
	// T holds the configured thresholds.
	T Thresholds
	// Now is the evaluation time, injected so tests are deterministic.
	Now time.Time
}

// Span returns the duration covered by the sustain window that rules evaluate
// over — the tail of the retained history, not the whole of it.
func (e *Eval) Span() time.Duration {
	return span(e.recent(e.T.SustainFor))
}

func span(snaps []telemetry.Snapshot) time.Duration {
	if len(snaps) < 2 {
		return 0
	}
	return snaps[len(snaps)-1].Timestamp.Sub(snaps[0].Timestamp)
}

// recent returns the tail of the window covering the last d.
//
// Rules evaluate over a tail rather than over everything retained. The store
// keeps minutes of history so that trends are visible, but a condition that
// started thirty seconds ago must still be able to satisfy a thirty-second
// sustain requirement — requiring it to hold across the entire retained history
// would mean a rule could not fire until the problem had been happening for the
// full retention period.
// The window includes the last sample taken *before* the cutoff, not only
// those at or after it. Without that sample the window is almost always
// fractionally shorter than d — the oldest included sample sits just inside the
// boundary — so a "has this held for d?" check comparing the window's span
// against d would be false for every window that did not land exactly on a
// scrape. Bracketing the interval makes the span genuinely cover d whenever the
// history does.
func (e *Eval) recent(d time.Duration) []telemetry.Snapshot {
	if len(e.Window) == 0 || d <= 0 {
		return e.Window
	}
	cutoff := e.Window[len(e.Window)-1].Timestamp.Add(-d)
	start := sort.Search(len(e.Window), func(i int) bool {
		return !e.Window[i].Timestamp.Before(cutoff)
	})
	if start > 0 {
		start--
	}
	return e.Window[start:]
}

// Sustained reports whether pred holds across every snapshot in the last
// T.SustainFor and that span is actually covered by the data.
//
// Requiring the full span means a rule stays quiet for the first sustain window
// after startup rather than firing on a single sample. That is the intended
// behaviour: a diagnosis drawn from one scrape is a guess, and queues spike for
// one scheduler step routinely.
func (e *Eval) Sustained(pred func(telemetry.Snapshot) bool) bool {
	if e.T.SustainFor <= 0 {
		return len(e.Window) > 0 && pred(e.Latest)
	}
	recent := e.recent(e.T.SustainFor)
	if span(recent) < e.T.SustainFor {
		return false
	}
	for _, s := range recent {
		if !pred(s) {
			return false
		}
	}
	return true
}

// TrendWindow is how far back Trend looks, as a multiple of the sustain window.
// A trend needs more history than a threshold does to mean anything.
const TrendWindow = 3

// Trend returns the difference between the mean of the last third of the trend
// window and the mean of the first third, for the metric selected by pick. The
// second return is false when there is too little history or the metric is not
// measured throughout.
//
// It is deliberately crude — fitting a regression to noisy scrape data would
// imply more precision than the input supports. All the rules need to know is
// whether a queue is draining or building.
func (e *Eval) Trend(pick func(telemetry.Snapshot) telemetry.Metric) (float64, bool) {
	const minSamples = 6
	window := e.recent(e.T.SustainFor * TrendWindow)
	if len(window) < minSamples {
		return 0, false
	}
	third := len(window) / 3
	early, okEarly := mean(window[:third], pick)
	late, okLate := mean(window[len(window)-third:], pick)
	if !okEarly || !okLate {
		return 0, false
	}
	return late - early, true
}

func mean(snaps []telemetry.Snapshot, pick func(telemetry.Snapshot) telemetry.Metric) (float64, bool) {
	var sum float64
	var n int
	for _, s := range snaps {
		if m := pick(s); m.OK {
			sum += m.Value
			n++
		}
	}
	if n != len(snaps) || n == 0 {
		// Requiring every sample to be measured avoids comparing a mean over
		// three points with a mean over ten.
		return 0, false
	}
	return sum / float64(n), true
}

// Engine evaluates a rule set against telemetry.
type Engine struct {
	rules []Rule
	t     Thresholds
	now   func() time.Time
}

// NewEngine returns an Engine using the default rule set.
func NewEngine(t Thresholds) *Engine {
	return &Engine{rules: DefaultRules(), t: t, now: time.Now}
}

// Rules returns the active rule set, sorted by code, for the /api/v1/rules
// endpoint and the documentation generator.
func (e *Engine) Rules() []Rule {
	out := append([]Rule(nil), e.rules...)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Thresholds returns the configured thresholds.
func (e *Engine) Thresholds() Thresholds { return e.t }

// WindowFunc supplies the retained history for a workload key. The store
// provides it; tests can supply a stub.
type WindowFunc func(key string) []telemetry.Snapshot

// Analyze evaluates every rule against every workload.
//
// The result is deterministic: rules run in code order over workloads in key
// order, so identical telemetry produces an identical response, and a
// recommendation's ID does not change between two calls while the condition
// holds.
func (e *Engine) Analyze(latest []telemetry.Snapshot, window WindowFunc) []telemetry.Recommendation {
	now := e.now()
	out := make([]telemetry.Recommendation, 0, len(latest))

	byKey := append([]telemetry.Snapshot(nil), latest...)
	sort.Slice(byKey, func(i, j int) bool { return byKey[i].Key() < byKey[j].Key() })

	for _, snap := range byKey {
		var hist []telemetry.Snapshot
		if window != nil {
			hist = window(snap.Key())
		}
		if len(hist) == 0 {
			hist = []telemetry.Snapshot{snap}
		}
		out = append(out, e.analyzeWorkload(snap, hist, now)...)
	}
	return out
}

func (e *Engine) analyzeWorkload(snap telemetry.Snapshot, hist []telemetry.Snapshot, now time.Time) []telemetry.Recommendation {
	ev := &Eval{Latest: snap, Window: hist, T: e.t, Now: now}

	fired := make(map[Code]telemetry.Recommendation)
	superseded := make(map[Code]bool)

	for _, rule := range e.Rules() {
		if !rule.appliesTo(snap.Runtime) {
			continue
		}
		f := rule.Eval(ev)
		if f == nil {
			continue
		}
		severity := rule.Severity
		if f.Severity != "" {
			severity = f.Severity
		}
		rec := telemetry.Recommendation{
			ID:              recommendationID(rule.Code, snap),
			Code:            string(rule.Code),
			Severity:        severity,
			Namespace:       snap.Namespace,
			WorkloadName:    snap.WorkloadName,
			Runtime:         snap.Runtime,
			Title:           rule.Title,
			Explanation:     f.Explanation,
			SuggestedAction: f.Action,
			Evidence:        f.Evidence,
			ObservedAt:      snap.Timestamp,
			WindowSeconds:   int(f.Window.Seconds()),
		}
		fired[rule.Code] = rec
		for _, code := range rule.Supersedes {
			superseded[code] = true
		}
	}

	out := make([]telemetry.Recommendation, 0, len(fired))
	for code, rec := range fired {
		if superseded[code] {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity.Rank() != out[j].Severity.Rank() {
			return out[i].Severity.Rank() < out[j].Severity.Rank()
		}
		return out[i].Code < out[j].Code
	})
	return out
}

func (r Rule) appliesTo(rt telemetry.Runtime) bool {
	if len(r.Runtimes) == 0 {
		return true
	}
	for _, allowed := range r.Runtimes {
		if allowed == rt {
			return true
		}
	}
	return false
}

// recommendationID is derived from the rule and the workload, never from
// evaluation order. Clients deduplicate on it, so it has to survive a restart
// and a change in the number of workloads being watched.
func recommendationID(code Code, snap telemetry.Snapshot) string {
	return fmt.Sprintf("%s:%s", code, snap.Key())
}

// evidence is a small constructor that keeps rule bodies readable.
func evidence(metric, source string, observed, threshold float64, comparison, unit string) telemetry.Evidence {
	return telemetry.Evidence{
		Metric:     metric,
		Source:     source,
		Observed:   observed,
		Threshold:  threshold,
		Comparison: comparison,
		Unit:       unit,
	}
}

// observation builds an Evidence entry that carries a supporting measurement with
// no threshold attached — the numbers a reader needs to judge the finding.
func observation(metric, source string, observed float64, unit string) telemetry.Evidence {
	return telemetry.Evidence{Metric: metric, Source: source, Observed: observed, Unit: unit}
}
