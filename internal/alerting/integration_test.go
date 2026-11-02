//go:build integration

// Package alerting_test contains integration tests that exercise the complete
// alert path from metrics ingestion through the collector, recommender, and
// webhook sender. The tests are tagged //go:build integration so they do not
// run in the normal `go test ./...` pass, which has no external services.
//
// # What is tested
//
// The main entry point for these tests is the OnCycle callback wired in
// cmd/control-plane/main.go, which calls recommender.Analyze after every
// collection cycle and feeds the result to Sender.Notify. These tests verify:
//
//   - Firing: a real IFA diagnostic finding automatically produces a webhook POST.
//   - Deduplication: a persistent condition sends exactly one firing event.
//   - Resolution: removing the condition sends a resolved event.
//   - Severity filtering: findings below min_severity are silently dropped.
//   - Escalation: same-ID severity transitions fire an escalated event
//     (validated at the Sender layer because no built-in rule naturally changes
//     the severity of a single finding; see ADR 0006).
//   - HTTP failure modes: 400/401/429/500/503, timeouts, connection refusals,
//     and redirect suppression are handled without panics.
//   - Queue saturation: a full queue drops alerts and counts them without
//     blocking the collector.
//   - Graceful shutdown: Close drains queued alerts within a bounded timeout.
//   - Restart semantics: an open alert re-fires after the sender is recreated.
//   - Prometheus self-metrics: /metrics reflects actual delivery outcomes.
//   - Helm Secret configuration: the webhook URL is not rendered into a
//     ConfigMap (shell-level assertion in scripts/alerting-integration.sh).
//
// # Running locally
//
//	go test -tags integration -v -count=1 ./internal/alerting/...
package alerting_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/alerting"
	"github.com/p95labs/ifa/internal/collector"
	"github.com/p95labs/ifa/internal/metrics"
	"github.com/p95labs/ifa/internal/recommender"
	"github.com/p95labs/ifa/internal/telemetry"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// webhookReceiver records POSTed alert payloads and allows configurable
// HTTP response behaviour.
type webhookReceiver struct {
	mu sync.Mutex

	payloads []alerting.Payload
	// status is returned for every request. 0 means 200.
	status atomic.Int32
	// delay, if non-zero, is inserted before the response is written.
	delay atomic.Int64 // nanoseconds
	// hits counts every request, including rejected ones, so retry behaviour
	// is observable.
	hits atomic.Int64

	srv *httptest.Server
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)

		if d := time.Duration(r.delay.Load()); d > 0 {
			time.Sleep(d)
		}

		if code := r.status.Load(); code != 0 && code != http.StatusOK {
			w.WriteHeader(int(code))
			return
		}

		var p alerting.Payload
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.payloads = append(r.payloads, p)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *webhookReceiver) got() []alerting.Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alerting.Payload(nil), r.payloads...)
}

func (r *webhookReceiver) gotByEvent(event alerting.Event) []alerting.Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []alerting.Payload
	for _, p := range r.payloads {
		if p.Event == event {
			out = append(out, p)
		}
	}
	return out
}

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// metricsFixture is an HTTP server that serves controllable Prometheus
// exposition text. Switch() atomically replaces the served body so that tests
// can change the state seen by the next scrape without restarting anything.
//
// Each response also increments a monotonic token counter so that the vLLM
// adapter can compute a positive tokens-per-second rate. Without incrementing
// counters the rate is zero, which triggers IFA-EFF-002 (low token
// throughput) on every scrape and makes tests that assert on a single firing
// non-deterministic.
type metricsFixture struct {
	mu     sync.RWMutex
	body   string
	tokens atomic.Int64 // increments on each scrape to produce a measurable rate
	srv    *httptest.Server
}

func newMetricsFixture(t *testing.T, initial string) *metricsFixture {
	t.Helper()
	f := &metricsFixture{body: initial}
	f.tokens.Store(10000)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.RLock()
		template := f.body
		f.mu.RUnlock()

		// Advance the token counter by a large step so the computed
		// tokens/s (delta/interval) stays well above the default 200/s
		// threshold, preventing IFA-EFF-002 from firing alongside the
		// KV-cache findings we actually want to test.
		tok := f.tokens.Add(50000)
		body := strings.ReplaceAll(template, "__TOK__", fmt.Sprintf("%d", tok))
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *metricsFixture) Switch(body string) {
	f.mu.Lock()
	f.body = body
	f.mu.Unlock()
}

// vllmMetrics builds a minimal vLLM Prometheus exposition that sets
// kv_cache_usage_perc. Counters use the __TOK__ placeholder which the
// metricsFixture replaces with an incrementing value on each request, so the
// vLLM adapter can compute a measurable positive token rate and the
// IFA-EFF-002 (low token throughput) rule does not fire alongside the
// KV-cache findings under test.
//
// kvFraction is in the [0,1] range as vLLM reports it (0.95 = 95%).
func vllmMetrics(model string, kvFraction float64) string {
	return fmt.Sprintf(`# HELP vllm:num_requests_running Number of requests currently being processed.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="%s"} 2.0
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="%s"} 0.0
# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="%s"} %g
# HELP vllm:num_preemptions_total Cumulative number of preemptions from the engine.
# TYPE vllm:num_preemptions_total counter
vllm:num_preemptions_total{engine="0",model_name="%s"} 0.0
# HELP vllm:prompt_tokens_total Number of prefill tokens processed.
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{engine="0",model_name="%s"} __TOK__
# HELP vllm:generation_tokens_total Number of generation tokens processed.
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{engine="0",model_name="%s"} __TOK__
# HELP vllm:request_success_total Number of finished requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{engine="0",finished_reason="stop",model_name="%s"} 100.0
`, model, model, model, kvFraction, model, model, model, model)
}

