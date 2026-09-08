package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testOptions() Options {
	return Options{
		Interval:    time.Second,
		Timeout:     500 * time.Millisecond,
		Concurrency: 4,
		ClusterName: "test",
		Logger:      discardLogger(),
	}
}

// vllmPayload renders a minimal but correctly shaped vLLM exposition.
func vllmPayload(waiting, kvFraction, genTokens, promptTokens, finished float64) string {
	return fmt.Sprintf(`vllm:num_requests_running{model_name="m",engine="0"} 4
vllm:num_requests_waiting{model_name="m",engine="0"} %g
vllm:kv_cache_usage_perc{model_name="m",engine="0"} %g
vllm:generation_tokens_total{model_name="m",engine="0"} %g
vllm:prompt_tokens_total{model_name="m",engine="0"} %g
vllm:request_success_total{model_name="m",engine="0",finished_reason="stop"} %g
vllm:time_to_first_token_seconds_bucket{model_name="m",engine="0",le="0.1"} 5
vllm:time_to_first_token_seconds_bucket{model_name="m",engine="0",le="1"} 10
vllm:time_to_first_token_seconds_bucket{model_name="m",engine="0",le="+Inf"} 10
vllm:time_to_first_token_seconds_count{model_name="m",engine="0"} 10
vllm:time_to_first_token_seconds_sum{model_name="m",engine="0"} 3.2
vllm:e2e_request_latency_seconds_bucket{model_name="m",engine="0",le="1"} 5
vllm:e2e_request_latency_seconds_bucket{model_name="m",engine="0",le="+Inf"} 10
vllm:e2e_request_latency_seconds_count{model_name="m",engine="0"} 10
vllm:e2e_request_latency_seconds_sum{model_name="m",engine="0"} 12
`, waiting, kvFraction, genTokens, promptTokens, finished)
}

func newTestCollector(t *testing.T, targets []Target, store *telemetry.Store, opts Options) *Collector {
	t.Helper()
	c, err := New(targets, store, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestScrapeProducesNormalisedSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(12, 0.83, 1000, 5000, 200))
	}))
	defer srv.Close()

	store := telemetry.NewStore()
	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Namespace: "inference",
		Runtime: telemetry.RuntimeVLLM, ModelName: "m",
		MetricsURL: srv.URL,
	}}, store, testOptions())

	snap, err := c.Scrape(context.Background(), c.Targets()[0])
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}

	if snap.WorkloadName != "llm" || snap.Namespace != "inference" {
		t.Errorf("identity not applied: %+v", snap.Key())
	}
	if snap.ClusterName != "test" {
		t.Errorf("cluster name = %q", snap.ClusterName)
	}
	if !snap.RequestsWaiting.OK || snap.RequestsWaiting.Value != 12 {
		t.Errorf("requests_waiting = %v", snap.RequestsWaiting)
	}
	if !snap.KVCacheUsagePct.OK || snap.KVCacheUsagePct.Value != 83 {
		t.Errorf("kv cache = %v, want 83 (fraction converted to percent)", snap.KVCacheUsagePct)
	}
	// GPU metrics need DCGM. Without it they must be unmeasured rather than
	// substituted from something that is merely also a percentage.
	if snap.GPUUtilizationPct.OK {
		t.Errorf("GPU utilisation reported without a DCGM endpoint: %v", snap.GPUUtilizationPct)
	}
	// The first scrape of a counter has nothing to difference against.
	if snap.TokensPerSecond.OK {
		t.Errorf("a rate was reported from a single scrape: %v", snap.TokensPerSecond)
	}
}

