package alerting

import (
	"bytes"
	"context"
	"encoding/json"
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

// discardLogger keeps test output readable. Failures are asserted on counters
// and received payloads, not on log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// receiver is a webhook endpoint that records what it was sent.
type receiver struct {
	mu       sync.Mutex
	payloads []Payload
	// status is returned to every request. 0 means 200.
	status atomic.Int32
	// hits counts requests including ones that were rejected, so retry
	// behaviour is observable.
	hits atomic.Int64
	srv  *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if code := r.status.Load(); code != 0 && code != http.StatusOK {
			w.WriteHeader(int(code))
			return
		}
		var p Payload
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

func (r *receiver) got() []Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Payload(nil), r.payloads...)
}

// rec builds a recommendation with the fields the sender reads.
func rec(code string, sev telemetry.Severity, workload string) telemetry.Recommendation {
	return telemetry.Recommendation{
		ID:              code + ":inference/" + workload,
		Code:            code,
		Severity:        sev,
		Namespace:       "inference",
		WorkloadName:    workload,
		Runtime:         telemetry.RuntimeVLLM,
		Title:           "Test finding",
		Explanation:     "Something is wrong.",
		SuggestedAction: "Do something about it.",
		Evidence: []telemetry.Evidence{{
			Metric: "kv_cache_usage_percent", Source: "vllm:kv_cache_usage_perc",
			Observed: 95, Threshold: 90, Comparison: ">", Unit: "%",
		}},
		ObservedAt:    time.Unix(1700000000, 0).UTC(),
		WindowSeconds: 45,
	}
}