// highKVMetrics returns metrics that will fire IFA-KV-002 (KV near capacity).
// kv_cache_usage_perc = 0.95 exceeds the default threshold of 0.90.
const kvModel = "test-model"

func highKV() string   { return vllmMetrics(kvModel, 0.95) }
func normalKV() string { return vllmMetrics(kvModel, 0.10) }

// ── Pipeline helper ───────────────────────────────────────────────────────────

// pipeline combines the real IFA components — store, recommender, alerter, and
// collector — exactly as cmd/control-plane/main.go wires them. Running it
// proves the OnCycle callback path works end to end.
type pipeline struct {
	cancel  context.CancelFunc
	store   *telemetry.Store
	engine  *recommender.Engine
	sender  *alerting.Sender
	metrics *metrics.Registry
	done    chan struct{}
}

// newPipeline creates and starts a full IFA pipeline pointed at fixtureSrv.
// The pipeline is stopped automatically when the test ends.
func newPipeline(t *testing.T, fixtureSrv *httptest.Server, webhookURL string, opts pipelineOpts) *pipeline {
	t.Helper()

	if opts.interval == 0 {
		opts.interval = 200 * time.Millisecond
	}
	if opts.minSeverity == "" {
		// Default to warning so info-level observations (e.g. IFA-OBS-001
		// "telemetry incomplete" which fires whenever latency histograms are
		// absent from the fixture) do not appear alongside the KV-cache
		// finding we are actually testing.
		opts.minSeverity = telemetry.SeverityWarning
	}

	thresholds := recommender.DefaultThresholds()
	// SustainFor=0 means Sustained() evaluates only the latest snapshot,
	// so a finding fires on the very first scrape rather than after many cycles.
	thresholds.SustainFor = 0

	store := telemetry.NewStore()
	engine := recommender.NewEngine(thresholds)
	reg := metrics.New(metrics.BuildInfo{Version: "test"})

	sender, err := alerting.New(alerting.Options{
		URL:         webhookURL,
		MinSeverity: opts.minSeverity,
		Cluster:     "test-cluster",
		Timeout:     2 * time.Second,
		Backoff:     time.Millisecond,
		QueueSize:   opts.queueSize,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatalf("alerting.New: %v", err)
	}
	reg.SetAlertStats(func() metrics.AlertStats {
		sent, failed, dropped, suppressed := sender.Stats()
		return metrics.AlertStats{Sent: sent, Failed: failed, Dropped: dropped, Suppressed: suppressed, Open: sender.Open()}
	})

	target := collector.Target{
		WorkloadName: "chat",
		Namespace:    "inference",
		Runtime:      telemetry.RuntimeVLLM,
		ModelName:    kvModel,
		MetricsURL:   fixtureSrv.URL + "/metrics",
	}

	// Timeout must be strictly less than the scrape interval.
	scrapeTimeout := opts.interval * 3 / 4
	if scrapeTimeout < 50*time.Millisecond {
		scrapeTimeout = 50 * time.Millisecond
	}

	coll, err := collector.New([]collector.Target{target}, store, collector.Options{
		Interval:    opts.interval,
		Timeout:     scrapeTimeout,
		Concurrency: 1,
		Logger:      discardLog(),
		Metrics:     reg,
		OnCycle: func() {
			recs := engine.Analyze(store.Latest(), store.History)
			sender.Notify(recs)
		},
	})
	if err != nil {
		t.Fatalf("collector.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		coll.Run(ctx)
	}()

	p := &pipeline{
		cancel:  cancel,
		store:   store,
		engine:  engine,
		sender:  sender,
		metrics: reg,
		done:    done,
	}
	t.Cleanup(func() {
		cancel()
		<-done
		sender.Close()
	})
	return p
}

type pipelineOpts struct {
	interval    time.Duration
	minSeverity telemetry.Severity
	queueSize   int
}

// prometheusMetrics fetches the /metrics endpoint from a running pipeline and
// returns its body as text.
func (p *pipeline) prometheusMetrics() string {
	var buf bytes.Buffer
	p.metrics.Write(&buf)
	return buf.String()
}

// ── Scenario A: Firing ────────────────────────────────────────────────────────

// TestIntegration_Alerting_Firing exercises the complete path from a real
// vLLM-format metric scrape through the rule engine to the webhook receiver.
// It does NOT call Sender.Notify directly: the OnCycle callback fires alerts
// automatically after each collection cycle.
func TestIntegration_Alerting_Firing(t *testing.T) {
	fixture := newMetricsFixture(t, highKV())
	recv := newWebhookReceiver(t)

	p := newPipeline(t, fixture.srv, recv.srv.URL, pipelineOpts{})

	// Wait for the first firing alert to arrive.
	waitFor(t, "firing payload", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) > 0
	})

	// Exactly one firing for one finding.
	firings := recv.gotByEvent(alerting.EventFiring)
	if len(firings) != 1 {
		t.Fatalf("expected 1 firing, got %d", len(firings))
	}

	pl := firings[0]
	t.Logf("payload: %+v", pl)

	// Structural checks.
	if pl.SchemaVersion != alerting.SchemaVersion {
		t.Errorf("schema_version: got %d, want %d", pl.SchemaVersion, alerting.SchemaVersion)
	}
	if pl.Event != alerting.EventFiring {
		t.Errorf("event: got %q, want firing", pl.Event)
	}
	if pl.ID == "" {
		t.Error("id must not be empty")
	}
	if pl.Code == "" {
		t.Error("code must not be empty")
	}
	if pl.Severity == "" {
		t.Error("severity must not be empty")
	}
	if pl.Cluster != "test-cluster" {
		t.Errorf("cluster: got %q, want test-cluster", pl.Cluster)
	}
	if pl.Namespace != "inference" {
		t.Errorf("namespace: got %q, want inference", pl.Namespace)
	}
	if pl.Workload != "chat" {
		t.Errorf("workload: got %q, want chat", pl.Workload)
	}
	if pl.Runtime == "" {
		t.Error("runtime must not be empty")
	}
	if pl.Title == "" {
		t.Error("title must not be empty")
	}
	if pl.Explanation == "" {
		t.Error("explanation must not be empty")
	}
	if pl.SuggestedAction == "" {
		t.Error("suggested_action must not be empty")
	}
	if len(pl.Evidence) == 0 {
		t.Error("evidence must not be empty")
	}
	if pl.ObservedAt.IsZero() {
		t.Error("observed_at must be set on a firing event")
	}
	if pl.SentAt.IsZero() {
		t.Error("sent_at must be set")
	}

	// The finding should correspond to IFA-KV-002 (KV near capacity, warning).
	// KV is at 0.95 > default threshold 0.90, preemptions_per_sec = 0 so
	// IFA-KV-001 does not fire.
	if pl.Code != "IFA-KV-002" {
		t.Errorf("expected IFA-KV-002, got %s", pl.Code)
	}
	if pl.Severity != "warning" {
		t.Errorf("expected warning severity, got %q", pl.Severity)
	}

	// Sender stat reflects the delivery.
	sent, failed, dropped, _ := p.sender.Stats()
	if sent != 1 || failed != 0 || dropped != 0 {
		t.Errorf("stats: sent=%d failed=%d dropped=%d, want 1/0/0", sent, failed, dropped)
	}

	// Alert is tracked as open.
	if open := p.sender.Open(); open != 1 {
		t.Errorf("Open: got %d, want 1", open)
	}
}