func TestCounterRatesNeedTwoScrapes(t *testing.T) {
	var gen atomic.Int64
	gen.Store(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(2, 0.5, float64(gen.Load()), 5000, 200))
	}))
	defer srv.Close()

	store := telemetry.NewStore()
	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, ModelName: "m", MetricsURL: srv.URL,
	}}, store, testOptions())

	// Drive the clock so the rate is exact instead of depending on how long
	// two HTTP round trips took.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	target := c.Targets()[0]
	if _, err := c.Scrape(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	now = now.Add(10 * time.Second)
	gen.Store(1000 + 4000)
	snap, err := c.Scrape(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.TokensPerSecond.OK {
		t.Fatal("no token rate after two scrapes")
	}
	if snap.TokensPerSecond.Value != 400 {
		t.Errorf("tokens/sec = %v, want 400 (4000 tokens over 10s)", snap.TokensPerSecond.Value)
	}
}

// A counter reset means the serving process restarted. Reporting a rate of zero
// would make a pod that has just come back look idle and would fire the
// low-throughput rule at the worst possible moment.
func TestCounterResetReportsUnmeasuredNotZero(t *testing.T) {
	var gen atomic.Int64
	gen.Store(50000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(2, 0.5, float64(gen.Load()), 5000, 200))
	}))
	defer srv.Close()

	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, ModelName: "m", MetricsURL: srv.URL,
	}}, telemetry.NewStore(), testOptions())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	target := c.Targets()[0]

	_, _ = c.Scrape(context.Background(), target)
	now = now.Add(10 * time.Second)
	gen.Store(120) // the process restarted
	snap, err := c.Scrape(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TokensPerSecond.OK {
		t.Errorf("a rate of %v was reported across a counter reset; it should be unmeasured",
			snap.TokensPerSecond.Value)
	}

	// The next interval must recover cleanly from the new baseline.
	now = now.Add(10 * time.Second)
	gen.Store(120 + 2000)
	snap, err = c.Scrape(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.TokensPerSecond.OK || snap.TokensPerSecond.Value != 200 {
		t.Errorf("after a reset the next rate = %v, want 200", snap.TokensPerSecond)
	}
}

func TestDCGMSuppliesGPUMetrics(t *testing.T) {
	metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(2, 0.5, 1000, 5000, 200))
	}))
	defer metricsSrv.Close()

	// Two GPUs: one idle, one saturated. Taking the maximum is what surfaces
	// the saturated device; averaging would hide it.
	dcgmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="A"} 12
