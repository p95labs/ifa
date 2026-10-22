// Package dynserver implements a deterministic HTTP server for IFA dynamic
// multi-scrape integration testing.
//
// # Purpose
//
// The static-fixture integration test (kind-integration.sh) validates the full
// IFA pipeline using a committed verbatim vLLM capture that never changes
// between scrapes. That is intentional for the latency-rules test, but it
// means rate-dependent rules — IFA-CAP-003, IFA-EFF-002, IFA-KV-001 and
// counter-reset handling — cannot be exercised there.
//
// dynserver closes that gap. It serves synthetic Prometheus exposition at
// /scenario/{id}/metrics and, for Scenario D, synthetic DCGM exposition at
// /scenario/{id}/dcgm. Every payload is clearly labelled as synthetic
// integration-test data; none of it is a live vLLM or GPU capture.
//
// # State machines
//
// Each scenario maintains an explicit state index. A POST to
// /scenario/{id}/advance increments the index to the next state and returns the
// new index as a JSON integer. A GET to /scenario/{id}/state returns the current
// index. Tests control transitions from outside: the script advances state
// immediately after detecting that IFA has processed the previous state, so
// transitions are coupled to real IFA scrapes rather than to a fixed sleep.
//
// # Scenarios
//
//	A — counter-to-rate conversion (generation tokens, prompt tokens, requests)
//	B — growing request queue (IFA-CAP-003 trend rule)
//	C — KV-cache preemption (IFA-KV-001 and IFA-KV-002 suppression)
//	D — low token throughput with synthetic DCGM GPU metrics (IFA-EFF-002)
//	E — counter reset handling
//	F — missing telemetry and recovery
//
// # Payload labelling
//
// Every metric series in every payload carries the label
//
//	synthetic_source="dynserver-integration-test"
//
// in addition to the engine and model_name labels that vLLM emits, and the
// model_name is always "synthetic/test" so that payloads cannot be mistaken for
// captures from a real deployment.
//
// # Isolation
//
// This package is entirely within the test/ tree. It imports no IFA production
// package; the server is compiled into its own binary and loaded into kind as a
// separate image. Production binaries are unchanged.
package dynserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// scenarioState is one scenario's mutable state.
type scenarioState struct {
	mu    sync.Mutex
	index int
}

func (s *scenarioState) get() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index
}

func (s *scenarioState) advance(max int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index < max {
		s.index++
	}
	return s.index
}

// Server serves deterministic Prometheus exposition for IFA integration tests.
type Server struct {
	scenarios map[string]*scenarioState
}

// New returns an initialised Server with all scenarios at state 0.
func New() *Server {
	s := &Server{
		scenarios: map[string]*scenarioState{
			"a":        {},
			"b":        {},
			"c":        {},
			"d-eff":    {},
			"d-nodcgm": {},
			"e":        {},
			"f":        {},
		},
	}
	return s
}

// RegisterHandlers wires the server's handlers onto mux.
func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	for id := range s.scenarios {
		id := id // capture for closure
		mux.HandleFunc("/scenario/"+id+"/metrics", s.metricsHandler(id))
		mux.HandleFunc("/scenario/"+id+"/dcgm", s.dcgmHandler(id))
		mux.HandleFunc("/scenario/"+id+"/advance", s.advanceHandler(id))
		mux.HandleFunc("/scenario/"+id+"/state", s.stateHandler(id))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *Server) metricsHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sc := s.scenarios[id]
		idx := sc.get()

		payload, statusCode := s.payload(id, idx)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(statusCode)
		if payload != "" {
			fmt.Fprint(w, payload)
		}
	}
}

func (s *Server) dcgmHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sc := s.scenarios[id]
		idx := sc.get()

		payload := s.dcgmPayload(id, idx)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, payload)
	}
}

func (s *Server) advanceHandler(id string) http.HandlerFunc {
	maxStates := map[string]int{
		"a":        3,
		"b":        8, // 0–5 growing, 6–8 stable
		"c":        4,
		"d-eff":    2,
		"d-nodcgm": 1,
		"e":        3,
		"f":        3,
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sc := s.scenarios[id]
		max := maxStates[id]
		newIdx := sc.advance(max)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"state": newIdx})
	}
}