// newTestSender returns a Sender pointed at the receiver, with a short backoff
// so retry tests do not spend real time waiting.
func newTestSender(t *testing.T, r *receiver, min telemetry.Severity) *Sender {
	t.Helper()
	s, err := New(Options{
		URL:         r.srv.URL,
		MinSeverity: min,
		Cluster:     "test-cluster",
		Timeout:     2 * time.Second,
		Backoff:     time.Millisecond,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// waitFor polls until cond holds or the deadline passes. The sender is
// asynchronous by design, so assertions have to wait for the queue to drain
// rather than assume it already has.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── Delivery ────────────────────────────────────────────────────────────────

func TestNotifyDeliversAFiringAlert(t *testing.T) {
	r := newReceiver(t)
	s := newTestSender(t, r, telemetry.SeverityWarning)

	s.Notify([]telemetry.Recommendation{rec("IFA-KV-001", telemetry.SeverityCritical, "chat")})

	waitFor(t, "one delivery", func() bool {
		sent, failed, dropped, _ := s.Stats()
		return len(r.got()) == 1 && sent == 1 && failed == 0 && dropped == 0
	})

	p := r.got()[0]
	if p.Event != EventFiring {
		t.Errorf("event: got %q, want %q", p.Event, EventFiring)
	}
	if p.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version: got %d, want %d", p.SchemaVersion, SchemaVersion)
	}
	if p.ID != "IFA-KV-001:inference/chat" {
		t.Errorf("id: got %q", p.ID)
	}
	if p.Severity != "critical" {
		t.Errorf("severity: got %q, want critical", p.Severity)
	}
	if p.Cluster != "test-cluster" {
		t.Errorf("cluster: got %q, want test-cluster", p.Cluster)
	}
	if p.Workload != "chat" || p.Namespace != "inference" {
		t.Errorf("workload identity: got %s/%s", p.Namespace, p.Workload)
	}
	if len(p.Evidence) != 1 || p.Evidence[0].Observed != 95 {
		t.Errorf("evidence not carried through: %+v", p.Evidence)
	}
	if p.SentAt.IsZero() {
		t.Error("sent_at was not set")
	}

	sent, failed, dropped, _ := s.Stats()
	if sent != 1 || failed != 0 || dropped != 0 {
		t.Errorf("stats: sent=%d failed=%d dropped=%d, want 1/0/0", sent, failed, dropped)
	}
}

// ── Severity filter ─────────────────────────────────────────────────────────

func TestMinSeverityFiltersLessSevereFindings(t *testing.T) {
	tests := []struct {
		name    string
		min     telemetry.Severity
		finding telemetry.Severity
		want    bool
	}{
		{"critical threshold passes critical", telemetry.SeverityCritical, telemetry.SeverityCritical, true},
		{"critical threshold blocks warning", telemetry.SeverityCritical, telemetry.SeverityWarning, false},
		{"critical threshold blocks info", telemetry.SeverityCritical, telemetry.SeverityInfo, false},
		{"warning threshold passes critical", telemetry.SeverityWarning, telemetry.SeverityCritical, true},
		{"warning threshold passes warning", telemetry.SeverityWarning, telemetry.SeverityWarning, true},
		{"warning threshold blocks info", telemetry.SeverityWarning, telemetry.SeverityInfo, false},
		{"info threshold passes everything", telemetry.SeverityInfo, telemetry.SeverityInfo, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newReceiver(t)
			s := newTestSender(t, r, tc.min)

			s.Notify([]telemetry.Recommendation{rec("IFA-TEST-001", tc.finding, "svc")})

			if tc.want {
				waitFor(t, "delivery", func() bool { return len(r.got()) == 1 })
				return
			}
			// Nothing should arrive. Give the sender a moment to prove it.
			time.Sleep(50 * time.Millisecond)
			if got := len(r.got()); got != 0 {
				t.Errorf("expected no delivery for %s under %s threshold, got %d",
					tc.finding, tc.min, got)
			}
			if s.Open() != 0 {
				t.Error("a filtered finding must not be tracked as open, " +
					"or it would later produce a resolved alert for something never sent")
			}
		})
	}
}

// ── Deduplication ───────────────────────────────────────────────────────────

func TestRepeatedEvaluationSendsOnceAndCountsSuppressions(t *testing.T) {
	r := newReceiver(t)
	s := newTestSender(t, r, telemetry.SeverityWarning)

	finding := rec("IFA-KV-001", telemetry.SeverityCritical, "chat")

	// The rule engine re-derives this finding on every cycle. Ten cycles is ten
	// identical findings, and must be one alert.
	for i := 0; i < 10; i++ {
		s.Notify([]telemetry.Recommendation{finding})
	}

	waitFor(t, "the first delivery", func() bool { return len(r.got()) >= 1 })
	time.Sleep(50 * time.Millisecond)

	if got := len(r.got()); got != 1 {
		t.Fatalf("ten evaluations of one finding produced %d alerts, want 1", got)
	}
	if _, _, _, suppressed := s.Stats(); suppressed != 9 {
		t.Errorf("suppressed: got %d, want 9", suppressed)
	}
}

func TestEscalationSendsASecondAlertAndDeEscalationDoesNot(t *testing.T) {
	r := newReceiver(t)
	s := newTestSender(t, r, telemetry.SeverityWarning)

	s.Notify([]telemetry.Recommendation{rec("IFA-Q-001", telemetry.SeverityWarning, "chat")})
	waitFor(t, "the warning", func() bool { return len(r.got()) == 1 })

	s.Notify([]telemetry.Recommendation{rec("IFA-Q-001", telemetry.SeverityCritical, "chat")})
	waitFor(t, "the escalation", func() bool { return len(r.got()) == 2 })

	got := r.got()
	if got[1].Event != EventEscalated {
		t.Errorf("second event: got %q, want %q", got[1].Event, EventEscalated)
	}
	if got[1].PreviousSeverity != "warning" {
		t.Errorf("previous_severity: got %q, want warning", got[1].PreviousSeverity)
	}
	if got[1].Severity != "critical" {
		t.Errorf("severity: got %q, want critical", got[1].Severity)
	}

	// Dropping back to warning is the same open problem. It must not produce a
	// third alert.
	s.Notify([]telemetry.Recommendation{rec("IFA-Q-001", telemetry.SeverityWarning, "chat")})
	time.Sleep(50 * time.Millisecond)
	if n := len(r.got()); n != 2 {
		t.Errorf("de-escalation produced an alert: got %d deliveries, want 2", n)
	}
}

func TestFindingDisappearingSendsResolved(t *testing.T) {
	r := newReceiver(t)
	s := newTestSender(t, r, telemetry.SeverityWarning)

	s.Notify([]telemetry.Recommendation{rec("IFA-KV-001", telemetry.SeverityCritical, "chat")})
	waitFor(t, "the firing alert", func() bool { return len(r.got()) == 1 })

	// The condition clears, so the engine stops producing the finding.
	s.Notify(nil)
	waitFor(t, "the resolved alert", func() bool { return len(r.got()) == 2 })

	p := r.got()[1]
	if p.Event != EventResolved {
		t.Errorf("event: got %q, want %q", p.Event, EventResolved)
	}
	if p.ID != "IFA-KV-001:inference/chat" {
		t.Errorf("resolved alert must carry the same ID for correlation, got %q", p.ID)
	}
	if p.PreviousSeverity != "critical" {
		t.Errorf("previous_severity: got %q, want critical", p.PreviousSeverity)
	}
	if len(p.Evidence) != 0 {
		t.Error("a resolved alert must not carry evidence: there is no current measurement")
	}
	if s.Open() != 0 {
		t.Errorf("open alerts after resolution: got %d, want 0", s.Open())
	}

	// And it resolves exactly once.
	s.Notify(nil)
	time.Sleep(50 * time.Millisecond)
	if n := len(r.got()); n != 2 {
		t.Errorf("resolution repeated: got %d deliveries, want 2", n)
	}
}

func TestDistinctFindingsOnTheSameWorkloadAreTrackedSeparately(t *testing.T) {
	r := newReceiver(t)
	s := newTestSender(t, r, telemetry.SeverityWarning)

	s.Notify([]telemetry.Recommendation{
		rec("IFA-KV-001", telemetry.SeverityCritical, "chat"),
		rec("IFA-Q-001", telemetry.SeverityWarning, "chat"),
	})
	waitFor(t, "both alerts", func() bool { return len(r.got()) == 2 })

	if s.Open() != 2 {
		t.Fatalf("open: got %d, want 2", s.Open())
	}

	// One clears, the other holds. Exactly one resolution, no re-fire.
	s.Notify([]telemetry.Recommendation{rec("IFA-KV-001", telemetry.SeverityCritical, "chat")})
	waitFor(t, "the resolution", func() bool { return len(r.got()) == 3 })

	p := r.got()[2]
	if p.Event != EventResolved || p.Code != "IFA-Q-001" {
		t.Errorf("expected IFA-Q-001 resolved, got %s %s", p.Event, p.Code)
	}
}

// ── Retry ───────────────────────────────────────────────────────────────────

func TestRetryOnServerErrorsButNotOnClientErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantHits int64
	}{
		{"500 is retried once", http.StatusInternalServerError, 2},
		{"503 is retried once", http.StatusServiceUnavailable, 2},
		{"429 is retried once", http.StatusTooManyRequests, 2},
		{"400 is not retried", http.StatusBadRequest, 1},
		{"404 is not retried", http.StatusNotFound, 1},
		{"401 is not retried", http.StatusUnauthorized, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newReceiver(t)
			r.status.Store(int32(tc.status))
			s := newTestSender(t, r, telemetry.SeverityWarning)

			s.Notify([]telemetry.Recommendation{rec("IFA-TEST-001", telemetry.SeverityCritical, "svc")})

			waitFor(t, "the failure to be counted", func() bool {
				_, failed, _, _ := s.Stats()
				return failed == 1
			})
			// Allow a moment for any further attempt that should not happen.
			time.Sleep(30 * time.Millisecond)

			if got := r.hits.Load(); got != tc.wantHits {
				t.Errorf("request attempts: got %d, want %d", got, tc.wantHits)
			}
			if sent, _, _, _ := s.Stats(); sent != 0 {
				t.Errorf("sent: got %d, want 0", sent)
			}
		})
	}
}

