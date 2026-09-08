// Package vllm maps vLLM's Prometheus exposition onto the shared telemetry
// model.
//
// Three properties of vLLM's output shape this adapter, and getting any of them
// wrong produces an integration that returns zeros against a live server while
// passing its own tests:
//
//  1. Every series is labelled. vLLM tags each metric with model_name and, in
//     the V1 engine, engine (the engine-core index). A lookup by bare metric
//     name matches nothing.
//  2. Latencies are histograms, not summaries. There is no quantile label to
//     read; percentiles have to be interpolated from cumulative buckets, and
//     their precision is bounded by vLLM's bucket boundaries.
//  3. There is no failure counter. vLLM exposes vllm:request_success_total
//     broken down by finished_reason and nothing that counts failures, so an
//     error rate cannot be derived from vLLM alone. This adapter leaves
//     ErrorRatePct unmeasured rather than inventing one from the success
//     counter, and reports the abort share instead.
//
// Metric names were taken from vllm/v1/metrics/loggers.py. V0-era names are
// accepted as fallbacks where they differ (see kvCacheUsage below), because
// clusters run older vLLM for a long time.
//
// Validation status: the parsing is exercised against fixtures built from
// vLLM's own metric definitions and bucket boundaries. It has not been run
// against a live vLLM server in this repository's CI — see docs/VLLM.md.
package vllm

import (
	"fmt"
	"sort"

	"github.com/p95labs/ifa/internal/promtext"
	"github.com/p95labs/ifa/internal/runtime"
	"github.com/p95labs/ifa/internal/telemetry"
)

// Metric names emitted by vLLM. Counter families carry the _total suffix that
// the Prometheus client library appends.
const (
	MetricNumRequestsRunning = "vllm:num_requests_running"
	MetricNumRequestsWaiting = "vllm:num_requests_waiting"
	// MetricNumRequestsWaitingByReason splits the waiting queue with a
	// reason label of "capacity" or "deferred". Present from the V1 engine
	// onwards; absent on older builds.
	MetricNumRequestsWaitingByReason = "vllm:num_requests_waiting_by_reason"

	// MetricKVCacheUsage is the V1 name. MetricGPUCacheUsageLegacy is the V0
	// name for the same quantity, still emitted by older releases. Both report
	// a fraction in [0,1].
	MetricKVCacheUsage        = "vllm:kv_cache_usage_perc"
	MetricGPUCacheUsageLegacy = "vllm:gpu_cache_usage_perc"

	MetricTimeToFirstToken = "vllm:time_to_first_token_seconds"
	MetricE2ELatency       = "vllm:e2e_request_latency_seconds"
	MetricQueueTime        = "vllm:request_queue_time_seconds"

	MetricPromptTokensTotal     = "vllm:prompt_tokens_total"
	MetricGenerationTokensTotal = "vllm:generation_tokens_total"
	MetricRequestSuccessTotal   = "vllm:request_success_total"
	MetricPreemptionsTotal      = "vllm:num_preemptions_total"

	MetricPrefixCacheQueriesTotal = "vllm:prefix_cache_queries_total"
	MetricPrefixCacheHitsTotal    = "vllm:prefix_cache_hits_total"
)

// Label names on vLLM series.
const (
	LabelModelName      = "model_name"
	LabelFinishedReason = "finished_reason"
	LabelWaitingReason  = "reason"
)

// Waiting-queue reasons reported by vllm:num_requests_waiting_by_reason.
const (
	WaitingReasonCapacity = "capacity"
	WaitingReasonDeferred = "deferred"
)

// finishedReasonAbort is the finished_reason value vLLM records for requests
// that ended before producing a complete response.
const finishedReasonAbort = "abort"

// Adapter implements runtime.Adapter for vLLM.
type Adapter struct{}

// New returns a vLLM adapter. It holds no state.
func New() Adapter { return Adapter{} }

// Runtime identifies this adapter.
func (Adapter) Runtime() telemetry.Runtime { return telemetry.RuntimeVLLM }

// ExpectedMetrics lists what the adapter reads, in the order it is worth
// checking them: scheduler state first, then cache, then latency, then
// throughput counters.
func (Adapter) ExpectedMetrics() []string {
	return []string{
		MetricNumRequestsRunning,
		MetricNumRequestsWaiting,
		MetricKVCacheUsage,
		MetricTimeToFirstToken,
		MetricE2ELatency,
		MetricGenerationTokensTotal,
		MetricPromptTokensTotal,
		MetricRequestSuccessTotal,
		MetricPreemptionsTotal,
		MetricQueueTime,
		MetricPrefixCacheQueriesTotal,
		MetricPrefixCacheHitsTotal,
		MetricNumRequestsWaitingByReason,
	}
}

// optionalMetrics are not reported as missing: they exist only on some vLLM
// versions or only when a feature is enabled, so their absence is normal and
// listing them would train operators to ignore the missing-metric report.
var optionalMetrics = map[string]bool{
	MetricNumRequestsWaitingByReason: true,
	MetricQueueTime:                  true,
	MetricPrefixCacheQueriesTotal:    true,
	MetricPrefixCacheHitsTotal:       true,
	MetricPreemptionsTotal:           true,
}

