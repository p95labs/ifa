package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

// Defaults applied when an option is left zero.
const (
	DefaultTimeout   = 5 * time.Second
	DefaultQueueSize = 256
	DefaultBackoff   = 500 * time.Millisecond
	// deliveryAttempts is the total number of tries per alert, not the number
	// of retries. Two: one immediate, one after a backoff. A third attempt buys
	// very little — an endpoint that has failed twice over a second is down
	// rather than briefly unlucky — and every attempt is time the single sender
	// goroutine is not draining the queue behind it.
	deliveryAttempts = 2
)

// Options configures a Sender.
type Options struct {
	// URL is the webhook endpoint. It commonly embeds a secret in its path,
	// which is why it is never logged directly. See RedactURL.
	URL string
	// MinSeverity is the least severe finding worth sending. A finding below it
	// is neither sent nor tracked, so it cannot later be "resolved".
	MinSeverity telemetry.Severity
	// Cluster is copied into every payload so a subscriber receiving alerts
	// from several clusters can tell them apart.
	Cluster string
	// Timeout bounds one delivery attempt.
	Timeout time.Duration
	// QueueSize bounds how many alerts may be waiting to be sent.
	QueueSize int
	// Logger receives delivery failures and drops. Required.
	Logger *slog.Logger

	// Now and Backoff exist so tests are deterministic and fast. Leave them
	// zero in production.
	Now     func() time.Time
	Backoff time.Duration
}

// Sender delivers findings to a webhook endpoint, at most once per transition.
type Sender struct {
	url         string
	minSeverity telemetry.Severity
	cluster     string
	timeout     time.Duration
	backoff     time.Duration
	client      *http.Client
	log         *slog.Logger
	now         func() time.Time

	queue chan Payload

	sent       atomic.Int64
	failed     atomic.Int64
	dropped    atomic.Int64
	suppressed atomic.Int64

	// mu guards open. Notify holds it while it both decides what to send and
	// enqueues: the queue write is non-blocking, so the critical section is
	// bounded, and doing both under one lock is what keeps the recorded state
	// and the queue from disagreeing. An alert that could not be enqueued is
	// deliberately not recorded as open, so the next evaluation tries again.
	mu   sync.Mutex
	open map[string]openAlert

	wg       sync.WaitGroup
	stopOnce sync.Once
	stop     chan struct{}

	// post performs one delivery attempt. It is a field so that the queueing
	// and shutdown behaviour — the parts that can lose an alert — can be tested
	// without timing against a real server.
	post func(context.Context, Payload) error
}

// New validates the options and starts the sender goroutine. The caller must
// call Close.
func New(opts Options) (*Sender, error) {
	if opts.Logger == nil {
		return nil, errors.New("alerting: logger is required")
	}
	if err := ValidateURL(opts.URL); err != nil {
		return nil, err
	}
	if opts.MinSeverity == "" {
		opts.MinSeverity = telemetry.SeverityWarning
	}
	if !validSeverity(opts.MinSeverity) {
		return nil, fmt.Errorf("alerting: min_severity must be info, warning or critical, got %q", opts.MinSeverity)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.Backoff <= 0 {
		opts.Backoff = DefaultBackoff
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	s := &Sender{
		url:         opts.URL,
		minSeverity: opts.MinSeverity,
		cluster:     opts.Cluster,
		timeout:     opts.Timeout,
		backoff:     opts.Backoff,
		log:         opts.Logger,
		now:         opts.Now,
		queue:       make(chan Payload, opts.QueueSize),
		open:        make(map[string]openAlert),
		stop:        make(chan struct{}),
		client: &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
			// A webhook URL is configuration, not a redirect to follow: sending
			// the payload — which carries the workload names of the fleet —
			// somewhere other than the configured host is not something a
			// remote server should be able to ask for.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	s.post = s.postOnce
	s.wg.Add(1)
	go s.run()
	return s, nil
}

// ValidateURL checks that a webhook URL is one this package will post to.
// Exported so the config loader can reject a bad value at startup rather than
// at the first finding, which may be hours later.
func ValidateURL(raw string) error {
	if raw == "" {
		return errors.New("alerting: webhook_url must not be empty when alerting is enabled")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("alerting: webhook_url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("alerting: webhook_url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("alerting: webhook_url must include a host")
	}
	return nil
}

// RedactURL renders a webhook URL for logs with any secret removed.
//
// Slack, Teams and PagerDuty all put the credential in the path, so redacting
// only the userinfo — which is what a DSN needs — would leak the token on the
// first startup log line. The host is kept: knowing an alert went to
// hooks.slack.com rather than to an internal receiver is useful, and is not a
// secret.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(set)"
	}
	out := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" {
		out += "/(redacted)"
	}
	return out
}

func validSeverity(s telemetry.Severity) bool {
	switch s {
	case telemetry.SeverityInfo, telemetry.SeverityWarning, telemetry.SeverityCritical:
		return true
	}
	return false
}

// Notify is handed the complete set of findings from one evaluation and enqueues
// only the transitions: findings that are new, findings that have become more
// severe, and findings that were open and are now absent.
//
// It must be called with the full current set, not with a subset. A filtered
// call would make every absent finding look resolved.
func (s *Sender) Notify(recs []telemetry.Recommendation) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	present := make(map[string]bool, len(recs))
	for _, rec := range recs {
		if rec.Severity.Rank() > s.minSeverity.Rank() {
			continue
		}
		present[rec.ID] = true

		prev, known := s.open[rec.ID]
		switch {
		case !known:
			if s.enqueue(firingPayload(rec, EventFiring, "", s.cluster, now)) {
				s.open[rec.ID] = newOpenAlert(rec)
			}
		case rec.Severity.Rank() < prev.severity.Rank():
			// Escalation: warning became critical. Worth waking someone a
			// second time.
			if s.enqueue(firingPayload(rec, EventEscalated, prev.severity, s.cluster, now)) {
				s.open[rec.ID] = newOpenAlert(rec)
			}
		default:
			// Unchanged, or de-escalated. A finding that drops from critical to
			// warning is still the same open problem, and a message saying so
			// is noise on the way to the resolution.
			s.suppressed.Add(1)
		}
	}

	for id, a := range s.open {
		if present[id] {
			continue
		}
		// Only forget the alert once the resolution is actually queued, so a
		// full queue means the resolution is retried on the next evaluation
		// rather than lost.
		if s.enqueue(resolvedPayload(a, s.cluster, now)) {
			delete(s.open, id)
		}
	}
}

