// Command control-plane runs the IFA control plane:
// it scrapes inference runtimes, evaluates diagnostic rules, and serves the
// results over a read-only HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/p95labs/ifa/internal/alerting"
	"github.com/p95labs/ifa/internal/api"
	"github.com/p95labs/ifa/internal/collector"
	"github.com/p95labs/ifa/internal/config"
	"github.com/p95labs/ifa/internal/demo"
	"github.com/p95labs/ifa/internal/k8s"
	"github.com/p95labs/ifa/internal/metrics"
	"github.com/p95labs/ifa/internal/recommender"
	"github.com/p95labs/ifa/internal/storage/timescale"
	"github.com/p95labs/ifa/internal/telemetry"
)

// version is overridden at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "control-plane: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "config.yaml", "path to the configuration file")
		demoMode    = flag.Bool("demo", false, "run against simulated inference workloads instead of real scrape targets")
		demoSeed    = flag.Int64("demo-seed", 1, "random seed for --demo, so runs are reproducible")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("ifa %s (%s)\n", version, commit())
		return nil
	}

	// An explicitly supplied config path must exist; the default one need not,
	// because the defaults alone are a valid (inert) configuration.
	explicit := isFlagSet("config")
	cfg, err := config.Load(*configPath, explicit)
	if err != nil {
		return err
	}

	log := newLogger(cfg.Logging)
	build := metrics.BuildInfo{Version: version, Commit: commit(), GoVer: goVersion()}
	reg := metrics.New(build)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var demoServer *demo.Server
	if *demoMode {
		demoServer, err = demo.NewServer(*demoSeed)
		if err != nil {
			return err
		}
		defer func() { _ = demoServer.Close() }()
		applyDemoConfig(cfg, demoServer)
		log.Info("demo mode: scraping simulated inference workloads",
			"endpoint", demoServer.BaseURL(), "workloads", len(demoServer.Scenarios()))
	}

	log.Info("starting ifa", "version", version, "config", cfg.String())

	// ── Telemetry store, with an optional durable sink ───────────────────────
	storeOpts := []telemetry.StoreOption{
		telemetry.WithRetention(cfg.Telemetry.RetentionSamples, cfg.Telemetry.RetentionPeriod.D()),
	}
	if cfg.Database.Enabled {
		sink, err := timescale.Open(ctx, cfg.Database.DSN, timescale.Options{
			QueueSize: cfg.Database.QueueSize,
			Logger:    log,
		})
		if err != nil {
			// Failing to start is the right response: an operator who enabled
			// the history backend should find out now, not discover an empty
			// table in a month.
			return fmt.Errorf("database enabled but unavailable: %w", err)
		}
		defer sink.Close()
		reg.SetDatabaseStats(sink.Stats)
		storeOpts = append(storeOpts, telemetry.WithSink(sink))
		log.Info("durable telemetry history enabled", "dsn", config.RedactDSN(cfg.Database.DSN))
	}
	store := telemetry.NewStore(storeOpts...)

	// ── Kubernetes discovery ─────────────────────────────────────────────────
	var (
		watcher   *k8s.Watcher
		workloads collector.WorkloadLookup
		apiSource api.WorkloadSource
	)
	if cfg.Kubernetes.Enabled {
		watcher, err = k8s.NewWatcher(k8s.Options{
			Namespace:     cfg.Kubernetes.Namespace,
			LabelSelector: cfg.Kubernetes.LabelSelector,
			ResyncPeriod:  cfg.Kubernetes.ResyncPeriod.D(),
			Logger:        log,
		})
		if err != nil {
			// Discovery is an enhancement, not a prerequisite: scraping and
			// diagnosis work without it, and refusing to start would turn a
			// missing kubeconfig into a total outage.
			log.Warn("kubernetes discovery unavailable; scaling rules will be skipped", "err", err)
		} else if err := watcher.Start(ctx); err != nil {
			log.Warn("kubernetes informers did not sync; scaling rules will be skipped", "err", err)
			watcher = nil
		} else {
			workloads = watcher
			apiSource = workloadAdapter{watcher}
		}
	}
	if demoServer != nil {
		workloads = demoServer
	}

	engine := recommender.NewEngine(cfg.Thresholds())

	// ── Alerting sender ───────────────────────────────────────────────────────
	var alertSender *alerting.Sender
	if cfg.Alerting.Enabled {
		aopts := cfg.AlertingOptions()
		aopts.Logger = log
		alertSender, err = alerting.New(aopts)
		if err != nil {
			return fmt.Errorf("alerting: %w", err)
		}
		reg.SetAlertStats(func() metrics.AlertStats {
			sent, failed, dropped, suppressed := alertSender.Stats()
			return metrics.AlertStats{
				Sent:       sent,
				Failed:     failed,
				Dropped:    dropped,
				Suppressed: suppressed,
				Open:       alertSender.Open(),
			}
		})
		log.Info("webhook alerting enabled",
			"webhook", alerting.RedactURL(cfg.Alerting.WebhookURL),
			"min_severity", cfg.Alerting.MinSeverity)
	}

	// ── Collector ────────────────────────────────────────────────────────────
	targets, err := cfg.Targets()
	if err != nil {
		return err
	}
	// Readiness flips when the collector finishes its first cycle. The channel
	// exists because the API server is constructed after the collector.
	collectorReady := make(chan struct{})
	var readyOnce sync.Once
	coll, err := collector.New(targets, store, collector.Options{
		Interval:     cfg.Collector.Interval.D(),
		Timeout:      cfg.Collector.Timeout.D(),
		Concurrency:  cfg.Collector.Concurrency,
		MaxBodyBytes: cfg.Collector.MaxBodyBytes,
		ClusterName:  cfg.ClusterName,
		Logger:       log,
		Metrics:      reg,
		Workloads:    workloads,
		OnFirstCycle: func() { readyOnce.Do(func() { close(collectorReady) }) },
		OnCycle: func() {
			if alertSender == nil {
				return
			}
			recs := engine.Analyze(store.Latest(), store.History)
			alertSender.Notify(recs)
		},
	})
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		log.Warn("no scrape targets configured; the API will report no workloads",
			"hint", "add collector.targets to the config file, or run with --demo")
	}

	// ── HTTP ─────────────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	srv, err := api.New(api.Options{
		Store:     store,
		Workloads: apiSource,
		Engine:    engine,
		Metrics:   reg,
		Logger:    log,
		Info: api.HealthInfo{
			Version:        version,
			Commit:         commit(),
			CollectorMode:  collectorMode(*demoMode, len(targets)),
			TargetCount:    len(targets),
			KubernetesMode: enabledLabel(watcher != nil),
			DatabaseMode:   enabledLabel(cfg.Database.Enabled),
		},
	}, mux)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Address,
		Handler:           requestLogger(log, mux),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       cfg.Server.IdleTimeout.D(),
		MaxHeaderBytes:    1 << 16,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP API listening", "address", cfg.Server.Address)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		coll.Run(ctx)
	}()
	go func() {
		select {
		case <-collectorReady:
			srv.SetReady()
		case <-ctx.Done():
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("HTTP server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.D())
	defer cancelShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP server did not shut down cleanly", "err", err)
	}
	<-collectorDone
	if alertSender != nil {
		alertSender.Close()
	}
	log.Info("stopped")
	return nil
}

// applyDemoConfig rewires the configuration to scrape the simulated fleet.
//
// The thresholds are the shipped defaults except for the sustain window, which
// is shortened so that findings appear within seconds rather than after the
// production default of 45 seconds. Nothing else is relaxed: the rules that
// fire in the demo are the rules that ship.
func applyDemoConfig(cfg *config.Config, srv *demo.Server) {
	cfg.ClusterName = "demo"
	cfg.Collector.Interval = config.Duration(time.Second)
	cfg.Collector.Timeout = config.Duration(800 * time.Millisecond)
	cfg.Recommender.Thresholds.SustainFor = config.Duration(5 * time.Second)
	cfg.Telemetry.RetentionPeriod = config.Duration(5 * time.Minute)
	cfg.Kubernetes.Enabled = false
	cfg.Database.Enabled = false

	cfg.Collector.Targets = nil
	for _, sc := range srv.Scenarios() {
		cfg.Collector.Targets = append(cfg.Collector.Targets, config.TargetConfig{
			Name:       sc.Name,
			Namespace:  "inference",
			Runtime:    "vllm",
			ModelName:  sc.Model,
			MetricsURL: srv.MetricsURL(sc.Name),
			DCGMURL:    srv.DCGMURL(sc.Name),
		})
	}
}

// requestLogger records API requests at debug level and slow ones at info.
// Logging every request at info would drown the operational messages that
// matter in a service that a dashboard may poll every few seconds.
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		took := time.Since(start)

		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", took}
		switch {
		case rec.status >= 500:
			log.Error("request failed", attrs...)
		case took > time.Second:
			log.Info("slow request", attrs...)
		default:
			log.Debug("request", attrs...)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// workloadAdapter converts discovered Kubernetes workloads into the API's wire
// type, so the API package does not depend on client-go.
type workloadAdapter struct{ w *k8s.Watcher }

func (a workloadAdapter) All() []api.Workload {
	src := a.w.All()
	out := make([]api.Workload, 0, len(src))
	for _, w := range src {
		out = append(out, api.Workload{
			Name:          w.Name,
			Namespace:     w.Namespace,
			Runtime:       w.Runtime,
			ModelName:     w.ModelName,
			Replicas:      w.Replicas,
			ReadyReplicas: w.ReadyReplicas,
			RestartCount:  w.RestartCount,
			GPURequest:    w.GPURequest,
			Labels:        w.Labels,
			LastUpdated:   w.LastUpdated,
		})
	}
	return out
}

func newLogger(cfg config.LoggingConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func collectorMode(demoMode bool, targets int) string {
	switch {
	case demoMode:
		return "demo"
	case targets == 0:
		return "idle"
	default:
		return "prometheus"
	}
}

func enabledLabel(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// commit and goVersion read the VCS stamps the Go toolchain embeds, so a binary
// can identify itself without a build script having to pass them in.
func commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return "unknown"
}

func goVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}
