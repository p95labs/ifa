// Package triton maps NVIDIA Triton Inference Server's Prometheus exposition
// onto the shared telemetry model.
//
// Two details of Triton's output are easy to get wrong and produce plausible
// but false readings:
//
//  1. nv_gpu_utilization is a rate in [0.0, 1.0], not a percentage. Treating it
//     as a percentage turns a fully loaded GPU into "0.9% utilised" and makes
//     the idle-GPU rule fire permanently.
//  2. The default latency metrics are cumulative microsecond counters, not
//     percentiles. Dividing them gives a mean, and a mean is not a p95. This
//     adapter only reports percentiles when Triton is started with
//     --metrics-config summary_latencies=true, and otherwise leaves them
//     unmeasured and says so in the missing-metric report.
//
// Status: implemented against Triton's documented metric surface and validated
// against a live Triton 25.12 CPU server (Python backend, no GPU). See
// docs/RUNTIMES.md for full validation scope and limitations.
package triton

import (
	"fmt"
	"sort"

	"github.com/p95labs/ifa/internal/promtext"
	"github.com/p95labs/ifa/internal/runtime"
	"github.com/p95labs/ifa/internal/telemetry"
)

// Triton metric names.
// Reference: https://github.com/triton-inference-server/server/blob/main/docs/user_guide/metrics.md
const (
	MetricRequestSuccess = "nv_inference_request_success"
	MetricRequestFailure = "nv_inference_request_failure"
	MetricPendingCount   = "nv_inference_pending_request_count"

	// Cumulative microsecond counters, always present.
	MetricRequestDurationUs = "nv_inference_request_duration_us"
	MetricQueueDurationUs   = "nv_inference_queue_duration_us"

	// Summary families, present only with --metrics-config summary_latencies=true.
	MetricRequestSummaryUs = "nv_inference_request_summary_us"
	MetricQueueSummaryUs   = "nv_inference_queue_summary_us"

	// GPU metrics, per device. nv_gpu_utilization is a fraction in [0, 1].
	MetricGPUUtilization       = "nv_gpu_utilization"
	MetricGPUMemoryUsed        = "nv_gpu_memory_used_bytes"
	MetricGPUMemoryTotal       = "nv_gpu_memory_total_bytes"
	LabelModel                 = "model"
	LabelGPUUUID               = "gpu_uuid"
	summaryQuantileP50         = 0.5
	summaryQuantileP95         = 0.95
	summaryQuantileP99         = 0.99
	microsecondsPerMillisecond = 1000.0
)

// Adapter implements runtime.Adapter for Triton.
type Adapter struct{}

// New returns a Triton adapter.
func New() Adapter { return Adapter{} }

// Runtime identifies this adapter.
func (Adapter) Runtime() telemetry.Runtime { return telemetry.RuntimeTriton }

// ExpectedMetrics lists what the adapter reads.
func (Adapter) ExpectedMetrics() []string {
	return []string{
		MetricRequestSuccess,
		MetricRequestFailure,
		MetricPendingCount,
		MetricRequestDurationUs,
		MetricQueueDurationUs,
		MetricGPUUtilization,
		MetricGPUMemoryUsed,
		MetricGPUMemoryTotal,
		MetricRequestSummaryUs,
		MetricQueueSummaryUs,
	}
}

// optionalMetrics are absent in a correctly configured default deployment.
var optionalMetrics = map[string]bool{
	MetricRequestSummaryUs: true,
	MetricQueueSummaryUs:   true,
}