// enqueue attempts a non-blocking queue write, reporting whether it succeeded.
func (s *Sender) enqueue(p Payload) bool {
	select {
	case s.queue <- p:
		return true
	default:
		if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
			s.log.Warn("alert dropped: webhook queue is full",
				"dropped_total", n, "id", p.ID, "event", p.Event,
				"hint", "the endpoint is slower than findings are produced; "+
					"raise alerting.queue_size or check the receiver")
		}
		return false
	}
}

// Stats reports delivery outcomes for the self-metrics endpoint.
func (s *Sender) Stats() (sent, failed, dropped, suppressed int64) {
	return s.sent.Load(), s.failed.Load(), s.dropped.Load(), s.suppressed.Load()
}

// Open reports how many findings are currently tracked as open. It exists for
// the self-metrics gauge and for tests.
func (s *Sender) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// Close drains the queue and stops the sender.
//
// Draining rather than dropping is the point: SIGTERM during a rolling update
// is routine, and an alert that was queued a millisecond before the signal is
// exactly as important as one queued a millisecond after the previous cycle.
func (s *Sender) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
	})
}

func (s *Sender) run() {
	defer s.wg.Done()
	for {
		select {
		case p := <-s.queue:
			s.deliver(p)
		case <-s.stop:
			for {
				select {
				case p := <-s.queue:
					s.deliver(p)
				default:
					return
				}
			}
		}
	}
}

// deliver sends one payload, retrying once when the failure looks transient.
func (s *Sender) deliver(p Payload) {
	var lastErr error
	for attempt := 1; attempt <= deliveryAttempts; attempt++ {
		if attempt > 1 {
			// Honour shutdown while backing off, so Close is not held up for
			// the backoff duration per queued alert.
			select {
			case <-time.After(s.backoff):
			case <-s.stop:
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		err := s.post(ctx, p)
		cancel()

		if err == nil {
			s.sent.Add(1)
			return
		}
		lastErr = err
		if !retryable(err) {
			// A 400 will be a 400 again. Retrying a rejected payload wastes the
			// sender's only goroutine on a request that cannot succeed.
			break
		}
	}

	if n := s.failed.Add(1); n == 1 || n%100 == 0 {
		s.log.Error("alert delivery failed",
			"url", RedactURL(s.url), "id", p.ID, "event", p.Event,
			"failed_total", n, "err", lastErr)
	}
}

func (s *Sender) postOnce(ctx context.Context, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding alert %s: %w", p.ID, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building alert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ifa")

	resp, err := s.client.Do(req)
	if err != nil {
		// A transport error carries the URL, and therefore the token, in its
		// message. Replacing it is the only way to keep the secret out of the
		// log line that reports the failure.
		return &transportError{err: err, url: RedactURL(s.url)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{code: resp.StatusCode}
	}
	return nil
}

// statusError is a non-2xx response.
type statusError struct{ code int }

func (e *statusError) Error() string {
	return fmt.Sprintf("webhook returned status %d", e.code)
}

// retryable reports whether another attempt could plausibly succeed.
//
// 5xx is the server saying it failed, 429 is it asking to be tried later, and a
// transport error never reached a server at all. Every 4xx other than 429 is the
// server rejecting this specific request, which a second identical request will
// not fix.
func (e *statusError) retryable() bool {
	return e.code >= 500 || e.code == http.StatusTooManyRequests
}

// transportError wraps a connection-level failure with the URL redacted.
type transportError struct {
	err error
	url string
}

func (e *transportError) Error() string {
	return fmt.Sprintf("posting alert to %s: %s", e.url, redactIn(e.err.Error(), e.url))
}

func (e *transportError) retryable() bool { return true }

// redactIn replaces any full URL in a message with the already-redacted form,
// so the token cannot arrive by way of the wrapped error's own text. net/url and
// net/http both include the request URL in their error strings.
func redactIn(msg, redacted string) string {
	for _, scheme := range []string{"https://", "http://"} {
		i := strings.Index(msg, scheme)
		if i < 0 {
			continue
		}
		end := strings.IndexAny(msg[i:], " \"")
		if end < 0 {
			return msg[:i] + redacted
		}
		return msg[:i] + redacted + msg[i+end:]
	}
	return msg
}

type retryableError interface{ retryable() bool }

func retryable(err error) bool {
	var r retryableError
	if errors.As(err, &r) {
		return r.retryable()
	}
	// An error this package did not classify — a JSON encoding failure, for
	// instance — is not transient.
	return false
}