func (s *Server) stateHandler(id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sc := s.scenarios[id]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"state": sc.get()})
	}
}

// ── Payload generators ────────────────────────────────────────────────────────

// payload returns the Prometheus text exposition and HTTP status for the given
// scenario at the given state index.
func (s *Server) payload(id string, idx int) (string, int) {
	switch id {
	case "a":
		return payloadA(idx), http.StatusOK
	case "b":
		return payloadB(idx), http.StatusOK
	case "c":
		return payloadC(idx), http.StatusOK
	case "d-eff", "d-nodcgm":
		return payloadD(idx), http.StatusOK
	case "e":
		return payloadE(idx), http.StatusOK
	case "f":
		return payloadF(idx)
	}
	return "", http.StatusNotFound
}

// dcgmPayload returns the DCGM Prometheus text exposition for the given
// scenario at the given state index. Only Scenario D has meaningful DCGM data;
// other scenarios return an empty string (no GPUs found).
func (s *Server) dcgmPayload(id string, idx int) string {
	if id == "d-eff" {
		return dcgmPayloadD(idx)
	}
	return ""
}

// ── Scenario A: counter-to-rate conversion ────────────────────────────────────
//
// Validates that the collector correctly converts cumulative counters to
// per-second rates. The first observation must not produce a rate; subsequent
// observations with increasing counters must produce positive rates; equal
// counters must produce zero rates.
//
// State 0: baseline — first observation, no rate computable.
// State 1: counters advanced +1000 gen / +200 prompt / +10 requests.
//
//	Expected rate over 5 s: 200 gen/s, 40 prompt/s, 2 req/s.
//
// State 2: counters advanced by the same delta as State 1 → same rates.
// State 3: counters unchanged from State 2 → zero rates.
func payloadA(idx int) string {
	type counters struct {
		genTokens   float64
		promptTok   float64
		successStop float64
		preemptions float64
	}
	states := []counters{
		{1000, 500, 50, 0},
		{2000, 700, 60, 0},
		{3000, 900, 70, 0},
		{3000, 900, 70, 0}, // same as state 2 → zero rates
	}
	if idx >= len(states) {
		idx = len(states) - 1
	}
	c := states[idx]
	return vllmPayload("synthetic/test", 2, 1, 0.20,
		c.genTokens, c.promptTok, c.successStop, c.preemptions, 0, 0,
		2500, // ttft_p95 ms → 2.5 s → does not trigger TTFT rules at default 1000 ms threshold
		1500, // e2e_p95 ms → well below 10 000 ms threshold
		0,    // queue_time_p95 ms
	)
}

// ── Scenario B: growing request queue (IFA-CAP-003) ───────────────────────────
//
// Validates the queue-growth trend rule. Trend() requires minSamples=6 across
// SustainFor*TrendWindow = 10s*3 = 30s of history. With a 5s scrape interval,
// 6 samples fill 30s of history; Trend() then compares the mean of the first
// third with the mean of the last third.
//
// Growing phase (states 0–5): queue = 5, 7, 9, 11, 13, 15.
//
//	Early third mean (samples 0–1): (5+7)/2 = 6.
//	Late third mean  (samples 4–5): (13+15)/2 = 14.
//	Delta = 8 > 1 (threshold). Latest queue = 15 > 8 (QueueWaitingRequests).
//	→ IFA-CAP-003 fires after state 5 is processed.
//
// Stable phase (states 6–8): queue = 15, 15, 15.
//
//	After 6 stable scrapes (states 6–8 in successive windows), both thirds are
//	15 → delta = 0 ≤ 1 → IFA-CAP-003 stops firing.
//	A deep-but-stable queue does not trigger the growing-queue diagnosis.
func payloadB(idx int) string {
	queueDepths := []float64{5, 7, 9, 11, 13, 15, 15, 15, 15}
	if idx >= len(queueDepths) {
		idx = len(queueDepths) - 1
	}
	q := queueDepths[idx]
	return vllmPayload("synthetic/test", 4, q, 0.30,
		500, 200, 20, 0, 0, 0,
		300,  // ttft_p95 ms — below 1000 ms threshold, no LAT rule
		2000, // e2e_p95 ms — below 10000 ms threshold
		0,
	)
}

