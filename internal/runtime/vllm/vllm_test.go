package vllm

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p95labs/ifa/internal/runtime"
	"github.com/p95labs/ifa/internal/telemetry"
)

const model = "meta-llama/Llama-3.1-8B-Instruct"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(b)
}

func parse(t *testing.T, name, modelName string) runtime.Reading {
	t.Helper()
	r, err := New().Parse(fixture(t, name), modelName)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

func closeTo(t *testing.T, label string, got telemetry.Metric, want float64) {
	t.Helper()
	if !got.OK {
		t.Errorf("%s: unmeasured, want %v", label, want)
		return
	}
	if math.Abs(got.Value-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", label, got.Value, want)
	}
}

func TestParseV1Payload(t *testing.T) {
	r := parse(t, "vllm_v1_healthy.txt", model)
	s := r.Snapshot

	if s.Runtime != telemetry.RuntimeVLLM {
		t.Errorf("runtime = %q", s.Runtime)
	}

	closeTo(t, "requests_running", s.RequestsRunning, 11)
	closeTo(t, "requests_waiting", s.RequestsWaiting, 3)
	closeTo(t, "waiting_for_capacity", s.WaitingForCapacity, 3)
	closeTo(t, "waiting_deferred", s.WaitingDeferred, 0)
	closeTo(t, "kv_cache_usage_percent", s.KVCacheUsagePct, 64.12)

	// Expected values are the linear interpolation Prometheus itself performs
	// over vLLM's own bucket boundaries.
	closeTo(t, "ttft_p50_ms", s.TTFTP50Ms, 157.69230769230768)
	closeTo(t, "ttft_p95_ms", s.TTFTP95Ms, 482.14285714285717)
	closeTo(t, "p95_latency_ms", s.P95LatencyMs, 1857.142857142857)
	closeTo(t, "queue_time_p95_ms", s.QueueTimeP95Ms, 344.44444444444446)

	wantCounters := map[runtime.Counter]float64{
		runtime.CounterGenerationTokens:   1930447,
		runtime.CounterPromptTokens:       4821904,
		runtime.CounterPreemptions:        0,
		runtime.CounterPrefixCacheQueries: 4821904,
		runtime.CounterPrefixCacheHits:    3182456,
		// stop + length + abort
		runtime.CounterRequestsFinished: 9312 + 651 + 37,
		runtime.CounterRequestsAborted:  37,
	}
	for k, want := range wantCounters {
		got, ok := r.Counters[k]
		if !ok {
			t.Errorf("counter %s missing", k)
			continue
		}
		if got != want {
			t.Errorf("counter %s = %v, want %v", k, got, want)
		}
	}

	// vLLM has no failure counter. Reporting one would mean inventing it.
	if _, ok := r.Counters[runtime.CounterRequestsFailed]; ok {
		t.Error("a failure counter was reported; vLLM does not expose one")
	}
	if s.ErrorRatePct.OK {
		t.Error("error rate was measured; vLLM cannot support one")
	}

	if len(r.Missing) != 0 {
		t.Errorf("missing metrics on a complete payload: %v", r.Missing)
	}
	if r.UnparseableLines != 0 {
		t.Errorf("unparseable lines in a valid payload: %d", r.UnparseableLines)
	}
}

func TestUnrelatedExportersAreIgnored(t *testing.T) {
	// The fixture includes python_gc_* and process_* series, which every vLLM
	// server emits alongside its own metrics.
	r := parse(t, "vllm_v1_healthy.txt", model)
	if r.UnparseableLines != 0 {
		t.Errorf("unrelated exporter lines were counted as unparseable: %d", r.UnparseableLines)
	}
	if !r.Snapshot.RequestsRunning.OK {
		t.Error("unrelated series interfered with extraction")
	}
}

func TestLegacyCacheMetricName(t *testing.T) {
	// Pre-V1 builds emit gpu_cache_usage_perc and no engine label.
	r := parse(t, "vllm_v0_legacy.txt", "mistralai/Mistral-7B-Instruct-v0.2")

	closeTo(t, "kv_cache_usage_percent", r.Snapshot.KVCacheUsagePct, 93)
	closeTo(t, "requests_waiting", r.Snapshot.RequestsWaiting, 14)

	for _, m := range r.Missing {
		if m == MetricKVCacheUsage {
			t.Error("KV cache reported missing even though the legacy metric was present")
		}
	}
}

func TestModelFilterSeparatesColocatedModels(t *testing.T) {
	a := parse(t, "vllm_two_models.txt", "model-a")
	b := parse(t, "vllm_two_models.txt", "model-b")

	closeTo(t, "model-a waiting", a.Snapshot.RequestsWaiting, 0)
	closeTo(t, "model-b waiting", b.Snapshot.RequestsWaiting, 41)
	closeTo(t, "model-a kv", a.Snapshot.KVCacheUsagePct, 12)
	closeTo(t, "model-b kv", b.Snapshot.KVCacheUsagePct, 97)

	if a.Counters[runtime.CounterGenerationTokens] != 500 {
		t.Errorf("model-a generation tokens = %v, want 500",
			a.Counters[runtime.CounterGenerationTokens])
	}

	// Without a filter the adapter aggregates, which is only correct when the
	// target genuinely serves one model.
	all := parse(t, "vllm_two_models.txt", "")
	closeTo(t, "aggregate waiting", all.Snapshot.RequestsWaiting, 41)
	closeTo(t, "aggregate kv (max, not sum)", all.Snapshot.KVCacheUsagePct, 97)
}

func TestMissingMetricsAreReported(t *testing.T) {
	// A server exposing only scheduler state: the integration half-works, and
	// saying so is more useful than reporting quiet zeros.
	payload := `vllm:num_requests_running{model_name="m"} 1
vllm:num_requests_waiting{model_name="m"} 0
`
	r, err := New().Parse(payload, "m")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		MetricKVCacheUsage:          true,
		MetricTimeToFirstToken:      true,
		MetricE2ELatency:            true,
		MetricGenerationTokensTotal: true,
		MetricPromptTokensTotal:     true,
		MetricRequestSuccessTotal:   true,
	}
	got := map[string]bool{}
	for _, m := range r.Missing {
		got[m] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s should have been reported missing", name)
		}
	}
	// Optional metrics must not be reported: they are absent on healthy
	// servers too, and noise here trains operators to ignore the field.
	for _, optional := range []string{
		MetricNumRequestsWaitingByReason, MetricQueueTime,
		MetricPrefixCacheHitsTotal, MetricPreemptionsTotal,
	} {
		if got[optional] {
			t.Errorf("optional metric %s was reported as missing", optional)
		}
	}
}

