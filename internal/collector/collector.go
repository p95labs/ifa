// Package collector scrapes inference runtimes and writes normalised
// telemetry into the store.
//
// It owns everything that needs memory across scrapes — counter-to-rate
// conversion, counter-reset handling, staleness — and delegates format
// knowledge to runtime adapters. See internal/runtime for that boundary.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/p95labs/ifa/internal/metrics"
	"github.com/p95labs/ifa/internal/runtime"
	"github.com/p95labs/ifa/internal/runtime/dcgm"
	"github.com/p95labs/ifa/internal/runtime/triton"
	"github.com/p95labs/ifa/internal/runtime/vllm"
	"github.com/p95labs/ifa/internal/telemetry"
)

// Target is one inference workload to scrape.
type Target struct {
	WorkloadName string
	Namespace    string
	Runtime      telemetry.Runtime
	// ModelName filters metrics on runtimes that serve several models from one
	// process. Leave empty when the target serves exactly one.
	ModelName string
	// MetricsURL is the runtime's Prometheus endpoint.
	MetricsURL string
	// DCGMURL is an optional DCGM Exporter endpoint providing real GPU
	// utilisation and memory. Without it those fields stay unmeasured: no
	// inference runtime reports device utilisation itself, and substituting a
	// proxy such as KV-cache occupancy would make GPU rules fire on a number
	// that is not GPU utilisation.
	DCGMURL string
	// Deployment names the Kubernetes Deployment backing this workload, used
	// to join replica counts. Defaults to WorkloadName.
	Deployment string
}

// Key identifies the target's workload.
func (t Target) Key() string {
	if t.Namespace == "" {
		return t.WorkloadName
	}
	return t.Namespace + "/" + t.WorkloadName
}

// Validate checks a target is usable and its URLs are safe to fetch.
func (t Target) Validate() error {
	if t.WorkloadName == "" {
		return errors.New("workload_name is required")
	}
	if _, ok := adapters[t.Runtime]; !ok {
		return fmt.Errorf("unsupported runtime %q for workload %q (supported: %s)",
			t.Runtime, t.WorkloadName, supportedRuntimes())
	}
	if err := validateScrapeURL(t.MetricsURL); err != nil {
		return fmt.Errorf("metrics_url for workload %q: %w", t.WorkloadName, err)
	}
	if t.DCGMURL != "" {
		if err := validateScrapeURL(t.DCGMURL); err != nil {
			return fmt.Errorf("dcgm_url for workload %q: %w", t.WorkloadName, err)
		}
	}
	return nil
}

