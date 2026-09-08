package triton

import (
	"math"
	"testing"

	"github.com/p95labs/ifa/internal/runtime"
	"github.com/p95labs/ifa/internal/telemetry"
)

// defaultPayload is what Triton exposes without --metrics-config
// summary_latencies=true: counters, gauges, and no percentiles.
const defaultPayload = `# HELP nv_inference_request_success Number of successful inference requests
# TYPE nv_inference_request_success counter
nv_inference_request_success{model="resnet50",version="1"} 10240
nv_inference_request_success{model="bert",version="1"} 512
# HELP nv_inference_request_failure Number of failed inference requests
# TYPE nv_inference_request_failure counter
nv_inference_request_failure{model="resnet50",version="1",reason="BACKEND"} 12
nv_inference_request_failure{model="resnet50",version="1",reason="REJECTED"} 6
# HELP nv_inference_pending_request_count Requests awaiting execution
# TYPE nv_inference_pending_request_count gauge
nv_inference_pending_request_count{model="resnet50",version="1"} 17
nv_inference_pending_request_count{model="bert",version="1"} 0
# HELP nv_inference_request_duration_us Cumulative request duration
# TYPE nv_inference_request_duration_us counter
nv_inference_request_duration_us{model="resnet50",version="1"} 88000000
# HELP nv_inference_queue_duration_us Cumulative queue duration
# TYPE nv_inference_queue_duration_us counter
nv_inference_queue_duration_us{model="resnet50",version="1"} 4000000
# HELP nv_gpu_utilization GPU utilization rate [0.0 - 1.0)
# TYPE nv_gpu_utilization gauge
nv_gpu_utilization{gpu_uuid="GPU-aaa"} 0.91
nv_gpu_utilization{gpu_uuid="GPU-bbb"} 0.12
# HELP nv_gpu_memory_used_bytes GPU used memory
# TYPE nv_gpu_memory_used_bytes gauge
nv_gpu_memory_used_bytes{gpu_uuid="GPU-aaa"} 34359738368
# HELP nv_gpu_memory_total_bytes GPU total memory
# TYPE nv_gpu_memory_total_bytes gauge
nv_gpu_memory_total_bytes{gpu_uuid="GPU-aaa"} 42949672960
`

// summaryPayload adds the optional latency summaries.
const summaryPayload = defaultPayload + `# TYPE nv_inference_request_summary_us summary
nv_inference_request_summary_us{model="resnet50",version="1",quantile="0.5"} 5200
nv_inference_request_summary_us{model="resnet50",version="1",quantile="0.95"} 41000
nv_inference_request_summary_us{model="resnet50",version="1",quantile="0.99"} 96000
nv_inference_request_summary_us_count{model="resnet50",version="1"} 10240
nv_inference_request_summary_us_sum{model="resnet50",version="1"} 88000000
# TYPE nv_inference_queue_summary_us summary
nv_inference_queue_summary_us{model="resnet50",version="1",quantile="0.95"} 12000
`