// Parse extracts a Reading from a Triton /metrics payload.
//
// modelName selects one model. Triton serves many models from one process and
// reports per-model series, so an empty modelName aggregates counters across
// every loaded model — correct only when the target really does host one.
func (a Adapter) Parse(payload string, modelName string) (runtime.Reading, error) {
	mf, err := promtext.ParseString(payload)
	if err != nil {
		return runtime.Reading{}, fmt.Errorf("triton: %w", err)
	}

	modelSel := promtext.Labels{}
	if modelName != "" {
		modelSel[LabelModel] = modelName
	}

	r := runtime.Reading{
		Counters:         make(map[runtime.Counter]float64, 4),
		UnparseableLines: mf.LinesSkipped,
	}
	snap := &r.Snapshot
	snap.Runtime = telemetry.RuntimeTriton
	snap.ModelName = modelName

	seen := map[string]bool{}
	sum := func(name string, sel promtext.Labels) (float64, bool) {
		v, ok := mf.Sum(name, sel)
		seen[name] = seen[name] || ok
		return v, ok
	}

	if v, ok := sum(MetricPendingCount, modelSel); ok {
		snap.RequestsWaiting = telemetry.Observed(v)
	}
	if v, ok := sum(MetricRequestSuccess, modelSel); ok {
		r.Counters[runtime.CounterRequestsFinished] = v
	}
	// Triton breaks failures down by reason (REJECTED, CANCELED, BACKEND,
	// OTHER). Summing them is the total the error-rate rule needs; the
	// breakdown is left for the operator reading the runtime's own metrics.
	if v, ok := sum(MetricRequestFailure, modelSel); ok {
		r.Counters[runtime.CounterRequestsFailed] = v
	}

	// Percentiles, when the operator enabled summary latencies.
	if v, ok := mf.SummaryQuantile(MetricRequestSummaryUs, summaryQuantileP50, modelSel); ok {
		snap.P50LatencyMs = telemetry.Observed(v / microsecondsPerMillisecond)
		seen[MetricRequestSummaryUs] = true
	}
	if v, ok := mf.SummaryQuantile(MetricRequestSummaryUs, summaryQuantileP95, modelSel); ok {
		snap.P95LatencyMs = telemetry.Observed(v / microsecondsPerMillisecond)
		seen[MetricRequestSummaryUs] = true
	}
	if v, ok := mf.SummaryQuantile(MetricRequestSummaryUs, summaryQuantileP99, modelSel); ok {
		snap.P99LatencyMs = telemetry.Observed(v / microsecondsPerMillisecond)
		seen[MetricRequestSummaryUs] = true
	}
	if v, ok := mf.SummaryQuantile(MetricQueueSummaryUs, summaryQuantileP95, modelSel); ok {
		snap.QueueTimeP95Ms = telemetry.Observed(v / microsecondsPerMillisecond)
		seen[MetricQueueSummaryUs] = true
	}

	// The cumulative duration counters are read so that their absence is
	// reported, but they are deliberately not turned into a fake percentile.
	_, _ = sum(MetricRequestDurationUs, modelSel)
	_, _ = sum(MetricQueueDurationUs, modelSel)

	// GPU metrics are per device and carry no model label, so aggregating by
	// max across devices is the only sensible reading: one saturated GPU is
	// what an operator needs to see.
	if util, ok := gpuMax(mf, MetricGPUUtilization); ok {
		seen[MetricGPUUtilization] = true
		// [0,1] rate to percent.
		snap.GPUUtilizationPct = telemetry.Observed(util * 100)
	}
	used, usedOK := gpuMax(mf, MetricGPUMemoryUsed)
	total, totalOK := gpuMax(mf, MetricGPUMemoryTotal)
	seen[MetricGPUMemoryUsed] = usedOK
	seen[MetricGPUMemoryTotal] = totalOK
	if usedOK && totalOK && total > 0 {
		snap.GPUMemoryUsedPct = telemetry.Observed(used / total * 100)
	}

	r.Missing = missingFrom(a.ExpectedMetrics(), seen)
	return r, nil
}

// gpuMax returns the largest value of a per-device GPU metric.
func gpuMax(mf *promtext.MetricFamilies, name string) (float64, bool) {
	return mf.Max(name, nil)
}

// Models lists the model names present in a payload. `ifa check` uses it to
// tell an operator which models a Triton target is serving when their
// configured model_name matches none of them — the most common reason a Triton
// target reports nothing.
func Models(payload string) ([]string, error) {
	mf, err := promtext.ParseString(payload)
	if err != nil {
		return nil, fmt.Errorf("triton: %w", err)
	}
	set := map[string]bool{}
	for _, name := range []string{MetricRequestSuccess, MetricPendingCount, MetricRequestDurationUs} {
		for _, s := range mf.Select(name, nil) {
			if m := s.Labels[LabelModel]; m != "" {
				set[m] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

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