// ── Scenario B: Deduplication ────────────────────────────────────────────────

// TestIntegration_Alerting_Deduplication verifies that a persistent finding
// sends exactly one firing notification regardless of how many collection cycles
// pass. This is the primary guarantee of ADR 0006.
func TestIntegration_Alerting_Deduplication(t *testing.T) {
	fixture := newMetricsFixture(t, highKV())
	recv := newWebhookReceiver(t)

	// Fast interval so we accumulate many cycles quickly.
	p := newPipeline(t, fixture.srv, recv.srv.URL, pipelineOpts{interval: 100 * time.Millisecond})

	// Wait for the first firing.
	waitFor(t, "first firing", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) > 0
	})

	// Run for several more cycles. Each cycle should suppress the already-open
	// finding.
	time.Sleep(600 * time.Millisecond)

	firings := recv.gotByEvent(alerting.EventFiring)
	if len(firings) != 1 {
		t.Errorf("expected exactly 1 firing across multiple cycles, got %d — deduplication broken", len(firings))
	}

	// The suppressed counter must be climbing.
	_, _, _, suppressed := p.sender.Stats()
	if suppressed == 0 {
		t.Error("suppressed counter must be > 0 after multiple cycles with the same open finding")
	}

	// The alert must still be open (not spuriously resolved).
	if open := p.sender.Open(); open != 1 {
		t.Errorf("Open: got %d, want 1 — alert must stay open while the condition persists", open)
	}

	t.Logf("firings=%d suppressed=%d open=%d", len(firings), suppressed, p.sender.Open())
}

// ── Scenario C: Resolution ────────────────────────────────────────────────────

// TestIntegration_Alerting_FiringAndResolution verifies the full lifecycle:
// a condition fires, the condition clears, and a resolved event arrives.
func TestIntegration_Alerting_FiringAndResolution(t *testing.T) {
	fixture := newMetricsFixture(t, highKV())
	recv := newWebhookReceiver(t)

	p := newPipeline(t, fixture.srv, recv.srv.URL, pipelineOpts{interval: 150 * time.Millisecond})

	// Wait for firing.
	waitFor(t, "firing", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) > 0
	})

	// Clear the condition. The next cycle will see no matching finding and send
	// a resolved event.
	fixture.Switch(normalKV())

	waitFor(t, "resolved", func() bool {
		return len(recv.gotByEvent(alerting.EventResolved)) > 0
	})

	resolutions := recv.gotByEvent(alerting.EventResolved)
	if len(resolutions) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(resolutions))
	}

	res := resolutions[0]
	if res.Event != alerting.EventResolved {
		t.Errorf("resolved event: got %q", res.Event)
	}
	// The resolved payload must carry the same ID as the firing payload.
	firingID := recv.gotByEvent(alerting.EventFiring)[0].ID
	if res.ID != firingID {
		t.Errorf("resolved ID %q != firing ID %q — cannot correlate alert lifecycle", res.ID, firingID)
	}
	// The previous_severity must be set on a resolved event.
	if res.PreviousSeverity == "" {
		t.Error("previous_severity must be set on resolved event")
	}
	// ObservedAt is zero on a resolved event (no current measurement).
	if !res.ObservedAt.IsZero() {
		t.Errorf("resolved event must have zero observed_at, got %v", res.ObservedAt)
	}
	if res.SentAt.IsZero() {
		t.Error("sent_at must be set on resolved event")
	}

	// After resolution the alert map is empty.
	if open := p.sender.Open(); open != 0 {
		t.Errorf("Open after resolution: got %d, want 0", open)
	}

	// Resolved event is not repeated.
	time.Sleep(400 * time.Millisecond)
	if n := len(recv.gotByEvent(alerting.EventResolved)); n != 1 {
		t.Errorf("resolved must not repeat: got %d resolved events", n)
	}

	t.Logf("firing.id=%s resolved.prev_severity=%s", firingID, res.PreviousSeverity)
}