// ── Scenario C: KV-cache preemption (IFA-KV-001 / IFA-KV-002) ────────────────
//
// Validates KV-cache preemption detection and the suppression of IFA-KV-002
// by IFA-KV-001.
//
// State 0: KV=95%, preemptions=0.
//
//	KVNearCapacity (IFA-KV-002) requires Sustained() over 10s: does not fire yet.
//	No preemption counter baseline exists yet.
//
// State 1: KV=95%, preemptions=0.
//
//	Second scrape; still not sustained for 10s.
//
// State 2: KV=95%, preemptions=0.
//
//	Third scrape (~15s elapsed); Sustained() condition now satisfied.
//	IFA-KV-002 fires. Preemption counter baseline = 0 (first observation).
//
// State 3: KV=95%, preemptions=5.
//
//	Preemption counter now has a baseline (state 2 was first observation).
//	Rate = (5−0)/5s = 1.0/s > 0.01 threshold. IFA-KV-001 fires.
//	IFA-KV-001 supersedes IFA-KV-002 → IFA-KV-002 absent.
//
// State 4: KV=95%, preemptions=25.
//
//	Rate = (25−5)/5s = 4.0/s. IFA-KV-001 continues; IFA-KV-002 suppressed.
//
// Note: state 0 also validates high-KV-with-no-preemptions (KV-001 absent).
func payloadC(idx int) string {
	type cState struct {
		queue       float64
		kvPct       float64
		preemptions float64
	}
	states := []cState{
		{1, 0.95, 0},
		{1, 0.95, 0},
		{1, 0.95, 0},
		{1, 0.95, 5},
		{1, 0.95, 25},
	}
	if idx >= len(states) {
		idx = len(states) - 1
	}
	st := states[idx]
	return vllmPayload("synthetic/test", 2, st.queue, st.kvPct,
		500, 200, 20, st.preemptions, 0, 0,
		300, 2000, 0,
	)
}

// ── Scenario D: low token throughput + synthetic DCGM (IFA-EFF-002) ───────────
//
// Validates IFA-EFF-002, which requires both measured GPU utilisation (from a
// DCGM endpoint) and measured low token throughput (from changing counters).
//
// State 0: baseline gen_tokens=1000.
//
//	First observation; token rate not yet computed. IFA-EFF-002 cannot fire
//	without a measured rate, even though GPU utilisation is high.
//
// State 1: gen_tokens=1500.
//
//	Rate = (1500−1000)/5s = 100/s < 200 tokens/s threshold.
//	DCGM reports GPU utilisation = 92% > 85% threshold.
//	IFA-EFF-002 fires. GPU source is DCGM, not KV-cache occupancy.
//
// State 2: gen_tokens=6500.
//
//	Rate = (6500−1500)/5s = 1000/s > 200 tokens/s threshold.
//	IFA-EFF-002 should NOT fire (throughput above threshold).
//
// For dynserver-d-nodcgm, the same vLLM payload is served but no DCGM URL is
// configured in IFA. GPU utilisation stays unmeasured → IFA-EFF-002 absent.
func payloadD(idx int) string {
	genTokens := []float64{1000, 1500, 6500}
	if idx >= len(genTokens) {
		idx = len(genTokens) - 1
	}
	g := genTokens[idx]
	return vllmPayload("synthetic/test", 4, 1, 0.40,
		g, 300, 25, 0, 0, 0,
		200, 1500, 0,
	)
}