func TestAbsentMetricsStayUnmeasured(t *testing.T) {
	r, err := New().Parse("", "")
	if err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot

	unmeasured := map[string]telemetry.Metric{
		"requests_running":  s.RequestsRunning,
		"requests_waiting":  s.RequestsWaiting,
		"kv_cache":          s.KVCacheUsagePct,
		"ttft_p95":          s.TTFTP95Ms,
		"p95_latency":       s.P95LatencyMs,
		"queue_time_p95":    s.QueueTimeP95Ms,
		"gpu_utilization":   s.GPUUtilizationPct,
		"gpu_memory":        s.GPUMemoryUsedPct,
		"error_rate":        s.ErrorRatePct,
		"tokens_per_second": s.TokensPerSecond,
	}
	for name, m := range unmeasured {
		if m.OK {
			t.Errorf("%s reported a value (%v) from an empty payload", name, m.Value)
		}
	}
	if len(r.Counters) != 0 {
		t.Errorf("counters from an empty payload: %v", r.Counters)
	}
}

func TestIdleServerHasNoLatencyPercentiles(t *testing.T) {
	// A server that has served nothing exposes histograms with a zero count.
	// Reporting a p95 of 0 would make an idle workload look like the fastest
	// one in the fleet.
	payload := `vllm:num_requests_running{model_name="m"} 0
vllm:time_to_first_token_seconds_bucket{model_name="m",le="0.1"} 0
vllm:time_to_first_token_seconds_bucket{model_name="m",le="+Inf"} 0
vllm:time_to_first_token_seconds_count{model_name="m"} 0
vllm:time_to_first_token_seconds_sum{model_name="m"} 0
`
	r, err := New().Parse(payload, "m")
	if err != nil {
		t.Fatal(err)
	}
	if r.Snapshot.TTFTP95Ms.OK {
		t.Errorf("idle server reported TTFT p95 = %v", r.Snapshot.TTFTP95Ms.Value)
	}
}