// ── Scenario D: Escalation ────────────────────────────────────────────────────

// TestIntegration_Alerting_Escalation verifies the escalated event. Because no
// built-in rule changes the severity of a finding while preserving its ID (the
// two KV rules have different codes), escalation is validated at the Sender
// integration layer by directly calling Notify with the same ID at two
// severity levels. This is the approach prescribed by the mission brief and
// documented in ADR 0006.
//
// The test confirms the Sender correctly:
//   - Fires a warning.
//   - Fires an escalated event when the same ID becomes critical.
//   - Carries the previous severity in the escalated payload.
//   - Suppresses subsequent cycles at the new severity.
func TestIntegration_Alerting_Escalation(t *testing.T) {
	recv := newWebhookReceiver(t)
	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		Cluster:     "test-cluster",
		Timeout:     2 * time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := telemetry.Recommendation{
		ID:              "IFA-KV-002:inference/chat",
		Code:            "IFA-KV-002",
		Namespace:       "inference",
		WorkloadName:    "chat",
		Runtime:         telemetry.RuntimeVLLM,
		Title:           "KV cache near capacity",
		Explanation:     "KV usage above threshold.",
		SuggestedAction: "Reduce concurrency.",
		Evidence: []telemetry.Evidence{{
			Metric: "kv_cache_usage_percent", Observed: 92, Threshold: 90, Comparison: ">", Unit: "%",
		}},
		ObservedAt: time.Now(),
	}

	// First notification: warning severity.
	warn := base
	warn.Severity = telemetry.SeverityWarning
	s.Notify([]telemetry.Recommendation{warn})
	waitFor(t, "warning firing", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) == 1
	})

	// Same ID, now critical — must escalate.
	crit := base
	crit.Severity = telemetry.SeverityCritical
	crit.Evidence[0].Observed = 99
	s.Notify([]telemetry.Recommendation{crit})
	waitFor(t, "escalated event", func() bool {
		return len(recv.gotByEvent(alerting.EventEscalated)) == 1
	})

	esc := recv.gotByEvent(alerting.EventEscalated)[0]
	if esc.Event != alerting.EventEscalated {
		t.Errorf("event: got %q, want escalated", esc.Event)
	}
	if esc.PreviousSeverity != "warning" {
		t.Errorf("previous_severity: got %q, want warning", esc.PreviousSeverity)
	}
	if esc.Severity != "critical" {
		t.Errorf("severity after escalation: got %q, want critical", esc.Severity)
	}
	if esc.ID != base.ID {
		t.Errorf("id: got %q, want %q", esc.ID, base.ID)
	}

	// A subsequent cycle at the same critical severity must suppress.
	s.Notify([]telemetry.Recommendation{crit})
	time.Sleep(50 * time.Millisecond)
	if n := len(recv.gotByEvent(alerting.EventEscalated)); n != 1 {
		t.Errorf("escalated must not repeat: got %d events", n)
	}
	_, _, _, suppressed := s.Stats()
	if suppressed == 0 {
		t.Error("suppressed must be > 0 after repeated same-severity cycle")
	}

	t.Logf("firing=1 escalated=1 suppressed=%d", suppressed)
}

// ── Scenario E: Severity filtering ────────────────────────────────────────────

// TestIntegration_Alerting_SeverityFiltering verifies that findings below the
// configured threshold are silently dropped and do not generate false resolution
// events later.
func TestIntegration_Alerting_SeverityFiltering(t *testing.T) {
	recv := newWebhookReceiver(t)

	// min_severity=critical: warning findings must be dropped.
	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityCritical,
		Timeout:     time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	warnRec := telemetry.Recommendation{
		ID: "IFA-KV-002:inf/w", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "w",
		Title: "KV near capacity",
	}
	critRec := telemetry.Recommendation{
		ID: "IFA-KV-001:inf/w", Code: "IFA-KV-001",
		Severity: telemetry.SeverityCritical, WorkloadName: "w",
		Title: "KV preemptions",
	}

	// Both present: only the critical one must be delivered.
	s.Notify([]telemetry.Recommendation{warnRec, critRec})
	waitFor(t, "critical firing", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) == 1
	})

	// Now clear all findings (empty list).
	// The warning was never tracked, so no false resolution for it.
	// The critical was tracked, so a resolved event should arrive.
	s.Notify(nil)
	waitFor(t, "critical resolved", func() bool {
		return len(recv.gotByEvent(alerting.EventResolved)) == 1
	})

	firings := recv.gotByEvent(alerting.EventFiring)
	if len(firings) != 1 {
		t.Errorf("expected exactly 1 firing (critical), got %d", len(firings))
	}
	if firings[0].Code != "IFA-KV-001" {
		t.Errorf("expected critical IFA-KV-001, got %s", firings[0].Code)
	}
	if n := len(recv.gotByEvent(alerting.EventResolved)); n != 1 {
		t.Errorf("expected 1 resolved (for critical), got %d", n)
	}

	// Verify the warning finding did not appear.
	for _, p := range recv.got() {
		if p.Code == "IFA-KV-002" {
			t.Errorf("warning finding IFA-KV-002 was delivered but should have been filtered")
		}
	}

	t.Logf("filtered warning, delivered critical, sent=%d", func() int64 {
		s, _, _, _ := s.Stats()
		return s
	}())
}