// dcgmPayloadD returns synthetic DCGM Exporter exposition for Scenario D.
// One simulated GPU reports 92% utilisation and ~80% framebuffer usage.
// This is explicitly labelled as simulated GPU-metrics integration testing;
// it does not validate physical GPU behaviour.
func dcgmPayloadD(_ int) string {
	return `# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %). Synthetic integration-test data — not a physical GPU capture.
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-synthetic-0",device="nvidia0",Hostname="dynserver-integration-test"} 92
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used (MiB). Synthetic integration-test data.
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-synthetic-0",device="nvidia0",Hostname="dynserver-integration-test"} 19200
# HELP DCGM_FI_DEV_FB_FREE Framebuffer memory free (MiB). Synthetic integration-test data.
# TYPE DCGM_FI_DEV_FB_FREE gauge
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-synthetic-0",device="nvidia0",Hostname="dynserver-integration-test"} 4800
# HELP DCGM_FI_DEV_GPU_TEMP GPU temperature (Celsius). Synthetic integration-test data.
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-synthetic-0",device="nvidia0",Hostname="dynserver-integration-test"} 65
`
}

// ── Scenario E: counter reset handling ────────────────────────────────────────
//
// Validates that the collector correctly handles a counter reset (process
// restart). When a cumulative counter decreases, the delta is negative; the
// rate tracker marks the interval as unmeasured and re-baselines from the
// new (lower) value.
//
// State 0: baseline gen_tokens=5000, preemptions=100.
// State 1: gen_tokens=3000, preemptions=80 — RESET (lower than state 0).
//
//	Rate must be unmeasured (not zero) for this interval.
//	New baseline is established at (3000, 80).
//
// State 2: gen_tokens=4000, preemptions=90 — valid increment from state 1.
//
//	Rate = (4000−3000)/5s = 200/s (generation), (90−80)/5s = 2/s (preemptions).
//
// State 3: gen_tokens=5000, preemptions=100 — valid increment from state 2.
//
//	Same rates as state 2.
func payloadE(idx int) string {
	type eState struct {
		gen         float64
		preemptions float64
	}
	states := []eState{
		{5000, 100},
		{3000, 80}, // reset
		{4000, 90},
		{5000, 100},
	}
	if idx >= len(states) {
		idx = len(states) - 1
	}
	st := states[idx]
	return vllmPayload("synthetic/test", 2, 1, 0.20,
		st.gen, 300, 25, st.preemptions, 0, 0,
		200, 1500, 0,
	)
}

// ── Scenario F: missing telemetry and recovery ────────────────────────────────
//
// Validates that IFA handles unavailable or malformed telemetry correctly.
//
// State 0: healthy metrics payload. Establishes baseline telemetry.
// State 1: server returns HTTP 503. IFA must not write a snapshot; metrics_missing
//
//	is irrelevant; a scrape-error counter must increment.
//
// State 2: server returns a payload missing all required metrics (only unrecognised
//
//	lines). The adapter sees an empty exposition; metrics_missing is
//	populated for the non-optional metrics. Values must not be silently zero.
//
// State 3: healthy payload again. IFA resumes collecting usable telemetry.
//
//	History-dependent rules (Sustained, Trend) must not fire until
//	enough valid samples have re-accumulated.
func payloadF(idx int) (string, int) {
	switch idx {
	case 0, 3:
		// Healthy payload.
		return vllmPayload("synthetic/test", 2, 1, 0.20,
			800, 300, 20, 0, 0, 0,
			300, 2000, 0,
		), http.StatusOK
	case 1:
		// Simulate an unreachable metrics endpoint.
		return "service unavailable", http.StatusServiceUnavailable
	case 2:
		// Partially valid payload: only unrecognised metric families.
		// The vLLM adapter will find no expected metric names; Missing will
		// list the required ones. The snapshot fields remain at their zero
		// values (unmeasured), not silently at zero.
		return "# HELP unknown_metric An unrecognised metric family.\n" +
			"# TYPE unknown_metric gauge\n" +
			"unknown_metric{label=\"synthetic\"} 42\n", http.StatusOK
	}
	// Beyond state 3, stay at healthy.
	return vllmPayload("synthetic/test", 2, 1, 0.20,
		800, 300, 20, 0, 0, 0,
		300, 2000, 0,
	), http.StatusOK
}

