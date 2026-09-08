package timescale

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// testSink builds a Sink with a stub writer. The behaviour that matters here is
// the queueing — which is where telemetry can be lost — not the SQL.
func testSink(t *testing.T, queueSize int, exec func(context.Context, telemetry.Snapshot) error) *Sink {
	t.Helper()
	s := newSink(Options{QueueSize: queueSize, Logger: discardLogger()})
	s.exec = exec
	s.start(time.Second)
	t.Cleanup(s.Close)
	return s
}

func snapshot(name string) telemetry.Snapshot {
	return telemetry.Snapshot{
		Timestamp:       time.Now().UTC(),
		Namespace:       "inference",
		WorkloadName:    name,
		Runtime:         telemetry.RuntimeVLLM,
		KVCacheUsagePct: telemetry.Observed(50),
	}
}

func TestWritesReachTheBackend(t *testing.T) {
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})

	s := testSink(t, 16, func(_ context.Context, snap telemetry.Snapshot) error {
		mu.Lock()
		got = append(got, snap.WorkloadName)
		if len(got) == 3 {
			close(done)
		}
		mu.Unlock()
		return nil
	})

	for _, n := range []string{"a", "b", "c"} {
		s.Write(snapshot(n))
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes did not reach the backend")
	}

	written, failed, dropped := s.Stats()
	if written != 3 || failed != 0 || dropped != 0 {
		t.Errorf("stats = written %d, failed %d, dropped %d; want 3/0/0", written, failed, dropped)
	}
}

// The point of the bounded queue: with the database unreachable, Write must
// never block the collector. The earlier implementation started a goroutine per
// snapshot, which grows without limit until the process dies.
func TestWriteNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	s := testSink(t, 2, func(context.Context, telemetry.Snapshot) error {
		<-release
		return nil
	})
	t.Cleanup(func() { close(release) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			s.Write(snapshot("stuck"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked when the queue was full; the collector would have stalled")
	}

	_, _, dropped := s.Stats()
	if dropped == 0 {
		t.Error("nothing was recorded as dropped, so telemetry loss would be invisible")
	}
}

func TestFailedWritesAreCountedNotRetriedForever(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	s := testSink(t, 16, func(context.Context, telemetry.Snapshot) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errors.New("database is on fire")
	})

	for i := 0; i < 5; i++ {
		s.Write(snapshot("failing"))
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, failed, _ := s.Stats(); failed == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	written, failed, _ := s.Stats()
	if written != 0 || failed != 5 {
		t.Errorf("stats = written %d, failed %d; want 0/5", written, failed)
	}
	mu.Lock()
	defer mu.Unlock()
	// One attempt per snapshot. Retrying inside the sink would let a failing
	// database starve fresh telemetry behind stale telemetry.
	if attempts != 5 {
		t.Errorf("%d attempts for 5 snapshots; the sink should not retry", attempts)
	}
}

func TestCloseDrainsTheQueue(t *testing.T) {
	var mu sync.Mutex
	var count int
	gate := make(chan struct{})

	s := newSink(Options{QueueSize: 64, Logger: discardLogger()})
	s.exec = func(context.Context, telemetry.Snapshot) error {
		<-gate // hold the writer until everything is queued
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}
	s.start(time.Second)

	for i := 0; i < 20; i++ {
		s.Write(snapshot("draining"))
	}
	close(gate)
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if count != 20 {
		t.Errorf("wrote %d of 20 queued snapshots before shutting down", count)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := newSink(Options{QueueSize: 4, Logger: discardLogger()})
	s.exec = func(context.Context, telemetry.Snapshot) error { return nil }
	s.start(time.Second)

	s.Close()
	s.Close() // must not panic on a second close of the stop channel
}

// Writing 0.0 for an unmeasured value would make "we did not measure GPU
// utilisation" indistinguishable from "the GPU was idle" for anyone querying
// the history later — and AVG() over that column would be silently wrong.
func TestUnmeasuredValuesBecomeSQLNull(t *testing.T) {
	if got := nullable(telemetry.Metric{}); got != nil {
		t.Errorf("unmeasured metric = %v, want nil", got)
	}
	if got := nullable(telemetry.Observed(0)); got != 0.0 {
		t.Errorf("measured zero = %v, want 0.0 — a real zero must not become NULL", got)
	}
	if got := nullable(telemetry.Observed(42.5)); got != 42.5 {
		t.Errorf("measured value = %v, want 42.5", got)
	}
}

func TestOpenRequiresALogger(t *testing.T) {
	_, err := Open(context.Background(), "postgres://localhost/x", Options{})
	if err == nil || !strings.Contains(err.Error(), "logger") {
		t.Errorf("expected a logger-required error, got %v", err)
	}
}

// Every column in the insert must have a matching placeholder, or the statement
// fails at runtime against a real database and nowhere else.
func TestInsertStatementColumnsMatchPlaceholders(t *testing.T) {
	open := strings.Index(insertSQL, "(")
	close := strings.Index(insertSQL, ")")
	columns := strings.Count(insertSQL[open:close], ",") + 1

	values := insertSQL[strings.Index(insertSQL, "VALUES"):]
	placeholders := strings.Count(values, "$")

	if columns != placeholders {
		t.Errorf("%d columns but %d placeholders", columns, placeholders)
	}
}