// validateScrapeURL rejects anything that is not a plain HTTP(S) URL with a
// host.
//
// Scrape targets come from configuration, which in a Helm deployment comes from
// a ConfigMap that more people can edit than can edit the Deployment. Restricting
// the scheme keeps a target from being pointed at file:// or at a scheme whose
// handler does something other than a GET.
func validateScrapeURL(raw string) error {
	if raw == "" {
		return errors.New("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

// adapters is the registry of supported runtimes. Adding a runtime means
// implementing runtime.Adapter and adding one line here.
var adapters = map[telemetry.Runtime]runtime.Adapter{
	telemetry.RuntimeVLLM:   vllm.New(),
	telemetry.RuntimeTriton: triton.New(),
}

func supportedRuntimes() string {
	out := ""
	for rt := range adapters {
		if out != "" {
			out += ", "
		}
		out += string(rt)
	}
	return out
}

// WorkloadLookup supplies Kubernetes replica counts for a workload. The
// Kubernetes watcher implements it; it is an interface so the collector does
// not depend on client-go and can be tested without it.
type WorkloadLookup interface {
	Replicas(namespace, name string) (desired, ready, max int32, ok bool)
}

// Options configures a Collector.
type Options struct {
	Interval time.Duration
	// Timeout bounds a single scrape, including reading the body.
	Timeout time.Duration
	// MaxBodyBytes caps how much of a response is read. A target that streams
	// an unbounded body — a misconfigured URL pointing at a log endpoint, or a
	// hostile one — would otherwise be able to exhaust the control plane's
	// memory.
	MaxBodyBytes int64
	// Concurrency bounds how many targets are scraped in parallel. Scraping
	// serially makes the effective interval depend on how many targets are
	// slow; scraping all at once makes a large fleet arrive as a burst.
	Concurrency int
	ClusterName string
	Logger      *slog.Logger
	Metrics     *metrics.Registry
	Workloads   WorkloadLookup
	// OnFirstCycle is called once, after the first scrape cycle completes.
	// The API uses it to flip readiness: before that point the store is empty,
	// and serving an empty fleet as if it were the whole fleet is misleading.
	OnFirstCycle func()
	// OnCycle is called after every completed scrape cycle, including the first.
	// When alerting is enabled, main registers a callback that calls Analyze
	// and Notify so that alerts fire on a predictable schedule rather than on
	// API request traffic.
	OnCycle func()
	// Clock overrides the collector's source of time. It exists so that tests
	// can produce exact rates and exact scrape spacing instead of depending on
	// how long an HTTP round trip happened to take. Leave nil in production.
	Clock func() time.Time
}

// Defaults applied when an option is left zero.
const (
	DefaultInterval     = 15 * time.Second
	DefaultTimeout      = 10 * time.Second
	DefaultMaxBodyBytes = 8 << 20 // 8 MiB; a vLLM payload is a few tens of KiB
	DefaultConcurrency  = 8
)

// Collector scrapes a fixed set of targets on an interval.
type Collector struct {
	targets []Target
	store   *telemetry.Store
	client  *http.Client
	opts    Options
	rates   *rateTracker
	now     func() time.Time
	// warned tracks conditions already reported for a target, so a persistent
	// fault produces one warning and a recovery message rather than one line
	// per scrape forever. The counters in /metrics carry the volume.
	warned sync.Map
}

// warnOnce logs at warn level the first time a condition appears for a target
// and at debug level while it persists.
func (c *Collector) warnOnce(key, msg string, args ...any) {
	if _, seen := c.warned.LoadOrStore(key, true); seen {
		c.opts.Logger.Debug(msg, args...)
		return
	}
	c.opts.Logger.Warn(msg, args...)
}

// clearWarn forgets a condition, so its recovery is reported and a later
// recurrence warns again.
func (c *Collector) clearWarn(key, recoveredMsg string, args ...any) {
	if _, seen := c.warned.LoadAndDelete(key); seen {
		c.opts.Logger.Info(recoveredMsg, args...)
	}
}

// New validates the targets and returns a Collector.
func New(targets []Target, store *telemetry.Store, opts Options) (*Collector, error) {
	if store == nil {
		return nil, errors.New("collector: store is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("collector: logger is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Timeout >= opts.Interval {
		// Otherwise a stuck target keeps a scrape in flight past the point
		// where the next one starts, and the two overlap indefinitely.
		return nil, fmt.Errorf(
			"collector: scrape timeout (%s) must be shorter than the interval (%s)",
			opts.Timeout, opts.Interval)
	}

	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("collector: invalid target: %w", err)
		}
		if seen[t.Key()] {
			return nil, fmt.Errorf(
				"collector: duplicate target %q; two targets with the same namespace and "+
					"workload_name would overwrite each other's telemetry", t.Key())
		}
		seen[t.Key()] = true
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	return &Collector{
		targets: targets,
		store:   store,
		opts:    opts,
		rates:   newRateTracker(),
		now:     clock,
		client: &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: opts.Timeout,
				// Scrape targets are addressed directly inside the cluster.
				// Following a redirect would let a target send the collector
				// somewhere it was not configured to go.
				DisableCompression: false,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Targets returns the configured targets.
func (c *Collector) Targets() []Target { return c.targets }

// Run scrapes on the configured interval until ctx is cancelled. It blocks.
func (c *Collector) Run(ctx context.Context) {
	// Scrape immediately so the API has data before the first tick, then on
	// the interval.
	c.ScrapeAll(ctx)
	if c.opts.OnFirstCycle != nil {
		c.opts.OnFirstCycle()
	}
	if c.opts.OnCycle != nil {
		c.opts.OnCycle()
	}

	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	prune := time.NewTicker(c.opts.Interval * 10)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			c.opts.Logger.Info("collector stopped")
			return
		case <-ticker.C:
			c.ScrapeAll(ctx)
			if c.opts.OnCycle != nil {
				c.opts.OnCycle()
			}
		case <-prune.C:
			if n := c.store.Prune(); n > 0 {
				c.opts.Logger.Info("pruned workloads with no recent telemetry", "count", n)
			}
		}
	}
}

// ScrapeAll scrapes every target once, with bounded parallelism.
func (c *Collector) ScrapeAll(ctx context.Context) {
	sem := make(chan struct{}, c.opts.Concurrency)
	var wg sync.WaitGroup

	for _, t := range c.targets {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			defer func() { <-sem }()
			c.scrapeOne(ctx, t)
		}(t)
	}
	wg.Wait()
}

func (c *Collector) scrapeOne(ctx context.Context, t Target) {
	start := c.now()
	snap, err := c.Scrape(ctx, t)
	elapsed := c.now().Sub(start)

	if err != nil {
		// A failed scrape must not write a snapshot. Writing a zeroed one
		// would let rules diagnose a workload the collector cannot see, and
		// would keep the staleness rule from ever firing.
		c.warnOnce("scrape:"+t.Key(), "scrape failed",
			"workload", t.Key(), "url", t.MetricsURL, "err", err)
		if c.opts.Metrics != nil {
			c.opts.Metrics.RecordScrapeError(t.Key(), string(t.Runtime))
		}
		return
	}
	c.clearWarn("scrape:"+t.Key(), "scrape recovered", "workload", t.Key())

	snap.ScrapeDurationMs = telemetry.Observed(float64(elapsed.Milliseconds()))
	c.store.Add(*snap)
	if c.opts.Metrics != nil {
		c.opts.Metrics.RecordScrape(t.Key(), string(t.Runtime), elapsed, len(snap.MetricsMissing))
		c.opts.Metrics.SetStoreSize(c.store.Size())
	}
}

// Scrape fetches and normalises one target. It is exported so that `ifa check`
// and tests can run a single scrape without starting the loop.
func (c *Collector) Scrape(ctx context.Context, t Target) (*telemetry.Snapshot, error) {
	adapter, ok := adapters[t.Runtime]
	if !ok {
		return nil, fmt.Errorf("unsupported runtime %q", t.Runtime)
	}

	body, err := c.fetch(ctx, t.MetricsURL)
	if err != nil {
		return nil, err
	}

	reading, err := adapter.Parse(string(body), t.ModelName)
	if err != nil {
		return nil, fmt.Errorf("parsing %s metrics for %s: %w", t.Runtime, t.Key(), err)
	}
	if reading.UnparseableLines > 0 {
		c.warnOnce("parse:"+t.Key(), "exposition lines could not be parsed",
			"workload", t.Key(), "lines", reading.UnparseableLines,
			"hint", "the runtime's metrics format may have changed")
	}

	now := c.now().UTC()
	snap := reading.Snapshot
	snap.Timestamp = now
	snap.ClusterName = c.opts.ClusterName
	snap.Namespace = t.Namespace
	snap.WorkloadName = t.WorkloadName
	snap.Runtime = t.Runtime
	if snap.ModelName == "" {
		snap.ModelName = t.ModelName
	}
	snap.MetricsMissing = reading.Missing

	c.applyRates(&snap, t, reading.Counters, now)
	c.applyGPU(ctx, &snap, t)
	c.applyWorkloadContext(&snap, t)

	return &snap, nil
}

// applyRates converts the adapter's raw counters into per-second rates and the
// ratios derived from them.
func (c *Collector) applyRates(snap *telemetry.Snapshot, t Target, counters map[runtime.Counter]float64, now time.Time) {
	rate := func(name runtime.Counter) (float64, bool) {
		v, present := counters[name]
		if !present {
			return 0, false
		}
		return c.rates.rate(t.Key()+"|"+string(name), v, now)
	}

	if v, ok := rate(runtime.CounterGenerationTokens); ok {
		snap.TokensPerSecond = telemetry.Observed(v)
	}
	if v, ok := rate(runtime.CounterPromptTokens); ok {
		snap.PromptTokensPerSec = telemetry.Observed(v)
	}
	if v, ok := rate(runtime.CounterPreemptions); ok {
		snap.PreemptionsPerSec = telemetry.Observed(v)
	}

	finished, finishedOK := rate(runtime.CounterRequestsFinished)
	if finishedOK {
		snap.RequestRatePerSec = telemetry.Observed(finished)
	}

	// Ratios are computed from rates over the same interval rather than from
	// lifetime totals, so a workload that was unhealthy an hour ago does not
	// keep reporting a bad ratio after it recovers.
	if failed, ok := rate(runtime.CounterRequestsFailed); ok && finishedOK {
		if total := finished + failed; total > 0 {
			snap.ErrorRatePct = telemetry.Observed(failed / total * 100)
		}
	}
	if aborted, ok := rate(runtime.CounterRequestsAborted); ok && finishedOK && finished > 0 {
		snap.AbortRatePct = telemetry.Observed(aborted / finished * 100)
	}

	queries, qOK := rate(runtime.CounterPrefixCacheQueries)
	hits, hOK := rate(runtime.CounterPrefixCacheHits)
	if qOK && hOK && queries > 0 {
		snap.PrefixCacheHitRatePct = telemetry.Observed(hits / queries * 100)
	}
}

// applyGPU overlays real device metrics from DCGM Exporter when configured.
//
// Aggregation takes the maximum across devices: a single saturated GPU in a
// multi-GPU pod is the thing worth surfacing, and averaging would hide it.
func (c *Collector) applyGPU(ctx context.Context, snap *telemetry.Snapshot, t Target) {
	if t.DCGMURL == "" {
		return
	}
	body, err := c.fetch(ctx, t.DCGMURL)
	if err != nil {
		c.warnOnce("dcgm:"+t.Key(), "DCGM scrape failed; GPU metrics will not be measured",
			"workload", t.Key(), "url", t.DCGMURL, "err", err)
		return
	}
	gpus, err := dcgm.Parse(string(body))
	if err != nil || len(gpus) == 0 {
		c.warnOnce("dcgm:"+t.Key(), "DCGM payload contained no recognised GPU metrics; "+
			"GPU rules will not run for this workload",
			"workload", t.Key(), "url", t.DCGMURL, "err", err)
		return
	}
	c.clearWarn("dcgm:"+t.Key(), "DCGM metrics recovered", "workload", t.Key())

	var maxUtil, maxMem float64
	var haveUtil, haveMem bool
	for _, g := range gpus {
		if g.UtilizationPct.OK && (!haveUtil || g.UtilizationPct.Value > maxUtil) {
			maxUtil, haveUtil = g.UtilizationPct.Value, true
		}
		if g.MemoryUsedPct.OK && (!haveMem || g.MemoryUsedPct.Value > maxMem) {
			maxMem, haveMem = g.MemoryUsedPct.Value, true
		}
	}
	snap.GPUUtilizationPct = telemetry.ObservedIf(maxUtil, haveUtil)
	snap.GPUMemoryUsedPct = telemetry.ObservedIf(maxMem, haveMem)
}

// applyWorkloadContext joins Kubernetes replica counts onto the snapshot. The
// scaling rules need them and no inference runtime knows about them.
func (c *Collector) applyWorkloadContext(snap *telemetry.Snapshot, t Target) {
	if c.opts.Workloads == nil {
		return
	}
	name := t.Deployment
	if name == "" {
		name = t.WorkloadName
	}
	desired, ready, max, ok := c.opts.Workloads.Replicas(t.Namespace, name)
	if !ok {
		return
	}
	snap.Replicas = telemetry.Observed(float64(desired))
	snap.ReadyReplicas = telemetry.Observed(float64(ready))
	if max > 0 {
		snap.MaxReplicas = telemetry.Observed(float64(max))
	}
}

// fetch performs a bounded GET.
func (c *Collector) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=0.8,*/*;q=0.1")
	req.Header.Set("User-Agent", "ifa")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", rawURL, err)
	}
	defer func() {
		// Drain up to a bounded amount so the connection can be reused, then
		// close. Closing without draining forces a new TCP connection on every
		// scrape; draining without a bound reintroduces the unbounded read.
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, rawURL)
	}

	limited := io.LimitReader(resp.Body, c.opts.MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", rawURL, err)
	}
	if int64(len(body)) > c.opts.MaxBodyBytes {
		return nil, fmt.Errorf(
			"response from %s exceeds the %d byte limit; refusing to buffer it",
			rawURL, c.opts.MaxBodyBytes)
	}
	return body, nil
}