// ── HTTP failure modes ────────────────────────────────────────────────────────

// TestIntegration_Alerting_HTTPFailureModes verifies the retry policy for each
// response class that the sender can encounter.
func TestIntegration_Alerting_HTTPFailureModes(t *testing.T) {
	type testCase struct {
		name     string
		status   int32
		wantHits int64 // total HTTP attempts (1 = no retry, 2 = one retry)
		wantFail bool  // whether the delivery ends up as a failure
	}

	cases := []testCase{
		// Non-retryable 4xx: the server rejected this specific payload.
		{"400 BadRequest not retried", 400, 1, true},
		{"401 Unauthorized not retried", 401, 1, true},
		{"403 Forbidden not retried", 403, 1, true},
		{"404 NotFound not retried", 404, 1, true},
		// Retryable responses: transient server problems.
		{"429 TooManyRequests retried once", 429, 2, true},
		{"500 InternalServerError retried once", 500, 2, true},
		{"503 ServiceUnavailable retried once", 503, 2, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recv := newWebhookReceiver(t)
			recv.status.Store(tc.status)

			s, err := alerting.New(alerting.Options{
				URL:         recv.srv.URL,
				MinSeverity: telemetry.SeverityInfo,
				Timeout:     time.Second,
				Backoff:     time.Millisecond,
				Logger:      discardLog(),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			r := telemetry.Recommendation{
				ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
				Severity: telemetry.SeverityWarning, WorkloadName: "wl",
				Title: "KV near capacity",
			}
			s.Notify([]telemetry.Recommendation{r})

			if tc.wantFail {
				waitFor(t, "failure counted", func() bool {
					_, failed, _, _ := s.Stats()
					return failed == 1
				})
			}
			// Allow a brief pause so that any spurious extra attempt could
			// arrive.
			time.Sleep(30 * time.Millisecond)

			if got := recv.hits.Load(); got != tc.wantHits {
				t.Errorf("HTTP attempts: got %d, want %d", got, tc.wantHits)
			}
			sent, failed, _, _ := s.Stats()
			if tc.wantFail {
				if failed != 1 || sent != 0 {
					t.Errorf("stats: sent=%d failed=%d; want sent=0 failed=1", sent, failed)
				}
			} else {
				if sent != 1 || failed != 0 {
					t.Errorf("stats: sent=%d failed=%d; want sent=1 failed=0", sent, failed)
				}
			}
		})
	}
}

// TestIntegration_Alerting_RetrySucceeds verifies that a transient 500 on the
// first attempt does not count as a final failure when the second attempt
// returns 200.
func TestIntegration_Alerting_RetrySucceeds(t *testing.T) {
	var hits atomic.Int64
	var mu sync.Mutex
	var received []alerting.Payload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var p alerting.Payload
		_ = json.NewDecoder(req.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := alerting.New(alerting.Options{
		URL:         srv.URL,
		MinSeverity: telemetry.SeverityWarning,
		Timeout:     time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := telemetry.Recommendation{
		ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "wl",
		Title: "KV near capacity",
	}
	s.Notify([]telemetry.Recommendation{r})

	waitFor(t, "successful delivery after retry", func() bool {
		sent, _, _, _ := s.Stats()
		return sent == 1
	})

	_, failed, _, _ := s.Stats()
	if failed != 0 {
		t.Errorf("failed=%d; retry succeeded so failed must be 0", failed)
	}
	if hits.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", hits.Load())
	}
}

// TestIntegration_Alerting_Timeout verifies that a receiver that takes longer
// than the configured timeout is counted as a failure without hanging the test.
func TestIntegration_Alerting_Timeout(t *testing.T) {
	recv := newWebhookReceiver(t)
	recv.delay.Store(int64(2 * time.Second)) // longer than the 500 ms timeout

	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     500 * time.Millisecond,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := telemetry.Recommendation{
		ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "wl",
		Title: "KV near capacity",
	}
	s.Notify([]telemetry.Recommendation{r})

	waitFor(t, "timeout failure counted", func() bool {
		_, failed, _, _ := s.Stats()
		return failed >= 1
	})

	sent, _, _, _ := s.Stats()
	if sent != 0 {
		t.Errorf("sent=%d; a timed-out delivery must not count as sent", sent)
	}
}

// TestIntegration_Alerting_ConnectionRefused verifies that a delivery to a
// port with nothing listening is counted as a failure without panicking.
func TestIntegration_Alerting_ConnectionRefused(t *testing.T) {
	// Find a free port and immediately close it so it is definitively refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	s, err := alerting.New(alerting.Options{
		URL:         "http://" + addr + "/hook",
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     500 * time.Millisecond,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := telemetry.Recommendation{
		ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "wl",
		Title: "KV near capacity",
	}
	s.Notify([]telemetry.Recommendation{r})

	waitFor(t, "connection-refused failure counted", func() bool {
		_, failed, _, _ := s.Stats()
		return failed >= 1
	})
}

// TestIntegration_Alerting_RedirectNotFollowed verifies that the sender does
// not follow HTTP redirects, per the security comment in sender.go.
func TestIntegration_Alerting_RedirectNotFollowed(t *testing.T) {
	var finalHits atomic.Int64
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		finalHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()

	// Redirect target → final.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/hook", http.StatusFound)
	}))
	defer redirect.Close()

	s, err := alerting.New(alerting.Options{
		URL:         redirect.URL + "/hook",
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := telemetry.Recommendation{
		ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "wl",
		Title: "KV near capacity",
	}
	s.Notify([]telemetry.Recommendation{r})

	waitFor(t, "redirect failure counted", func() bool {
		_, failed, _, _ := s.Stats()
		return failed >= 1
	})

	// The payload must not have reached the redirect destination.
	if finalHits.Load() > 0 {
		t.Error("redirect was followed: payload reached the redirect destination — credentials would follow redirects")
	}
}

// TestIntegration_Alerting_FailedDeliveryState verifies what happens to the
// sender's open-alert state when delivery permanently fails (both attempts
// fail). The sender records the alert as open (it was enqueued and attempted)
// but the subscriber never received it. This is at-most-once semantics: the
// failed delivery is not replayed.
func TestIntegration_Alerting_FailedDeliveryState(t *testing.T) {
	recv := newWebhookReceiver(t)
	recv.status.Store(http.StatusBadRequest) // permanent failure

	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := telemetry.Recommendation{
		ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "wl",
		Title: "KV near capacity",
	}
	s.Notify([]telemetry.Recommendation{r})

	waitFor(t, "failure counted", func() bool {
		_, failed, _, _ := s.Stats()
		return failed == 1
	})

	// The alert must be tracked as open (attempted) so the next evaluation
	// that shows the finding absent can attempt a resolution.
	if open := s.Open(); open != 1 {
		t.Errorf("Open after failed delivery: got %d, want 1 — "+
			"a permanently failed delivery is still a known-open alert", open)
	}

	// Clearing the finding causes a resolution attempt.
	recv.status.Store(http.StatusOK) // recover the endpoint
	s.Notify(nil)
	waitFor(t, "resolution after recovery", func() bool {
		return len(recv.gotByEvent(alerting.EventResolved)) > 0
	})
}

// ── Queue saturation and recovery ────────────────────────────────────────────

// TestIntegration_Alerting_QueueSaturation verifies that a full queue never
// blocks Notify, increments ifa_alerts_dropped_total, and leaves the collector
// unaffected.
//
// The receiver holds every connection open until the test releases it, so the
// single sender goroutine stays busy and the queue fills up.
func TestIntegration_Alerting_QueueSaturation(t *testing.T) {
	release := make(chan struct{})

	// Blocking receiver: accepts the connection but stalls the response.
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer blocking.Close()

	s, err := alerting.New(alerting.Options{
		URL:         blocking.URL,
		MinSeverity: telemetry.SeverityInfo,
		QueueSize:   4,
		Timeout:     5 * time.Second, // long enough to not timeout before release
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		s.Close()
	}()

	// Generate more findings than the queue can hold.
	findings := make([]telemetry.Recommendation, 50)
	for i := range findings {
		findings[i] = telemetry.Recommendation{
			ID:           fmt.Sprintf("IFA-KV-%03d:ns/wl", i),
			Code:         "IFA-KV-002",
			Severity:     telemetry.SeverityWarning,
			WorkloadName: "wl",
			Title:        "KV near capacity",
		}
	}

	done := make(chan struct{})
	go func() {
		s.Notify(findings)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Notify blocked on a full queue — the collector's OnCycle would stall")
	}

	_, _, dropped, _ := s.Stats()
	if dropped == 0 {
		t.Error("dropped counter must be > 0 when the queue overflows")
	}
	t.Logf("dropped=%d", dropped)
}

// TestIntegration_Alerting_RecoveryAfterQueueOverflow verifies that after a
// receiver comes back online, subsequent new events are delivered successfully.
func TestIntegration_Alerting_RecoveryAfterQueueOverflow(t *testing.T) {
	recv := newWebhookReceiver(t)
	// Start with the receiver down.
	recv.status.Store(http.StatusServiceUnavailable)

	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		QueueSize:   4,
		Timeout:     200 * time.Millisecond,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Saturate the queue with findings that will fail delivery.
	for i := range 10 {
		r := telemetry.Recommendation{
			ID:           fmt.Sprintf("IFA-KV-%03d:ns/wl", i),
			Code:         "IFA-KV-002",
			Severity:     telemetry.SeverityWarning,
			WorkloadName: "wl",
			Title:        "KV near capacity",
		}
		s.Notify([]telemetry.Recommendation{r})
	}

	// Bring the receiver back. A new distinct finding (different ID) should
	// now be delivered.
	waitFor(t, "some failures accumulated", func() bool {
		_, failed, _, _ := s.Stats()
		return failed > 0
	})
	recv.status.Store(0)

	fresh := telemetry.Recommendation{
		ID:           "IFA-KV-FRESH:ns/wl",
		Code:         "IFA-KV-002",
		Severity:     telemetry.SeverityWarning,
		WorkloadName: "wl",
		Title:        "KV near capacity — fresh finding",
	}
	s.Notify([]telemetry.Recommendation{fresh})
	waitFor(t, "fresh delivery after recovery", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) > 0
	})

	found := false
	for _, p := range recv.got() {
		if p.ID == fresh.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("fresh finding was not delivered after receiver recovered")
	}
}

// ── Graceful shutdown ─────────────────────────────────────────────────────────

// TestIntegration_Alerting_GracefulShutdown verifies that Close drains queued
// alerts and does not discard them.
func TestIntegration_Alerting_GracefulShutdown(t *testing.T) {
	recv := newWebhookReceiver(t)

	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		QueueSize:   64,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	findings := make([]telemetry.Recommendation, n)
	for i := range n {
		findings[i] = telemetry.Recommendation{
			ID:           fmt.Sprintf("IFA-SHUT-%03d:ns/wl", i),
			Code:         "IFA-KV-002",
			Severity:     telemetry.SeverityWarning,
			WorkloadName: "wl",
			Title:        fmt.Sprintf("finding %d", i),
		}
	}
	s.Notify(findings)

	// Close must drain all queued alerts.
	s.Close()

	sent, _, _, _ := s.Stats()
	if sent != n {
		t.Errorf("Close drained %d of %d alerts; all queued alerts must be delivered on graceful shutdown", sent, n)
	}
}

// TestIntegration_Alerting_ShutdownWithSlowReceiver verifies that Close does
// not hang indefinitely when the receiver is slow. The timeout here must
// be shorter than the test deadline; we accept bounded delivery, not zero
// data loss.
func TestIntegration_Alerting_ShutdownWithSlowReceiver(t *testing.T) {
	recv := newWebhookReceiver(t)
	recv.delay.Store(int64(100 * time.Millisecond)) // 100 ms per delivery

	s, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		QueueSize:   64,
		Timeout:     200 * time.Millisecond,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}

	const n = 5
	findings := make([]telemetry.Recommendation, n)
	for i := range n {
		findings[i] = telemetry.Recommendation{
			ID:           fmt.Sprintf("IFA-SLOW-%03d:ns/wl", i),
			Code:         "IFA-KV-002",
			Severity:     telemetry.SeverityWarning,
			WorkloadName: "wl",
			Title:        fmt.Sprintf("finding %d", i),
		}
	}
	s.Notify(findings)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return within 10 seconds — shutdown must be bounded")
	}

	t.Logf("Close returned in %s", time.Since(start).Round(time.Millisecond))
}