// Parse extracts a Reading from a vLLM /metrics payload.
//
// When modelName is non-empty, only series carrying that model_name label are
// considered. This matters on servers running more than one model: aggregating
// across them would produce a queue depth and a cache utilisation that belong
// to no actual workload.
func (a Adapter) Parse(payload string, modelName string) (runtime.Reading, error) {
	mf, err := promtext.ParseString(payload)
	if err != nil {
		return runtime.Reading{}, fmt.Errorf("vllm: %w", err)
	}

	sel := promtext.Labels{}
	if modelName != "" {
		sel[LabelModelName] = modelName
	}

	r := runtime.Reading{
		Counters:         make(map[runtime.Counter]float64, 8),
		UnparseableLines: mf.LinesSkipped,
	}
	snap := &r.Snapshot
	snap.Runtime = telemetry.RuntimeVLLM
	snap.ModelName = modelName

	seen := map[string]bool{}
	// sum reads a counter-like or additive gauge across every matching series.
	// Requests and tokens add up across engine-core replicas; utilisation does
	// not, which is why the two helpers are separate.
	sum := func(name string) telemetry.Metric {
		v, ok := mf.Sum(name, sel)
		seen[name] = seen[name] || ok
		return telemetry.ObservedIf(v, ok)
	}
	max := func(name string) telemetry.Metric {
		v, ok := mf.Max(name, sel)
		seen[name] = seen[name] || ok
		return telemetry.ObservedIf(v, ok)
	}
	// quantileMs converts a histogram quantile from seconds to milliseconds.
	quantileMs := func(family string, q float64) telemetry.Metric {
		v, ok := mf.Quantile(family, q, sel)
		seen[family] = seen[family] || ok
		return telemetry.ObservedIf(v*1000, ok)
	}

	// Scheduler state.
	snap.RequestsRunning = sum(MetricNumRequestsRunning)
	snap.RequestsWaiting = sum(MetricNumRequestsWaiting)

	capacitySel := cloneWith(sel, LabelWaitingReason, WaitingReasonCapacity)
	deferredSel := cloneWith(sel, LabelWaitingReason, WaitingReasonDeferred)
	if v, ok := mf.Sum(MetricNumRequestsWaitingByReason, capacitySel); ok {
		snap.WaitingForCapacity = telemetry.Observed(v)
		seen[MetricNumRequestsWaitingByReason] = true
	}
	if v, ok := mf.Sum(MetricNumRequestsWaitingByReason, deferredSel); ok {
		snap.WaitingDeferred = telemetry.Observed(v)
		seen[MetricNumRequestsWaitingByReason] = true
	}

	// KV cache. vLLM reports a fraction; the shared model uses percent.
	kv := max(MetricKVCacheUsage)
	if !kv.OK {
		// Fall back to the V0 name. Record it under the V1 key so the
		// missing-metric report does not accuse a working older server.
		if v, ok := mf.Max(MetricGPUCacheUsageLegacy, sel); ok {
			kv = telemetry.Observed(v)
			seen[MetricKVCacheUsage] = true
		}
	}
	if kv.OK {
		snap.KVCacheUsagePct = telemetry.Observed(kv.Value * 100)
	}

	// Latency percentiles. p50/p95/p99 come from the same bucket set, so an
	// absent histogram leaves all three unmeasured together.
	snap.TTFTP50Ms = quantileMs(MetricTimeToFirstToken, 0.50)
	snap.TTFTP95Ms = quantileMs(MetricTimeToFirstToken, 0.95)
	snap.TTFTP99Ms = quantileMs(MetricTimeToFirstToken, 0.99)

	snap.P50LatencyMs = quantileMs(MetricE2ELatency, 0.50)
	snap.P95LatencyMs = quantileMs(MetricE2ELatency, 0.95)
	snap.P99LatencyMs = quantileMs(MetricE2ELatency, 0.99)

	snap.QueueTimeP95Ms = quantileMs(MetricQueueTime, 0.95)

	// Counters. Raw values only: the collector owns rate conversion.
	if v := sum(MetricGenerationTokensTotal); v.OK {
		r.Counters[runtime.CounterGenerationTokens] = v.Value
	}
	if v := sum(MetricPromptTokensTotal); v.OK {
		r.Counters[runtime.CounterPromptTokens] = v.Value
	}
	if v := sum(MetricPreemptionsTotal); v.OK {
		r.Counters[runtime.CounterPreemptions] = v.Value
	}
	if v := sum(MetricPrefixCacheQueriesTotal); v.OK {
		r.Counters[runtime.CounterPrefixCacheQueries] = v.Value
	}
	if v := sum(MetricPrefixCacheHitsTotal); v.OK {
		r.Counters[runtime.CounterPrefixCacheHits] = v.Value
	}

	// request_success_total is broken down by finished_reason. Summing every
	// reason gives the finished-request count; the abort slice is the closest
	// thing vLLM offers to a client-visible failure signal.
	//
	// Note what is deliberately absent: CounterRequestsFailed. vLLM has no
	// failure counter, so the error-rate rules stay dormant for vLLM targets
	// instead of firing on a fabricated number.
	if v := sum(MetricRequestSuccessTotal); v.OK {
		r.Counters[runtime.CounterRequestsFinished] = v.Value
	}
	if v, ok := mf.Sum(MetricRequestSuccessTotal, cloneWith(sel, LabelFinishedReason, finishedReasonAbort)); ok {
		r.Counters[runtime.CounterRequestsAborted] = v
	}

	r.Missing = missingFrom(a.ExpectedMetrics(), seen)
	return r, nil
}

// cloneWith returns a copy of base with one extra label constraint.
func cloneWith(base promtext.Labels, key, value string) promtext.Labels {
	out := make(promtext.Labels, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[key] = value
	return out
}

// missingFrom lists expected, non-optional metrics that were not observed.
func missingFrom(expected []string, seen map[string]bool) []string {
	var missing []string
	for _, name := range expected {
		if optionalMetrics[name] || seen[name] {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}