// ── vLLM payload builder ──────────────────────────────────────────────────────

// vllmPayload generates a synthetic vLLM 0.28.0-compatible Prometheus text
// exposition. All series are labelled synthetic_source="dynserver-integration-test"
// and use model_name="synthetic/test" so that payloads cannot be confused with
// captures from a real deployment.
//
// Parameters:
//
//	modelName   — model_name label value (always "synthetic/test" in practice)
//	running     — vllm:num_requests_running (gauge)
//	waiting     — vllm:num_requests_waiting (gauge)
//	kvFrac      — vllm:kv_cache_usage_perc  (fraction in [0,1])
//	genTokens   — vllm:generation_tokens_total (cumulative counter)
//	promptTok   — vllm:prompt_tokens_total (cumulative counter)
//	successStop — vllm:request_success_total{finished_reason="stop"} (counter)
//	preemptions — vllm:num_preemptions_total (cumulative counter)
//	cacheQ      — vllm:prefix_cache_queries_total (cumulative counter, 0 = omit)
//	cacheH      — vllm:prefix_cache_hits_total (cumulative counter, 0 = omit)
//	ttftP95Ms   — p95 TTFT in milliseconds (placed in a single-bucket histogram)
//	e2eP95Ms    — p95 e2e latency in milliseconds
//	queueP95Ms  — p95 queue time in milliseconds (0 = omit)
func vllmPayload(
	modelName string,
	running, waiting, kvFrac float64,
	genTokens, promptTok, successStop, preemptions float64,
	cacheQ, cacheH float64,
	ttftP95Ms, e2eP95Ms, queueP95Ms float64,
) string {
	// To produce a deterministic p95 from a histogram with two buckets, we
	// place all count in the bucket at the p95 value: count=1, bucket[le] = 1,
	// sum = value_in_seconds. The percentile interpolation then yields exactly
	// that value.
	//
	// vLLM uses specific bucket boundaries; we choose boundaries that contain
	// our target values exactly. For a single-observation histogram the p95
	// interpolation is: value = le_of_first_bucket_with_cumulative_count >= 0.95*1.
	// With count=1, that boundary is the first bucket with cumulative_count >= 1,
	// which is our chosen le. That le is returned directly when the previous
	// bucket is exactly le - (le - prev_le) * (0.95 - (prev/1)) = le. More
	// precisely: one observation in bucket B means p95 lands in B and is
	// interpolated as prev_le + (le_B - prev_le) * (rank - prev_count) / 1.
	// rank = ceil(0.95 * 1) = 1, prev_count = 0 → = prev_le + (le_B - prev_le).
	// So the interpolation returns le_B. We pick le_B = target_seconds.

	ttftSec := ttftP95Ms / 1000.0
	e2eSec := e2eP95Ms / 1000.0

	var b strings.Builder

	// Header comment clearly identifies this as synthetic test data.
	fmt.Fprintf(&b, "# dynserver-integration-test: synthetic vLLM-compatible exposition\n")
	fmt.Fprintf(&b, "# model_name=%s  — not a real vLLM deployment\n", modelName)

	// Scheduler gauges.
	gauge(&b, "vllm:num_requests_running", "Number of requests in model execution batches.",
		modelName, f(running))
	gauge(&b, "vllm:num_requests_waiting", "Number of requests waiting to be processed.",
		modelName, f(waiting))

	// KV-cache utilisation.
	gauge(&b, "vllm:kv_cache_usage_perc", "KV-cache usage. 1 means 100 percent usage.",
		modelName, f(kvFrac))

	// Counters — cumulative, no _created helper needed for the adapter.
	counter(&b, "vllm:generation_tokens_total", "Number of generation tokens processed.",
		modelName, f(genTokens))
	counter(&b, "vllm:prompt_tokens_total", "Number of prefill tokens processed.",
		modelName, f(promptTok))
	counter(&b, "vllm:num_preemptions_total", "Cumulative number of preemptions from the engine.",
		modelName, f(preemptions))

	// request_success_total broken down by finished_reason.
	fmt.Fprintf(&b, "# HELP vllm:request_success_total Count of successfully processed requests.\n")
	fmt.Fprintf(&b, "# TYPE vllm:request_success_total counter\n")
	for _, reason := range []string{"stop", "length", "abort", "error", "repetition"} {
		v := 0.0
		if reason == "stop" {
			v = successStop
		}
		fmt.Fprintf(&b, "vllm:request_success_total{engine=\"0\",finished_reason=%q,model_name=%q} %s\n",
			reason, modelName, f(v))
	}

	// Optional prefix-cache counters.
	if cacheQ > 0 || cacheH > 0 {
		counter(&b, "vllm:prefix_cache_queries_total",
			"Prefix cache queries, in terms of number of queried tokens.", modelName, f(cacheQ))
		counter(&b, "vllm:prefix_cache_hits_total",
			"Prefix cache hits, in terms of number of cached tokens.", modelName, f(cacheH))
	}

	// TTFT histogram — single observation at ttftP95Ms/1000 seconds.
	histogram(&b, "vllm:time_to_first_token_seconds",
		"Histogram of time to first token in seconds.",
		modelName, ttftSec,
		// vLLM TTFT bucket boundaries (seconds):
		[]float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5,
			0.75, 1.0, 2.5, 5.0, 7.5, 10.0, 20.0, 40.0, 80.0, 160.0, 640.0, 2560.0})

	// E2E latency histogram — single observation at e2eP95Ms/1000 seconds.
	histogram(&b, "vllm:e2e_request_latency_seconds",
		"Histogram of end-to-end request latency in seconds.",
		modelName, e2eSec,
		// vLLM E2E bucket boundaries (seconds):
		[]float64{0.3, 0.5, 0.75, 1.0, 2.5, 5.0, 10.0, 15.0, 20.0, 30.0, 40.0, 50.0, 60.0})

	// Optional queue-time histogram.
	if queueP95Ms > 0 {
		queueSec := queueP95Ms / 1000.0
		histogram(&b, "vllm:request_queue_time_seconds",
			"Histogram of time spent waiting in queue before scheduling.",
			modelName, queueSec,
			[]float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5,
				0.75, 1.0, 2.5, 5.0, 7.5, 10.0, 20.0, 40.0, 80.0})
	}

	return b.String()
}