// ── Restart semantics ─────────────────────────────────────────────────────────

// TestIntegration_Alerting_RestartRefiresOpenAlerts verifies that when the
// sender is recreated (simulating an IFA restart), a finding that was open at
// shutdown fires again on the first evaluation — as documented in ADR 0006.
func TestIntegration_Alerting_RestartRefiresOpenAlerts(t *testing.T) {
	recv := newWebhookReceiver(t)

	makeRec := func() telemetry.Recommendation {
		return telemetry.Recommendation{
			ID:           "IFA-KV-002:ns/chat",
			Code:         "IFA-KV-002",
			Severity:     telemetry.SeverityWarning,
			WorkloadName: "chat",
			Title:        "KV near capacity",
		}
	}

	// First sender: finds fires, sender is "restarted".
	s1, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s1.Notify([]telemetry.Recommendation{makeRec()})
	waitFor(t, "first firing", func() bool { return len(recv.gotByEvent(alerting.EventFiring)) == 1 })
	s1.Close()

	// Second sender (simulated restart): same condition is still active.
	// It must re-fire because the in-memory state was lost.
	s2, err := alerting.New(alerting.Options{
		URL:         recv.srv.URL,
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	s2.Notify([]telemetry.Recommendation{makeRec()})
	waitFor(t, "re-fire after restart", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) == 2
	})

	// The second firing must have the same code and ID.
	firings := recv.gotByEvent(alerting.EventFiring)
	if firings[0].ID != firings[1].ID {
		t.Errorf("refired alert ID mismatch: %q vs %q", firings[0].ID, firings[1].ID)
	}

	t.Log("restart correctly re-fires open conditions (at-most-once-per-process semantics, not global exactly-once)")
}