func TestMalformedPayloadIsSurvivable(t *testing.T) {
	payload := `garbage line with no value
vllm:num_requests_waiting{model_name="m"} notanumber
vllm:num_requests_running{model_name="m"} 5
{"json": "instead of exposition"}
`
	r, err := New().Parse(payload, "m")
	if err != nil {
		t.Fatalf("Parse should tolerate malformed lines: %v", err)
	}
	closeTo(t, "requests_running", r.Snapshot.RequestsRunning, 5)
	if r.Snapshot.RequestsWaiting.OK {
		t.Error("a non-numeric value was accepted")
	}
	if r.UnparseableLines == 0 {
		t.Error("malformed lines were not counted; a format change would be invisible")
	}
}

// TestCapturedPayload runs the adapter against three verbatim /metrics payloads
// captured from a real vLLM 0.28.0 server (ARM64 CPU backend, facebook/opt-125m).
// The fixtures are intended to be repository testdata; a missing file causes the test to fail.
func TestCapturedPayload(t *testing.T) {
	const capturedModel = "facebook/opt-125m"

	cases := []struct {
		name        string
		fixture     string
		wantRunning float64
		wantWaiting float64
		wantKV      float64
	}{
		{
			name:        "idle",
			fixture:     "vllm_v1_captured.txt",
			wantRunning: 0,
			wantWaiting: 0,
			wantKV:      0,
		},
		{
			name:        "loaded",
			fixture:     "vllm_v1_under_load.txt",
			wantRunning: 10,
			wantWaiting: 0,
			wantKV:      2.857142857142858,
		},
		{
			name:        "capacity-queued",
			fixture:     "vllm_v1_queued.txt",
			wantRunning: 2,
			wantWaiting: 12,
			wantKV:      0.4618937644341847,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fixture(t, tc.fixture)

			// Sanity-check that the fixture contains the expected model label
			// before parsing; a wrong label produces all-unmeasured fields.
			if got := firstModelName(body); got != capturedModel {
				t.Fatalf("fixture model_name = %q, want %q", got, capturedModel)
			}

			r, err := New().Parse(body, capturedModel)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if len(r.Missing) > 0 {
				t.Errorf("required metrics absent from real capture — adapter may need updating: %v", r.Missing)
			}
			if r.UnparseableLines > 0 {
				t.Errorf("%d unparseable line(s); exposition format may have changed", r.UnparseableLines)
			}

			closeTo(t, "requests_running", r.Snapshot.RequestsRunning, tc.wantRunning)
			closeTo(t, "requests_waiting", r.Snapshot.RequestsWaiting, tc.wantWaiting)
			closeTo(t, "kv_cache_usage_percent", r.Snapshot.KVCacheUsagePct, tc.wantKV)
		})
	}
}

// firstModelName returns the first model_name label value found in a vLLM
// Prometheus exposition payload. TestCapturedPayload uses it to verify that
// each captured fixture came from the expected model.
func firstModelName(body string) string {
	const key = `model_name="`
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, key); i >= 0 {
			rest := line[i+len(key):]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				return rest[:j]
			}
		}
	}
	return ""
}

func BenchmarkParseV1(b *testing.B) {
	payload, err := os.ReadFile(filepath.Join("testdata", "vllm_v1_healthy.txt"))
	if err != nil {
		b.Fatal(err)
	}
	a := New()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		if _, err := a.Parse(string(payload), model); err != nil {
			b.Fatal(err)
		}
	}
}
