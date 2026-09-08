// Package timescale provides an optional durable sink for telemetry snapshots,
// backed by PostgreSQL with the TimescaleDB extension.
//
// It is optional on purpose. The rule engine only ever reads the in-memory
// window, so a database outage degrades IFA to "no history" rather than "no
// diagnostics", and a user evaluating the project does not have to stand up a
// database to see it work. See docs/adr/0003-optional-timescaledb.md.
package timescale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/p95labs/ifa/internal/telemetry"
)

// Sink batches snapshots onto a bounded queue and writes them from a single
// goroutine.
//
// The original implementation started a goroutine per snapshot. With a
// five-second scrape interval and a database that has gone away, that grows
// without limit until the process dies — the observability tool taking down
// its own pod. A bounded queue with an explicit drop counter fails visibly
// instead: telemetry loss is recorded, and the collector never blocks.
type Sink struct {
	pool  *pgxpool.Pool
	queue chan telemetry.Snapshot
	log   *slog.Logger

	dropped atomic.Int64
	failed  atomic.Int64
	written atomic.Int64

	wg       sync.WaitGroup
	stopOnce sync.Once
	stop     chan struct{}

	// exec performs one insert. It is a field so that the queueing behaviour —
	// which is the part that can lose data — can be tested without a database.
	exec func(context.Context, telemetry.Snapshot) error
}

// Options configures a Sink.
type Options struct {
	// QueueSize bounds how many snapshots may be waiting to be written.
	// Defaults to 1024.
	QueueSize int
	// WriteTimeout bounds a single insert. Defaults to 5s.
	WriteTimeout time.Duration
	// Logger receives write failures. Required.
	Logger *slog.Logger
}

const (
	defaultQueueSize    = 1024
	defaultWriteTimeout = 5 * time.Second
)

// Open connects to the database, verifies the connection, and starts the
// writer goroutine. The caller must call Close.
func Open(ctx context.Context, dsn string, opts Options) (*Sink, error) {
	if opts.Logger == nil {
		return nil, errors.New("timescale: logger is required")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("timescale: creating pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("timescale: ping: %w", err)
	}

	s := newSink(opts)
	s.pool = pool
	s.exec = s.insertToPool
	s.start(opts.WriteTimeout)
	return s, nil
}

func newSink(opts Options) *Sink {
	return &Sink{
		queue: make(chan telemetry.Snapshot, opts.QueueSize),
		log:   opts.Logger,
		stop:  make(chan struct{}),
	}
}

func (s *Sink) start(timeout time.Duration) {
	s.wg.Add(1)
	go s.run(timeout)
}

// Write enqueues a snapshot. It never blocks: when the queue is full the
// snapshot is dropped and counted.
func (s *Sink) Write(snap telemetry.Snapshot) {
	select {
	case s.queue <- snap:
	default:
		if n := s.dropped.Add(1); n == 1 || n%1000 == 0 {
			s.log.Warn("telemetry dropped: database write queue is full",
				"dropped_total", n)
		}
	}
}

// Stats reports write outcomes for the self-metrics endpoint.
func (s *Sink) Stats() (written, failed, dropped int64) {
	return s.written.Load(), s.failed.Load(), s.dropped.Load()
}

// Close drains the queue, stops the writer and releases the pool.
func (s *Sink) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		if s.pool != nil {
			s.pool.Close()
		}
	})
}

func (s *Sink) run(timeout time.Duration) {
	defer s.wg.Done()
	for {
		select {
		case snap := <-s.queue:
			s.insert(snap, timeout)
		case <-s.stop:
			// Drain whatever is already queued, then exit. Anything still
			// arriving after Close is lost by design.
			for {
				select {
				case snap := <-s.queue:
					s.insert(snap, timeout)
				default:
					return
				}
			}
		}
	}
}

func (s *Sink) insert(snap telemetry.Snapshot, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := s.exec(ctx, snap); err != nil {
		if n := s.failed.Add(1); n == 1 || n%100 == 0 {
			s.log.Error("telemetry insert failed",
				"workload", snap.Key(), "failed_total", n, "err", err)
		}
		return
	}
	s.written.Add(1)
}

// insertToPool writes one row. Parameters are bound, never interpolated.
func (s *Sink) insertToPool(ctx context.Context, snap telemetry.Snapshot) error {
	_, err := s.pool.Exec(ctx, insertSQL,
		snap.Timestamp, snap.ClusterName, snap.Namespace, snap.WorkloadName,
		string(snap.Runtime), snap.ModelName,
		nullable(snap.RequestRatePerSec), nullable(snap.TokensPerSecond),
		nullable(snap.P50LatencyMs), nullable(snap.P95LatencyMs), nullable(snap.P99LatencyMs),
		nullable(snap.TTFTP50Ms), nullable(snap.TTFTP95Ms), nullable(snap.TTFTP99Ms),
		nullable(snap.QueueTimeP95Ms),
		nullable(snap.RequestsRunning), nullable(snap.RequestsWaiting),
		nullable(snap.WaitingForCapacity), nullable(snap.WaitingDeferred),
		nullable(snap.GPUUtilizationPct), nullable(snap.GPUMemoryUsedPct),
		nullable(snap.KVCacheUsagePct), nullable(snap.PreemptionsPerSec),
		nullable(snap.PrefixCacheHitRatePct),
		nullable(snap.ErrorRatePct), nullable(snap.AbortRatePct),
		nullable(snap.Replicas), nullable(snap.ReadyReplicas),
	)
	return err
}

// nullable maps an unmeasured Metric to SQL NULL. Writing 0.0 instead would
// make "we did not measure GPU utilisation" indistinguishable from "the GPU
// was idle" for anyone querying the history later.
func nullable(m telemetry.Metric) any {
	if !m.OK {
		return nil
	}
	return m.Value
}

const insertSQL = `
INSERT INTO telemetry_snapshots (
	time, cluster_name, namespace, workload_name, runtime, model_name,
	request_rate_per_sec, tokens_per_second,
	p50_latency_ms, p95_latency_ms, p99_latency_ms,
	ttft_p50_ms, ttft_p95_ms, ttft_p99_ms, queue_time_p95_ms,
	requests_running, requests_waiting, waiting_for_capacity, waiting_deferred,
	gpu_utilization_percent, gpu_memory_used_percent,
	kv_cache_usage_percent, preemptions_per_sec, prefix_cache_hit_rate_percent,
	error_rate_percent, abort_rate_percent,
	replicas, ready_replicas
) VALUES (
	$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
	$21,$22,$23,$24,$25,$26,$27,$28
)`