// ── Prometheus self-metrics ───────────────────────────────────────────────────

// TestIntegration_Alerting_PrometheusMetrics verifies that the /metrics
// endpoint exposes and correctly updates the five alert metrics.
func TestIntegration_Alerting_PrometheusMetrics(t *testing.T) {
	fixture := newMetricsFixture(t, highKV())
	recv := newWebhookReceiver(t)

	p := newPipeline(t, fixture.srv, recv.srv.URL, pipelineOpts{interval: 150 * time.Millisecond})

	// Wait for firing.
	waitFor(t, "firing", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) > 0
	})

	// Wait for at least one suppression (second cycle).
	waitFor(t, "at least one suppression", func() bool {
		_, _, _, suppressed := p.sender.Stats()
		return suppressed > 0
	})

	body := p.prometheusMetrics()
	t.Logf("prometheus metrics:\n%s", body)

	assertMetric := func(name string) {
		t.Helper()
		if !strings.Contains(body, name) {
			t.Errorf("metric %q absent from /metrics output", name)
		}
	}

	assertMetric("ifa_alerts_sent_total")
	assertMetric("ifa_alert_failures_total")
	assertMetric("ifa_alerts_dropped_total")
	assertMetric("ifa_alerts_suppressed_total")
	assertMetric("ifa_alerts_open")

	// Values must reflect actual outcomes.
	if !strings.Contains(body, "ifa_alerts_sent_total 1") {
		t.Errorf("ifa_alerts_sent_total must be 1 after one firing, got:\n%s", body)
	}
	if !strings.Contains(body, "ifa_alert_failures_total 0") {
		t.Errorf("ifa_alert_failures_total must be 0 on a healthy delivery, got:\n%s", body)
	}
	if !strings.Contains(body, "ifa_alerts_open 1") {
		t.Errorf("ifa_alerts_open must be 1 while the finding persists, got:\n%s", body)
	}

	// Now resolve the finding and verify the open gauge drops to 0.
	fixture.Switch(normalKV())
	waitFor(t, "resolved", func() bool {
		return len(recv.gotByEvent(alerting.EventResolved)) > 0
	})

	body2 := p.prometheusMetrics()
	if !strings.Contains(body2, "ifa_alerts_open 0") {
		t.Errorf("ifa_alerts_open must be 0 after resolution, got:\n%s", body2)
	}
	// Sent counter must now be 2 (firing + resolution).
	if !strings.Contains(body2, "ifa_alerts_sent_total 2") {
		t.Errorf("ifa_alerts_sent_total must be 2 after firing+resolution, got:\n%s", body2)
	}
}