DCGM_FI_DEV_FB_USED{gpu="0",UUID="A"} 10000
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="A"} 70000
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="B"} 97
DCGM_FI_DEV_FB_USED{gpu="1",UUID="B"} 78000
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="B"} 2000
`)
	}))
	defer dcgmSrv.Close()

	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, ModelName: "m",
		MetricsURL: metricsSrv.URL, DCGMURL: dcgmSrv.URL,
	}}, telemetry.NewStore(), testOptions())

	snap, err := c.Scrape(context.Background(), c.Targets()[0])
	if err != nil {
		t.Fatal(err)
	}
	if !snap.GPUUtilizationPct.OK || snap.GPUUtilizationPct.Value != 97 {
		t.Errorf("GPU utilisation = %v, want 97 (max across devices)", snap.GPUUtilizationPct)
	}
	if !snap.GPUMemoryUsedPct.OK || snap.GPUMemoryUsedPct.Value != 97.5 {
		t.Errorf("GPU memory = %v, want 97.5", snap.GPUMemoryUsedPct)
	}
}

// A DCGM endpoint that is down must not take the whole scrape with it: the
// inference telemetry is still worth having.
func TestDCGMFailureDegradesGracefully(t *testing.T) {
	metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(9, 0.5, 1000, 5000, 200))
	}))
	defer metricsSrv.Close()
	dcgmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer dcgmSrv.Close()

	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, ModelName: "m",
		MetricsURL: metricsSrv.URL, DCGMURL: dcgmSrv.URL,
	}}, telemetry.NewStore(), testOptions())

	snap, err := c.Scrape(context.Background(), c.Targets()[0])
	if err != nil {
		t.Fatalf("a DCGM failure should not fail the scrape: %v", err)
	}
	if !snap.RequestsWaiting.OK {
		t.Error("inference telemetry was lost along with the GPU metrics")
	}
	if snap.GPUUtilizationPct.OK {
		t.Error("GPU utilisation was reported despite the DCGM scrape failing")
	}
}

// A failed scrape must not write a snapshot. A zeroed one would let rules
// diagnose a workload the collector cannot see, and would stop the staleness
// rule from ever firing.
// A persistent fault should produce one warning and a recovery message, not one
// line per scrape forever. The counters in /metrics carry the volume.
func TestRepeatedFailuresAreLoggedOnce(t *testing.T) {
	var mu sync.Mutex
	var warnings, infos int
	handler := countingHandler{
		mu: &mu,
		onRecord: func(level slog.Level) {
			switch level {
			case slog.LevelWarn:
				warnings++
			case slog.LevelInfo:
				infos++
			}
		},
	}

	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, vllmPayload(1, 0.5, 1, 1, 1))
	}))
	defer srv.Close()

	opts := testOptions()
	opts.Logger = slog.New(handler)
	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, ModelName: "m", MetricsURL: srv.URL,
	}}, telemetry.NewStore(), opts)

	for i := 0; i < 5; i++ {
		c.ScrapeAll(context.Background())
	}
	mu.Lock()
	got := warnings
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d warnings for 5 consecutive failures, want 1", got)
	}

	healthy.Store(true)
	c.ScrapeAll(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if infos == 0 {
		t.Error("recovery was not reported")
	}
}

// countingHandler counts records by level and discards them.
type countingHandler struct {
	mu       *sync.Mutex
	onRecord func(slog.Level)
	attrs    []slog.Attr
}

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onRecord(r.Level)
	return nil
}
func (h countingHandler) WithAttrs(a []slog.Attr) slog.Handler {
	h.attrs = append(h.attrs, a...)
	return h
}
func (h countingHandler) WithGroup(string) slog.Handler { return h }

func TestFailedScrapeWritesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	store := telemetry.NewStore()
	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, MetricsURL: srv.URL,
	}}, store, testOptions())

	c.ScrapeAll(context.Background())
	if got := store.Size(); got != 0 {
		t.Errorf("store holds %d snapshots after a failed scrape, want 0", got)
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Far more than the configured cap.
		payload := strings.Repeat("some_metric 1\n", 20000)
		fmt.Fprint(w, payload)
	}))
	defer srv.Close()

	opts := testOptions()
	opts.MaxBodyBytes = 1024
	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, MetricsURL: srv.URL,
	}}, telemetry.NewStore(), opts)

	_, err := c.Scrape(context.Background(), c.Targets()[0])
	if err == nil {
		t.Fatal("an oversized response was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error should explain the limit, got: %v", err)
	}
}

func TestSlowTargetIsCutOffByTheTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	opts := testOptions()
	opts.Timeout = 100 * time.Millisecond
	c := newTestCollector(t, []Target{{
		WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, MetricsURL: srv.URL,
	}}, telemetry.NewStore(), opts)

	start := time.Now()
	if _, err := c.Scrape(context.Background(), c.Targets()[0]); err == nil {
		t.Fatal("a hung target did not produce an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("scrape took %s; the timeout did not apply", elapsed)
	}
}

func TestTargetValidation(t *testing.T) {
	tests := []struct {
		name    string
		target  Target
		wantErr string
	}{
		{
			name:    "missing name",
			target:  Target{Runtime: telemetry.RuntimeVLLM, MetricsURL: "http://x/metrics"},
			wantErr: "workload_name",
		},
		{
			name:    "unknown runtime",
			target:  Target{WorkloadName: "w", Runtime: "tensorrt", MetricsURL: "http://x/metrics"},
			wantErr: "unsupported runtime",
		},
		{
			// Scrape targets come from a ConfigMap that more people can edit
			// than can edit the Deployment; restricting the scheme keeps a
			// target from being pointed somewhere a GET does not belong.
			name:    "file scheme",
			target:  Target{WorkloadName: "w", Runtime: telemetry.RuntimeVLLM, MetricsURL: "file:///etc/passwd"},
			wantErr: "scheme",
		},
		{
			name:    "no host",
			target:  Target{WorkloadName: "w", Runtime: telemetry.RuntimeVLLM, MetricsURL: "http:///metrics"},
			wantErr: "host",
		},
		{
			name:    "empty URL",
			target:  Target{WorkloadName: "w", Runtime: telemetry.RuntimeVLLM},
			wantErr: "empty",
		},
		{
			name:    "bad DCGM URL",
			target:  Target{WorkloadName: "w", Runtime: telemetry.RuntimeVLLM, MetricsURL: "http://x/metrics", DCGMURL: "gopher://x"},
			wantErr: "dcgm_url",
		},
		{
			name:   "valid",
			target: Target{WorkloadName: "w", Runtime: telemetry.RuntimeVLLM, MetricsURL: "https://x:8000/metrics"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.target.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// Two targets with the same key would overwrite each other's telemetry, and the
// resulting recommendations would flip between two workloads at random.
func TestDuplicateTargetsAreRejected(t *testing.T) {
	_, err := New([]Target{
		{WorkloadName: "w", Namespace: "ns", Runtime: telemetry.RuntimeVLLM, MetricsURL: "http://a/metrics"},
		{WorkloadName: "w", Namespace: "ns", Runtime: telemetry.RuntimeVLLM, MetricsURL: "http://b/metrics"},
	}, telemetry.NewStore(), testOptions())
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected a duplicate-target error, got %v", err)
	}
}

func TestTimeoutMustBeShorterThanInterval(t *testing.T) {
	opts := testOptions()
	opts.Interval = time.Second
	opts.Timeout = 2 * time.Second
	_, err := New(nil, telemetry.NewStore(), opts)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected the overlapping-scrape guard to reject this, got %v", err)
	}
}

type staticWorkloads struct{}

func (staticWorkloads) Replicas(_, name string) (int32, int32, int32, bool) {
	if name == "llm" {
		return 4, 2, 8, true
	}
	return 0, 0, 0, false
}

func TestKubernetesContextIsJoined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(2, 0.5, 1000, 5000, 200))
	}))
	defer srv.Close()

	opts := testOptions()
	opts.Workloads = staticWorkloads{}
	c := newTestCollector(t, []Target{
		{WorkloadName: "llm", Runtime: telemetry.RuntimeVLLM, ModelName: "m", MetricsURL: srv.URL},
		{WorkloadName: "unknown", Runtime: telemetry.RuntimeVLLM, ModelName: "m", MetricsURL: srv.URL},
	}, telemetry.NewStore(), opts)

	known, err := c.Scrape(context.Background(), c.Targets()[0])
	if err != nil {
		t.Fatal(err)
	}
	if known.Replicas.Value != 4 || known.ReadyReplicas.Value != 2 || known.MaxReplicas.Value != 8 {
		t.Errorf("replica context = %v/%v/%v, want 4/2/8",
			known.Replicas, known.ReadyReplicas, known.MaxReplicas)
	}

	// A workload the watcher does not know about must stay unmeasured rather
	// than defaulting to zero replicas, which would look like an outage.
	unknown, err := c.Scrape(context.Background(), c.Targets()[1])
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Replicas.OK {
		t.Errorf("replicas reported for an undiscovered workload: %v", unknown.Replicas)
	}
}

func TestScrapeAllIsConcurrentAndBounded(t *testing.T) {
	var inFlight, peak atomic.Int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		fmt.Fprint(w, vllmPayload(1, 0.5, 1, 1, 1))
	}))
	defer srv.Close()

	targets := make([]Target, 12)
	for i := range targets {
		targets[i] = Target{
			WorkloadName: fmt.Sprintf("w%d", i),
			Runtime:      telemetry.RuntimeVLLM, ModelName: "m", MetricsURL: srv.URL,
		}
	}
	opts := testOptions()
	opts.Concurrency = 3
	store := telemetry.NewStore()
	c := newTestCollector(t, targets, store, opts)

	c.ScrapeAll(context.Background())

	if got := len(store.Latest()); got != 12 {
		t.Errorf("collected %d workloads, want 12", got)
	}
	if p := peak.Load(); p > 3 {
		t.Errorf("peak concurrency %d exceeded the configured limit of 3", p)
	}
	if p := peak.Load(); p < 2 {
		t.Errorf("peak concurrency %d suggests scrapes ran serially", p)
	}
}

func TestScrapeAllStopsOnContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(1, 0.5, 1, 1, 1))
	}))
	defer srv.Close()

	targets := make([]Target, 50)
	for i := range targets {
		targets[i] = Target{
			WorkloadName: fmt.Sprintf("w%d", i),
			Runtime:      telemetry.RuntimeVLLM, MetricsURL: srv.URL,
		}
	}
	c := newTestCollector(t, targets, telemetry.NewStore(), testOptions())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		c.ScrapeAll(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ScrapeAll ignored a cancelled context")
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	c := newTestCollector(t, nil, telemetry.NewStore(), testOptions())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// OnCycle must fire after the first scrape and after every subsequent tick.
// OnFirstCycle and OnCycle are independent: both fire on the first cycle,
// in that order, so the API is ready before the alerter's first evaluation.
func TestOnCycleFiredAfterEachScrape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, vllmPayload(1, 0.5, 1000, 5000, 200))
	}))
	defer srv.Close()

	target := Target{
		WorkloadName: "w", Runtime: telemetry.RuntimeVLLM, MetricsURL: srv.URL,
	}

	var firstCycles, cycles atomic.Int64

	// Use a very short interval so a second tick arrives quickly.
	opts := testOptions()
	opts.Interval = 20 * time.Millisecond
	opts.Timeout = 10 * time.Millisecond
	opts.OnFirstCycle = func() { firstCycles.Add(1) }
	opts.OnCycle = func() { cycles.Add(1) }

	c := newTestCollector(t, []Target{target}, telemetry.NewStore(), opts)
	ctx, cancel := context.WithCancel(context.Background())

	go c.Run(ctx)

	// Wait for at least two OnCycle calls (first + one tick).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cycles.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	if got := cycles.Load(); got < 2 {
		t.Fatalf("OnCycle called %d time(s), want ≥ 2", got)
	}
	if got := firstCycles.Load(); got != 1 {
		t.Fatalf("OnFirstCycle called %d time(s), want exactly 1", got)
	}
	// OnFirstCycle fires before OnCycle on the first pass, so by the time
	// OnCycle has been called at least once, OnFirstCycle must be at exactly 1.
	if firstCycles.Load() != 1 {
		t.Fatalf("OnFirstCycle was called more than once")
	}
}