// ── Exposition helpers ────────────────────────────────────────────────────────

func f(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func gauge(b *strings.Builder, name, help, modelName, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s{engine=\"0\",model_name=%q} %s\n", name, modelName, value)
}

func counter(b *strings.Builder, name, help, modelName, value string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	fmt.Fprintf(b, "%s{engine=\"0\",model_name=%q} %s\n", name, modelName, value)
}

// histogram writes a single-observation histogram with one count at valueSec.
// The approach is documented in vllmPayload: with count=1, placing all
// observations in the bucket whose le boundary equals the target value causes
// the p95 interpolation to return exactly that value.
func histogram(b *strings.Builder, name, help, modelName string, valueSec float64, buckets []float64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)

	cumCount := 0.0
	targetBucket := -1
	for i, le := range buckets {
		if le >= valueSec && targetBucket < 0 {
			targetBucket = i
		}
		if targetBucket >= 0 && i >= targetBucket {
			cumCount = 1.0
		}
		leStr := strconv.FormatFloat(le, 'f', -1, 64)
		fmt.Fprintf(b, "%s_bucket{engine=\"0\",le=%q,model_name=%q} %s\n",
			name, leStr, modelName, f(cumCount))
	}
	// +Inf bucket always holds the total count.
	fmt.Fprintf(b, "%s_bucket{engine=\"0\",le=\"+Inf\",model_name=%q} 1\n", name, modelName)
	fmt.Fprintf(b, "%s_count{engine=\"0\",model_name=%q} 1\n", name, modelName)
	fmt.Fprintf(b, "%s_sum{engine=\"0\",model_name=%q} %s\n", name, modelName, f(valueSec))
}