func TestRetrySucceedsOnTheSecondAttempt(t *testing.T) {
	var hits atomic.Int64
	var mu sync.Mutex
	var received []Payload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var p Payload
		_ = json.NewDecoder(req.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Options{
		URL: srv.URL, MinSeverity: telemetry.SeverityWarning,
		Backoff: time.Millisecond, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Notify([]telemetry.Recommendation{rec("IFA-TEST-001", telemetry.SeverityCritical, "svc")})

	waitFor(t, "the retry to succeed", func() bool {
		sent, _, _, _ := s.Stats()
		return sent == 1
	})
	if _, failed, _, _ := s.Stats(); failed != 0 {
		t.Errorf("failed: got %d, want 0 — the retry succeeded", failed)
	}
	mu.Lock()
	n := len(received)
	mu.Unlock()
	if n != 1 {
		t.Errorf("payloads received: got %d, want 1", n)
	}
}

func TestTransportFailureIsCountedNotPanicked(t *testing.T) {
	// Nothing is listening on this port.
	s, err := New(Options{
		URL: "http://127.0.0.1:1", MinSeverity: telemetry.SeverityWarning,
		Timeout: 200 * time.Millisecond, Backoff: time.Millisecond,
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Notify([]telemetry.Recommendation{rec("IFA-TEST-001", telemetry.SeverityCritical, "svc")})

	waitFor(t, "the transport failure to be counted", func() bool {
		_, failed, _, _ := s.Stats()
		return failed == 1
	})
}

// ── Queue behaviour ─────────────────────────────────────────────────────────

func TestNotifyDoesNotBlockWhenTheQueueIsFull(t *testing.T) {
	// The sender goroutine is held on its first delivery, so the queue fills
	// and stays full. Notify must return regardless.
	release := make(chan struct{})
	var inFlight sync.Once

	s, err := New(Options{
		URL: "http://example.invalid/hook", MinSeverity: telemetry.SeverityInfo,
		QueueSize: 2, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.post = func(context.Context, Payload) error {
		inFlight.Do(func() { <-release })
		return nil
	}
	defer func() {
		close(release)
		s.Close()
	}()

	findings := make([]telemetry.Recommendation, 50)
	for i := range findings {
		findings[i] = rec("IFA-TEST-"+string(rune('A'+i%26))+string(rune('0'+i/26)),
			telemetry.SeverityCritical, "svc")
	}

	done := make(chan struct{})
	go func() {
		s.Notify(findings)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked when the queue was full; " +
			"the collector's evaluation cycle would stall behind a slow webhook")
	}

	_, _, dropped, _ := s.Stats()
	if dropped == 0 {
		t.Error("expected drops to be counted when the queue overflows")
	}

	// A dropped alert must not be recorded as open: the next evaluation has to
	// be able to try again.
	if open := s.Open(); open >= len(findings) {
		t.Errorf("open=%d of %d findings; dropped alerts must not be tracked as sent",
			open, len(findings))
	}
}

func TestCloseDrainsQueuedAlerts(t *testing.T) {
	var delivered atomic.Int64
	gate := make(chan struct{})

	s, err := New(Options{
		URL: "http://example.invalid/hook", MinSeverity: telemetry.SeverityInfo,
		QueueSize: 64, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Hold the sender until every alert is queued, so Close is guaranteed to be
	// draining a non-empty queue rather than racing an already-idle one.
	var once sync.Once
	s.post = func(context.Context, Payload) error {
		once.Do(func() { <-gate })
		delivered.Add(1)
		return nil
	}

	const n = 20
	findings := make([]telemetry.Recommendation, n)
	for i := 0; i < n; i++ {
		findings[i] = rec("IFA-D-"+string(rune('a'+i)), telemetry.SeverityCritical, "svc")
	}
	s.Notify(findings)

	close(gate)
	s.Close()

	if got := delivered.Load(); got != n {
		t.Errorf("delivered %d of %d queued alerts; Close must drain, "+
			"or a SIGTERM during a rolling update silently discards alerts", got, n)
	}
}

// ── Redaction ───────────────────────────────────────────────────────────────

func TestRedactURLHidesTheSecretAndKeepsTheHost(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// Modelled on a Slack incoming webhook, which is the reason path
			// redaction is necessary at all: the credential is the path, not the
			// userinfo a DSN would put it in.
			//
			// The host is deliberately a reserved .example domain (RFC 2606) and
			// the token deliberately hyphenated. Secret scanners anchor on the
			// real host and on a run of 24 alphanumerics; a fixture that looks
			// like a live credential blocks the push. What this test asserts is
			// that redaction covers path segments, which holds for any host, so
			// nothing is lost by not naming the real one.
			name: "webhook token in the path is redacted",
			in:   "https://hooks.example.com/services/T00000000/B00000000/not-a-real-token-000000",
			want: "https://hooks.example.com/(redacted)",
		},
		{
			name: "host only",
			in:   "https://receiver.example.com",
			want: "https://receiver.example.com",
		},
		{
			name: "bare root path is not a secret",
			in:   "http://receiver.internal:9000/",
			want: "http://receiver.internal:9000",
		},
		{
			name: "query string is dropped with the path",
			in:   "https://events.pagerduty.com/v2/enqueue?token=sekrit",
			want: "https://events.pagerduty.com/(redacted)",
		},
		{
			name: "unparseable input reveals nothing",
			in:   "::::not a url",
			want: "(set)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "not-a-real-token") || strings.Contains(got, "sekrit") {
				t.Errorf("RedactURL leaked the secret: %q", got)
			}
		})
	}
}

func TestTransportErrorMessageDoesNotContainTheURL(t *testing.T) {
	// Reserved domain and a hyphenated token, for the reasons given in
	// TestRedactURLHidesTheSecretAndKeepsTheHost.
	const secret = "T00000000/B00000000/not-a-real-token-000000"
	full := "https://hooks.example.com/services/" + secret

	// net/http embeds the request URL in its transport errors, which is how the
	// token would otherwise reach the log line reporting the failure.
	inner := &urlishError{msg: `Post "` + full + `": dial tcp: connection refused`}
	e := &transportError{err: inner, url: RedactURL(full)}

	msg := e.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("transport error leaked the webhook token: %s", msg)
	}
	if !strings.Contains(msg, "hooks.example.com") {
		t.Errorf("expected the host to survive redaction for debuggability: %s", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("expected the underlying cause to survive redaction: %s", msg)
	}
}

type urlishError struct{ msg string }

func (e *urlishError) Error() string { return e.msg }

// syncBuffer collects log output written by the sender goroutine while the test
// reads it.
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

// The two tests above check RedactURL and the error type in isolation, against a
// hand-written error string. This one exercises the path that actually matters: a
// real failure from net/http, wrapped by the real code, rendered by a real slog
// handler.
//
// net/http embeds the full request URL — path included — in its transport
// errors, so this is the route by which a webhook token genuinely reaches a log
// line. It is also the only way to know redactIn copes with the real message
// format rather than the one the test author imagined.
func TestARealDeliveryFailureNeverLogsTheToken(t *testing.T) {
	const token = "not-a-real-token-000000"

	var logs syncBuffer
	// Nothing listens on port 1, which produces a genuine *url.Error.
	s, err := New(Options{
		URL:         "http://127.0.0.1:1/services/T00000000/B00000000/" + token,
		MinSeverity: telemetry.SeverityWarning,
		Timeout:     200 * time.Millisecond,
		Backoff:     time.Millisecond,
		Logger: slog.New(slog.NewJSONHandler(&logs,
			&slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Notify([]telemetry.Recommendation{rec("IFA-KV-001", telemetry.SeverityCritical, "chat")})

	waitFor(t, "the delivery to fail and be logged", func() bool {
		_, failed, _, _ := s.Stats()
		return failed == 1 && logs.String() != ""
	})

	out := logs.String()
	if strings.Contains(out, token) {
		t.Fatalf("the webhook token reached the log output:\n%s", out)
	}
	// The host is kept deliberately: an operator has to be able to tell which
	// endpoint is failing.
	if !strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("expected the host in the failure log for debuggability:\n%s", out)
	}
	if !strings.Contains(out, "(redacted)") {
		t.Errorf("expected the redacted marker in the failure log:\n%s", out)
	}
}

// A dropped alert is logged with the finding's identity, which is not sensitive.
// But that log call sits beside ones that do handle the URL, so the queue-full
// path is worth pinning down too.
func TestQueueFullLogNeverContainsTheToken(t *testing.T) {
	const token = "not-a-real-token-000000"

	var logs syncBuffer
	release := make(chan struct{})
	var once sync.Once

	s, err := New(Options{
		URL:         "http://127.0.0.1:1/services/T00000000/B00000000/" + token,
		MinSeverity: telemetry.SeverityInfo,
		QueueSize:   1,
		Logger: slog.New(slog.NewJSONHandler(&logs,
			&slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.post = func(context.Context, Payload) error {
		once.Do(func() { <-release })
		return nil
	}
	defer func() {
		close(release)
		s.Close()
	}()

	findings := make([]telemetry.Recommendation, 20)
	for i := range findings {
		findings[i] = rec("IFA-D-"+string(rune('a'+i)), telemetry.SeverityCritical, "svc")
	}
	s.Notify(findings)

	waitFor(t, "a drop to be logged", func() bool {
		_, _, dropped, _ := s.Stats()
		return dropped > 0 && logs.String() != ""
	})

	if out := logs.String(); strings.Contains(out, token) {
		t.Fatalf("the webhook token reached the queue-full log output:\n%s", out)
	}
}

// ── Construction ────────────────────────────────────────────────────────────

func TestNewRejectsUnusableOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"no logger", Options{URL: "https://example.com/hook"}},
		{"no URL", Options{Logger: discardLogger()}},
		{"non-http scheme", Options{URL: "file:///etc/passwd", Logger: discardLogger()}},
		{"no host", Options{URL: "https://", Logger: discardLogger()}},
		{"unknown severity", Options{
			URL: "https://example.com/hook", MinSeverity: telemetry.Severity("urgent"),
			Logger: discardLogger(),
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.opts)
			if err == nil {
				s.Close()
				t.Fatal("expected an error at construction, got nil: " +
					"a misconfigured sender must fail at startup, not at the first finding")
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s, err := New(Options{URL: "https://example.com/hook", Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.minSeverity != telemetry.SeverityWarning {
		t.Errorf("default min severity: got %q, want warning", s.minSeverity)
	}
	if s.timeout != DefaultTimeout {
		t.Errorf("default timeout: got %s, want %s", s.timeout, DefaultTimeout)
	}
	if cap(s.queue) != DefaultQueueSize {
		t.Errorf("default queue size: got %d, want %d", cap(s.queue), DefaultQueueSize)
	}
}