func parse(t *testing.T, payload, model string) runtime.Reading {
	t.Helper()
	r, err := New().Parse(payload, model)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

// nv_gpu_utilization is a rate in [0,1]. Treating it as a percentage turns a
// fully loaded GPU into "0.91% utilised" and makes the idle-GPU rule fire
// permanently against every Triton deployment.
func TestGPUUtilisationIsConvertedFromRateToPercent(t *testing.T) {
	r := parse(t, defaultPayload, "resnet50")

	if !r.Snapshot.GPUUtilizationPct.OK {
		t.Fatal("GPU utilisation not measured")
	}
	if got := r.Snapshot.GPUUtilizationPct.Value; math.Abs(got-91) > 1e-9 {
		t.Errorf("GPU utilisation = %v, want 91 (0.91 rate → percent, max across devices)", got)
	}
}

func TestGPUMemoryPercentage(t *testing.T) {
	r := parse(t, defaultPayload, "resnet50")
	if !r.Snapshot.GPUMemoryUsedPct.OK {
		t.Fatal("GPU memory not measured")
	}
	if got := r.Snapshot.GPUMemoryUsedPct.Value; math.Abs(got-80) > 1e-9 {
		t.Errorf("GPU memory = %v, want 80", got)
	}
}

// Without summary latencies enabled, Triton exposes only cumulative duration
// counters. Their ratio is a mean, and a mean is not a p95: publishing one as
// a percentile would make every latency threshold meaningless.
func TestNoFakePercentilesWithoutSummaryMetrics(t *testing.T) {
	r := parse(t, defaultPayload, "resnet50")

	for name, m := range map[string]telemetry.Metric{
		"p50":       r.Snapshot.P50LatencyMs,
		"p95":       r.Snapshot.P95LatencyMs,
		"p99":       r.Snapshot.P99LatencyMs,
		"queue p95": r.Snapshot.QueueTimeP95Ms,
	} {
		if m.OK {
			t.Errorf("%s reported as %v without summary latencies enabled", name, m.Value)
		}
	}
}

func TestSummaryLatenciesAreReadWhenEnabled(t *testing.T) {
	r := parse(t, summaryPayload, "resnet50")

	tests := []struct {
		name string
		got  telemetry.Metric
		want float64
	}{
		{"p50", r.Snapshot.P50LatencyMs, 5.2},
		{"p95", r.Snapshot.P95LatencyMs, 41},
		{"p99", r.Snapshot.P99LatencyMs, 96},
		{"queue p95", r.Snapshot.QueueTimeP95Ms, 12},
	}
	for _, tc := range tests {
		if !tc.got.OK {
			t.Errorf("%s not measured", tc.name)
			continue
		}
		if math.Abs(tc.got.Value-tc.want) > 1e-9 {
			t.Errorf("%s = %v ms, want %v (microseconds converted)", tc.name, tc.got.Value, tc.want)
		}
	}
}

func TestPerModelSeparation(t *testing.T) {
	resnet := parse(t, defaultPayload, "resnet50")
	bert := parse(t, defaultPayload, "bert")

	if resnet.Snapshot.RequestsWaiting.Value != 17 {
		t.Errorf("resnet50 pending = %v, want 17", resnet.Snapshot.RequestsWaiting)
	}
	if bert.Snapshot.RequestsWaiting.Value != 0 {
		t.Errorf("bert pending = %v, want 0", bert.Snapshot.RequestsWaiting)
	}
	if resnet.Counters[runtime.CounterRequestsFinished] != 10240 {
		t.Errorf("resnet50 finished = %v", resnet.Counters[runtime.CounterRequestsFinished])
	}
	if bert.Counters[runtime.CounterRequestsFinished] != 512 {
		t.Errorf("bert finished = %v", bert.Counters[runtime.CounterRequestsFinished])
	}
}

// Triton breaks failures down by reason; the error-rate rule needs the total.
func TestFailureReasonsAreSummed(t *testing.T) {
	r := parse(t, defaultPayload, "resnet50")
	if got := r.Counters[runtime.CounterRequestsFailed]; got != 18 {
		t.Errorf("failures = %v, want 18 (BACKEND + REJECTED)", got)
	}
}

func TestMissingMetricsAreReportedAndOptionalOnesAreNot(t *testing.T) {
	r := parse(t, "", "resnet50")
	missing := map[string]bool{}
	for _, m := range r.Missing {
		missing[m] = true
	}
	for _, required := range []string{MetricRequestSuccess, MetricPendingCount, MetricGPUUtilization} {
		if !missing[required] {
			t.Errorf("%s should have been reported missing", required)
		}
	}
	for _, optional := range []string{MetricRequestSummaryUs, MetricQueueSummaryUs} {
		if missing[optional] {
			t.Errorf("%s is optional and should not be reported missing", optional)
		}
	}

	full := parse(t, summaryPayload, "resnet50")
	if len(full.Missing) != 0 {
		t.Errorf("complete payload reported missing metrics: %v", full.Missing)
	}
}

// A configured model name that matches nothing is the most common reason a
// Triton target silently reports nothing, so `ifa check` lists what is there.
func TestModelsListsWhatTheTargetServes(t *testing.T) {
	models, err := Models(defaultPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "bert" || models[1] != "resnet50" {
		t.Errorf("models = %v, want [bert resnet50]", models)
	}
}

func TestMalformedPayloadIsSurvivable(t *testing.T) {
	r := parse(t, "not exposition\nnv_gpu_utilization{gpu_uuid=\"a\"} 0.5\n", "")
	if !r.Snapshot.GPUUtilizationPct.OK {
		t.Error("a valid line was lost because of an invalid one")
	}
	if r.UnparseableLines == 0 {
		t.Error("the malformed line was not counted")
	}
}