// TestIntegration_Alerting_MetricsDisabledWhenAlertingOff verifies that when
// alerting is not configured, the /metrics output does not expose alert metrics
// (there is no stats callback, so the lines are simply absent).
func TestIntegration_Alerting_MetricsDisabledWhenAlertingOff(t *testing.T) {
	reg := metrics.New(metrics.BuildInfo{Version: "test"})
	// No SetAlertStats call.
	var buf bytes.Buffer
	reg.Write(&buf)
	body := buf.String()
	for _, name := range []string{
		"ifa_alerts_sent_total",
		"ifa_alert_failures_total",
		"ifa_alerts_dropped_total",
		"ifa_alerts_suppressed_total",
		"ifa_alerts_open",
	} {
		if strings.Contains(body, name) {
			t.Errorf("metric %q must not appear when alerting is disabled, got:\n%s", name, body)
		}
	}
}

// ── Credential redaction ──────────────────────────────────────────────────────

// TestIntegration_Alerting_CredentialRedaction verifies that a webhook URL
// containing a token is never written to log output, even when delivery fails.
// This is the end-to-end companion to the unit-level redaction tests.
func TestIntegration_Alerting_CredentialRedaction(t *testing.T) {
	const token = "secret-webhook-token-000000"

	// Point at a closed port so that a real transport error — which net/http
	// embeds the full URL into — is produced.
	var logs syncBuffer
	s, err := alerting.New(alerting.Options{
		URL:         "http://127.0.0.1:1/services/T0000/" + token,
		MinSeverity: telemetry.SeverityInfo,
		Timeout:     200 * time.Millisecond,
		Backoff:     time.Millisecond,
		Logger: slog.New(slog.NewJSONHandler(&logs,
			&slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := telemetry.Recommendation{
		ID: "IFA-KV-002:ns/wl", Code: "IFA-KV-002",
		Severity: telemetry.SeverityWarning, WorkloadName: "wl",
		Title: "KV near capacity",
	}
	s.Notify([]telemetry.Recommendation{r})

	waitFor(t, "failure logged", func() bool {
		_, failed, _, _ := s.Stats()
		return failed >= 1 && logs.String() != ""
	})

	out := logs.String()
	if strings.Contains(out, token) {
		t.Fatalf("webhook token appeared in log output:\n%s", out)
	}
	if !strings.Contains(out, "(redacted)") {
		t.Errorf("expected (redacted) marker in failure log:\n%s", out)
	}
}

// syncBuffer wraps a bytes.Buffer with a mutex so the sender goroutine and the
// test goroutine can both access it without a race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ── Full E2E pipeline: firing + resolution + Prometheus ───────────────────────

// TestIntegration_Alerting_FullPipeline is the canonical end-to-end scenario:
// metrics → collector → recommender → OnCycle → sender → receiver.
// It does NOT call Sender.Notify directly.
func TestIntegration_Alerting_FullPipeline(t *testing.T) {
	fixture := newMetricsFixture(t, highKV())
	recv := newWebhookReceiver(t)

	p := newPipeline(t, fixture.srv, recv.srv.URL, pipelineOpts{interval: 200 * time.Millisecond})

	// Phase 1: firing arrives automatically (no API call, no Notify call).
	waitFor(t, "automatic firing via OnCycle", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) > 0
	})
	pl := recv.gotByEvent(alerting.EventFiring)[0]
	t.Logf("firing: code=%s severity=%s id=%s", pl.Code, pl.Severity, pl.ID)

	// Phase 2: same condition across several cycles → exactly 1 firing.
	time.Sleep(600 * time.Millisecond)
	if n := len(recv.gotByEvent(alerting.EventFiring)); n != 1 {
		t.Errorf("deduplication: expected 1 firing, got %d", n)
	}

	// Phase 3: clear condition → resolved event.
	fixture.Switch(normalKV())
	waitFor(t, "automatic resolution via OnCycle", func() bool {
		return len(recv.gotByEvent(alerting.EventResolved)) > 0
	})
	res := recv.gotByEvent(alerting.EventResolved)[0]
	if res.ID != pl.ID {
		t.Errorf("resolved ID %q != firing ID %q", res.ID, pl.ID)
	}
	t.Logf("resolved: code=%s prev_severity=%s", res.Code, res.PreviousSeverity)

	// Phase 4: Prometheus metrics reflect the full lifecycle.
	body := p.prometheusMetrics()
	for _, name := range []string{
		"ifa_alerts_sent_total 2",    // firing + resolved
		"ifa_alerts_open 0",          // resolved
		"ifa_alert_failures_total 0", // no errors
	} {
		if !strings.Contains(body, name) {
			t.Errorf("expected %q in /metrics:\n%s", name, body)
		}
	}

	// Phase 5: re-introduce the condition → fires again (not suppressed after
	// resolution).
	fixture.Switch(highKV())
	waitFor(t, "second firing after recovery", func() bool {
		return len(recv.gotByEvent(alerting.EventFiring)) == 2
	})
	t.Log("second firing confirmed after condition re-appeared following resolution")
}
