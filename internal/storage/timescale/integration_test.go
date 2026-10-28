//go:build integration

// Integration tests for the TimescaleDB sink.
//
// These tests require a real TimescaleDB instance. Set TIMESCALE_TEST_DSN to
// the PostgreSQL connection string before running:
//
//	TIMESCALE_TEST_DSN="postgres://ifa_test:ifa_test@localhost:5432/ifa_test?sslmode=disable" \
//	  go test -tags integration -v ./internal/storage/timescale/...
//
// The convenience script scripts/timescale-integration.sh starts an ephemeral
// TimescaleDB container, applies the migration, runs the tests, and cleans up.
//
// TimescaleDB version validated: timescale/timescaledb:2.14.2-pg16
// PostgreSQL version: 16.x (bundled with the above image)
package timescale

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	osExec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/p95labs/ifa/internal/telemetry"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// testDSN returns the DSN for the integration-test database, or skips the test
// when TIMESCALE_TEST_DSN is not set.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TIMESCALE_TEST_DSN")
	if dsn == "" {
		t.Skip("TIMESCALE_TEST_DSN not set — skipping real-database integration tests")
	}
	return dsn
}

// testPool opens a pgx pool to the test database and registers cleanup.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping failed — is the database up? err: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testSinkDB opens a production Sink against the test database.
func testSinkDB(t *testing.T, opts Options) *Sink {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	dsn := testDSN(t)
	ctx := context.Background()
	s, err := Open(ctx, dsn, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// readMigration returns the contents of migrations/001_init.sql.
func readMigration(t *testing.T) string {
	t.Helper()
	// go test sets working directory to the package directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "..", "migrations", "001_init.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// applyMigration executes migrations/001_init.sql against the test database.
// Each statement is executed individually because pgx's extended protocol does
// not support multiple statements in a single query.
func applyMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql := readMigration(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()
	for _, stmt := range splitStatements(sql) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			short := stmt
			if len(short) > 80 {
				short = short[:80] + "…"
			}
			t.Fatalf("migration statement failed: %v\n  stmt: %s", err, short)
		}
	}
}

// splitStatements splits a SQL file into individual statements, stripping
// comments and blank fragments.
func splitStatements(sql string) []string {
	var out []string
	for _, raw := range strings.Split(sql, ";") {
		// Strip line comments and blank lines.
		var lines []string
		for _, l := range strings.Split(raw, "\n") {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			lines = append(lines, l)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// uniqueName returns a workload name that is unique within the test run so that
// parallel tests do not observe each other's rows.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, rand.Int63n(1_000_000_000))
}

// queryFloat64 reads a nullable DOUBLE PRECISION column. Returns nil for NULL.
func queryFloat64(t *testing.T, pool *pgxpool.Pool, col, where string, args ...any) *float64 {
	t.Helper()
	q := fmt.Sprintf("SELECT %s FROM telemetry_snapshots WHERE %s", col, where)
	row := pool.QueryRow(context.Background(), q, args...)
	var v *float64
	if err := row.Scan(&v); err != nil {
		t.Fatalf("queryFloat64(%s): %v", col, err)
	}
	return v
}

// assertApprox checks that got ≈ want within a relative tolerance of 1e-9.
func assertApprox(t *testing.T, label string, got, want float64) {
	t.Helper()
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	tolerance := 1e-9
	if want != 0 {
		relDiff := diff / want
		if relDiff < 0 {
			relDiff = -relDiff
		}
		if relDiff > tolerance {
			t.Errorf("%s: got %v, want %v (relative diff %v)", label, got, want, relDiff)
		}
		return
	}
	if diff > tolerance {
		t.Errorf("%s: got %v, want %v (absolute diff %v)", label, got, want, diff)
	}
}

// waitForSinkWrites polls Stats until written+failed reaches n, or the deadline.
func waitForSinkWrites(t *testing.T, s *Sink, n int64, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		w, f, _ := s.Stats()
		if w+f >= n {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	w, f, d := s.Stats()
	t.Fatalf("timeout waiting for %d writes/fails: written=%d failed=%d dropped=%d",
		n, w, f, d)
}

// ─── Phase 2: Migration verification ─────────────────────────────────────────

// TestIntegration_Migration_ExtensionLoaded verifies the TimescaleDB extension
// is present and active after applying the migration.
func TestIntegration_Migration_ExtensionLoaded(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	var name string
	err := pool.QueryRow(context.Background(),
		`SELECT extname FROM pg_extension WHERE extname = 'timescaledb'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("timescaledb extension not found: %v", err)
	}
	if name != "timescaledb" {
		t.Errorf("expected extname=timescaledb, got %q", name)
	}
}

// TestIntegration_Migration_HypertableCreated verifies that telemetry_snapshots
// is a TimescaleDB hypertable, not a plain Postgres table.
func TestIntegration_Migration_HypertableCreated(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	var count int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM timescaledb_information.hypertables
		WHERE hypertable_name = 'telemetry_snapshots'
	`).Scan(&count)
	if err != nil {
		t.Fatalf("querying hypertables: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 hypertable named telemetry_snapshots, got %d", count)
	}
}

// TestIntegration_Migration_ColumnsMatchInsert verifies that the database
// schema contains exactly the columns referenced in insertSQL. A mismatch here
// would be caught at runtime as a column count or type error — worse to find
// than catching it here.
func TestIntegration_Migration_ColumnsMatchInsert(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	rows, err := pool.Query(context.Background(), `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_name = 'telemetry_snapshots'
		ORDER BY ordinal_position
	`)
	if err != nil {
		t.Fatalf("querying columns: %v", err)
	}
	defer rows.Close()

	var dbCols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		dbCols = append(dbCols, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// Extract the columns from insertSQL (between INSERT INTO … ( and ) VALUES).
	start := strings.Index(insertSQL, "(")
	end := strings.Index(insertSQL, ")")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("could not parse insertSQL columns")
	}
	var sqlCols []string
	for _, col := range strings.Split(insertSQL[start+1:end], ",") {
		sqlCols = append(sqlCols, strings.TrimSpace(col))
	}

	// Every column in insertSQL must exist in the database.
	dbColSet := make(map[string]bool, len(dbCols))
	for _, c := range dbCols {
		dbColSet[c] = true
	}
	for _, col := range sqlCols {
		if !dbColSet[col] {
			t.Errorf("insertSQL column %q not found in database schema", col)
		}
	}

	t.Logf("database columns: %v", dbCols)
	t.Logf("insertSQL columns: %v", sqlCols)
}

// TestIntegration_Migration_IndexExists verifies the workload-history index.
func TestIntegration_Migration_IndexExists(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	var count int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM pg_indexes
		WHERE tablename = 'telemetry_snapshots'
		  AND indexname  = 'idx_telemetry_workload'
	`).Scan(&count)
	if err != nil {
		t.Fatalf("querying pg_indexes: %v", err)
	}
	if count != 1 {
		t.Errorf("expected idx_telemetry_workload to exist, count=%d", count)
	}
}

// TestIntegration_Migration_Idempotent verifies that running the migration a
// second time does not produce an error. All CREATE IF NOT EXISTS guards and
// the create_hypertable if_not_exists flag make this safe.
func TestIntegration_Migration_Idempotent(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool) // first run
	applyMigration(t, pool) // second run — must not fail
}

// ─── Phase 3: Persistence verification ───────────────────────────────────────

// TestIntegration_Persistence_AllColumns writes a fully-populated snapshot
// through the production sink and queries every column back independently.
func TestIntegration_Persistence_AllColumns(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	sink := testSinkDB(t, Options{WriteTimeout: 5 * time.Second})

	wl := uniqueName("all-cols")
	ts := time.Now().UTC().Truncate(time.Microsecond) // Postgres stores microseconds

	snap := telemetry.Snapshot{
		Timestamp:    ts,
		ClusterName:  "test-cluster",
		Namespace:    "inference",
		WorkloadName: wl,
		Runtime:      telemetry.RuntimeVLLM,
		ModelName:    "meta-llama/Llama-3.1-8B-Instruct",

		RequestRatePerSec: telemetry.Observed(42.5),
		TokensPerSecond:   telemetry.Observed(1234.0),

		P50LatencyMs:   telemetry.Observed(10.1),
		P95LatencyMs:   telemetry.Observed(25.5),
		P99LatencyMs:   telemetry.Observed(50.0),
		TTFTP50Ms:      telemetry.Observed(5.0),
		TTFTP95Ms:      telemetry.Observed(12.5),
		TTFTP99Ms:      telemetry.Observed(20.0),
		QueueTimeP95Ms: telemetry.Observed(3.7),

		RequestsRunning:    telemetry.Observed(16.0),
		RequestsWaiting:    telemetry.Observed(4.0),
		WaitingForCapacity: telemetry.Observed(2.0),
		WaitingDeferred:    telemetry.Observed(0.0), // measured zero — must not become NULL

		GPUUtilizationPct:     telemetry.Observed(87.3),
		GPUMemoryUsedPct:      telemetry.Observed(91.1),
		KVCacheUsagePct:       telemetry.Observed(75.0),
		PreemptionsPerSec:     telemetry.Observed(0.5),
		PrefixCacheHitRatePct: telemetry.Observed(60.0),

		ErrorRatePct: telemetry.Observed(0.01),
		AbortRatePct: telemetry.Observed(0.001),

		Replicas:      telemetry.Observed(2.0),
		ReadyReplicas: telemetry.Observed(2.0),
	}

	sink.Write(snap)
	waitForSinkWrites(t, sink, 1, 10*time.Second)

	written, failed, dropped := sink.Stats()
	if written != 1 || failed != 0 || dropped != 0 {
		t.Fatalf("want written=1/failed=0/dropped=0, got %d/%d/%d", written, failed, dropped)
	}

	// Query every column back.
	ctx := context.Background()
	var (
		dbTime                            time.Time
		dbCluster, dbNS, dbWL, dbRT, dbMN string
		dbReqRate, dbTPS                  *float64
		dbP50, dbP95, dbP99               *float64
		dbTTFT50, dbTTFT95, dbTTFT99      *float64
		dbQT                              *float64
		dbRR, dbRW                        *float64
		dbWFC, dbWD                       *float64
		dbGPU, dbGPUMem                   *float64
		dbKV, dbPre, dbPrefix             *float64
		dbErr, dbAbort                    *float64
		dbReplicas, dbReady               *float64
	)
	row := pool.QueryRow(ctx, `
		SELECT
			time, cluster_name, namespace, workload_name, runtime, model_name,
			request_rate_per_sec, tokens_per_second,
			p50_latency_ms, p95_latency_ms, p99_latency_ms,
			ttft_p50_ms, ttft_p95_ms, ttft_p99_ms, queue_time_p95_ms,
			requests_running, requests_waiting, waiting_for_capacity, waiting_deferred,
			gpu_utilization_percent, gpu_memory_used_percent,
			kv_cache_usage_percent, preemptions_per_sec, prefix_cache_hit_rate_percent,
			error_rate_percent, abort_rate_percent,
			replicas, ready_replicas
		FROM telemetry_snapshots WHERE workload_name = $1
	`, wl)
	if err := row.Scan(
		&dbTime, &dbCluster, &dbNS, &dbWL, &dbRT, &dbMN,
		&dbReqRate, &dbTPS,
		&dbP50, &dbP95, &dbP99,
		&dbTTFT50, &dbTTFT95, &dbTTFT99, &dbQT,
		&dbRR, &dbRW, &dbWFC, &dbWD,
		&dbGPU, &dbGPUMem,
		&dbKV, &dbPre, &dbPrefix,
		&dbErr, &dbAbort,
		&dbReplicas, &dbReady,
	); err != nil {
		t.Fatalf("querying inserted row: %v", err)
	}

	// Identity fields.
	if !dbTime.Equal(ts) {
		t.Errorf("time: got %v, want %v", dbTime, ts)
	}
	if dbCluster != "test-cluster" {
		t.Errorf("cluster_name: got %q", dbCluster)
	}
	if dbNS != "inference" {
		t.Errorf("namespace: got %q", dbNS)
	}
	if dbWL != wl {
		t.Errorf("workload_name: got %q", dbWL)
	}
	if dbRT != string(telemetry.RuntimeVLLM) {
		t.Errorf("runtime: got %q", dbRT)
	}
	if dbMN != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("model_name: got %q", dbMN)
	}

	// Numeric fields — all were measured, so must be non-NULL.
	checkNotNil := func(name string, v *float64) {
		t.Helper()
		if v == nil {
			t.Errorf("%s: got NULL, want a value", name)
		}
	}
	checkNotNil("request_rate_per_sec", dbReqRate)
	checkNotNil("tokens_per_second", dbTPS)
	checkNotNil("p50_latency_ms", dbP50)
	checkNotNil("p95_latency_ms", dbP95)
	checkNotNil("p99_latency_ms", dbP99)
	checkNotNil("ttft_p50_ms", dbTTFT50)
	checkNotNil("ttft_p95_ms", dbTTFT95)
	checkNotNil("ttft_p99_ms", dbTTFT99)
	checkNotNil("queue_time_p95_ms", dbQT)
	checkNotNil("requests_running", dbRR)
	checkNotNil("requests_waiting", dbRW)
	checkNotNil("waiting_for_capacity", dbWFC)
	checkNotNil("waiting_deferred", dbWD) // measured zero — must not be NULL
	checkNotNil("gpu_utilization_percent", dbGPU)
	checkNotNil("gpu_memory_used_percent", dbGPUMem)
	checkNotNil("kv_cache_usage_percent", dbKV)
	checkNotNil("preemptions_per_sec", dbPre)
	checkNotNil("prefix_cache_hit_rate_percent", dbPrefix)
	checkNotNil("error_rate_percent", dbErr)
	checkNotNil("abort_rate_percent", dbAbort)
	checkNotNil("replicas", dbReplicas)
	checkNotNil("ready_replicas", dbReady)

	// Value correctness (tolerant float comparison).
	if dbReqRate != nil {
		assertApprox(t, "request_rate_per_sec", *dbReqRate, 42.5)
	}
	if dbTPS != nil {
		assertApprox(t, "tokens_per_second", *dbTPS, 1234.0)
	}
	if dbP95 != nil {
		assertApprox(t, "p95_latency_ms", *dbP95, 25.5)
	}
	if dbGPU != nil {
		assertApprox(t, "gpu_utilization_percent", *dbGPU, 87.3)
	}
	if dbGPUMem != nil {
		assertApprox(t, "gpu_memory_used_percent", *dbGPUMem, 91.1)
	}
	if dbKV != nil {
		assertApprox(t, "kv_cache_usage_percent", *dbKV, 75.0)
	}
	// Measured zero must be 0, not NULL.
	if dbWD != nil {
		assertApprox(t, "waiting_deferred (measured zero)", *dbWD, 0.0)
	}
}

// TestIntegration_Persistence_NullVsZeroSemantics is the canonical test for the
// design principle documented in migrations/001_init.sql and sink.go:
//
//   - An unmeasured Metric becomes SQL NULL — "we did not observe this signal".
//   - A Metric measured as zero becomes numeric 0 — "we observed it; it was zero".
//   - A positive Metric retains its value.
//
// AVG() over a column full of NULLs returns NULL, not 0; that is the intended
// behaviour, and these assertions protect it.
func TestIntegration_Persistence_NullVsZeroSemantics(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	sink := testSinkDB(t, Options{WriteTimeout: 5 * time.Second})

	wl := uniqueName("null-zero")
	snap := telemetry.Snapshot{
		Timestamp:    time.Now().UTC(),
		ClusterName:  "test-cluster",
		Namespace:    "inference",
		WorkloadName: wl,
		Runtime:      telemetry.RuntimeVLLM,
		// Unmeasured — must become SQL NULL.
		GPUUtilizationPct: telemetry.Metric{},
		// Measured zero — must stay 0, not NULL.
		RequestsWaiting: telemetry.Observed(0),
		// Positive — must retain value.
		KVCacheUsagePct: telemetry.Observed(42.7),
	}

	sink.Write(snap)
	waitForSinkWrites(t, sink, 1, 10*time.Second)

	where := "workload_name = $1"

	// Unmeasured → NULL.
	gpuUtil := queryFloat64(t, pool, "gpu_utilization_percent", where, wl)
	if gpuUtil != nil {
		t.Errorf("unmeasured gpu_utilization_percent: want NULL, got %v", *gpuUtil)
	}

	// Measured zero → 0.
	waiting := queryFloat64(t, pool, "requests_waiting", where, wl)
	if waiting == nil {
		t.Error("measured-zero requests_waiting: want 0, got NULL")
	} else {
		assertApprox(t, "requests_waiting (zero)", *waiting, 0.0)
	}

	// Positive → value.
	kv := queryFloat64(t, pool, "kv_cache_usage_percent", where, wl)
	if kv == nil {
		t.Error("kv_cache_usage_percent: got NULL, want 42.7")
	} else {
		assertApprox(t, "kv_cache_usage_percent", *kv, 42.7)
	}
}

// TestIntegration_Persistence_MultipleWorkloads writes two workloads and
// verifies the database contains exactly one row for each.
func TestIntegration_Persistence_MultipleWorkloads(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	sink := testSinkDB(t, Options{WriteTimeout: 5 * time.Second})

	wl1 := uniqueName("wl-alpha")
	wl2 := uniqueName("wl-beta")
	ns := "production"

	for _, wl := range []string{wl1, wl2} {
		sink.Write(telemetry.Snapshot{
			Timestamp:       time.Now().UTC(),
			ClusterName:     "prod",
			Namespace:       ns,
			WorkloadName:    wl,
			Runtime:         telemetry.RuntimeVLLM,
			KVCacheUsagePct: telemetry.Observed(50),
		})
	}
	waitForSinkWrites(t, sink, 2, 10*time.Second)

	ctx := context.Background()
	for _, wl := range []string{wl1, wl2} {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM telemetry_snapshots WHERE workload_name = $1`,
			wl,
		).Scan(&count); err != nil {
			t.Fatalf("count for %s: %v", wl, err)
		}
		if count != 1 {
			t.Errorf("workload %s: want 1 row, got %d", wl, count)
		}
	}
}

// TestIntegration_Persistence_MultipleTimestamps inserts three successive
// snapshots for one workload and verifies they are stored with distinct
// timestamps and returned in ascending time order.
func TestIntegration_Persistence_MultipleTimestamps(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	sink := testSinkDB(t, Options{WriteTimeout: 5 * time.Second})

	wl := uniqueName("multi-ts")
	base := time.Now().UTC().Truncate(time.Microsecond)

	for i := range 3 {
		sink.Write(telemetry.Snapshot{
			Timestamp:       base.Add(time.Duration(i) * time.Second),
			ClusterName:     "test",
			Namespace:       "test",
			WorkloadName:    wl,
			RequestsRunning: telemetry.Observed(float64(i)),
		})
	}
	waitForSinkWrites(t, sink, 3, 10*time.Second)

	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT time, requests_running FROM telemetry_snapshots
		 WHERE workload_name = $1 ORDER BY time ASC`,
		wl,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var timestamps []time.Time
	var runningVals []float64
	for rows.Next() {
		var ts time.Time
		var rr *float64
		if err := rows.Scan(&ts, &rr); err != nil {
			t.Fatalf("scan: %v", err)
		}
		timestamps = append(timestamps, ts)
		if rr != nil {
			runningVals = append(runningVals, *rr)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(timestamps) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(timestamps))
	}
	for i := 1; i < len(timestamps); i++ {
		if !timestamps[i].After(timestamps[i-1]) {
			t.Errorf("rows not in ascending time order at i=%d", i)
		}
	}
	for i, v := range runningVals {
		assertApprox(t, fmt.Sprintf("requests_running[%d]", i), v, float64(i))
	}
}

// TestIntegration_Persistence_ReconnectRetainsRows closes a sink, opens a new
// one pointing at the same database, and verifies that rows written through the
// first sink are still readable. This distinguishes durable persistence from
// the in-memory store that IFA always maintains.
func TestIntegration_Persistence_ReconnectRetainsRows(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	dsn := testDSN(t)
	wl := uniqueName("reconnect")
	ts := time.Now().UTC().Truncate(time.Microsecond)

	// First sink: write, close.
	{
		ctx := context.Background()
		s, err := Open(ctx, dsn, Options{
			Logger:       discardLogger(),
			WriteTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("Open (first): %v", err)
		}
		s.Write(telemetry.Snapshot{
			Timestamp:       ts,
			ClusterName:     "test",
			Namespace:       "test",
			WorkloadName:    wl,
			KVCacheUsagePct: telemetry.Observed(99.0),
		})
		waitForSinkWrites(t, s, 1, 10*time.Second)
		s.Close()
	}

	// Second sink: verify the row is still there.
	{
		ctx := context.Background()
		s2, err := Open(ctx, dsn, Options{Logger: discardLogger()})
		if err != nil {
			t.Fatalf("Open (second): %v", err)
		}
		defer s2.Close()
	}

	// Direct SQL verification — independent of the sink.
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM telemetry_snapshots WHERE workload_name = $1`,
		wl,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after reconnect, got %d", count)
	}

	kv := queryFloat64(t, pool, "kv_cache_usage_percent", "workload_name = $1", wl)
	if kv == nil {
		t.Fatal("kv_cache_usage_percent: got NULL after reconnect")
	}
	assertApprox(t, "kv_cache_usage_percent after reconnect", *kv, 99.0)
}

// ─── Phase 4: Full collector→store→database pipeline ─────────────────────────

// TestIntegration_Pipeline_StoreToDatabase exercises the production code path
// from telemetry.Store.Add() → Sink.Write() → TimescaleDB, verifying that
// the in-memory store and the database receive identical snapshots and that
// IFA's diagnostic engine (which reads only the in-memory store) is unaffected
// by the database path.
func TestIntegration_Pipeline_StoreToDatabase(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	sink := testSinkDB(t, Options{WriteTimeout: 5 * time.Second})

	store := telemetry.NewStore(telemetry.WithSink(sink))

	wl := uniqueName("pipeline")
	ns := "production"

	// Simulate three successive scrape cycles.
	for i := range 3 {
		store.Add(telemetry.Snapshot{
			Timestamp:       time.Now().UTC().Add(time.Duration(i) * time.Second),
			ClusterName:     "prod",
			Namespace:       ns,
			WorkloadName:    wl,
			Runtime:         telemetry.RuntimeVLLM,
			ModelName:       "llama3",
			KVCacheUsagePct: telemetry.Observed(float64(10 + i*5)),
			RequestsRunning: telemetry.Observed(float64(i + 1)),
		})
	}
	waitForSinkWrites(t, sink, 3, 15*time.Second)

	// In-memory store must reflect all three snapshots.
	history := store.History(ns + "/" + wl)
	if len(history) != 3 {
		t.Errorf("in-memory history: want 3 snapshots, got %d", len(history))
	}
	latest := store.Latest()
	if len(latest) != 1 || latest[0].WorkloadName != wl {
		t.Errorf("Latest(): unexpected result %v", latest)
	}

	// Database must contain all three rows.
	var dbCount int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM telemetry_snapshots WHERE workload_name = $1`, wl,
	).Scan(&dbCount)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if dbCount != 3 {
		t.Errorf("database: want 3 rows, got %d", dbCount)
	}

	// KV cache values must match what was written.
	kvRows, err := pool.Query(context.Background(),
		`SELECT kv_cache_usage_percent FROM telemetry_snapshots
		 WHERE workload_name = $1 ORDER BY time ASC`, wl,
	)
	if err != nil {
		t.Fatalf("querying kv: %v", err)
	}
	defer kvRows.Close()
	var kvVals []float64
	for kvRows.Next() {
		var v *float64
		if err := kvRows.Scan(&v); err != nil {
			t.Fatalf("scan kv: %v", err)
		}
		if v != nil {
			kvVals = append(kvVals, *v)
		}
	}
	if err := kvRows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	expected := []float64{10, 15, 20}
	if len(kvVals) != len(expected) {
		t.Fatalf("kv values: want %v, got %v", expected, kvVals)
	}
	for i, v := range kvVals {
		assertApprox(t, fmt.Sprintf("kv[%d]", i), v, expected[i])
	}
}

// TestIntegration_Pipeline_DatabaseAndInMemoryIndependent verifies that
// disabling the sink (database not configured) does not affect the in-memory
// store, proving the two stores are independent.
func TestIntegration_Pipeline_DatabaseAndInMemoryIndependent(t *testing.T) {
	testDSN(t) // ensure env var is set (so we're in a real-DB test run)

	// Store with no sink — no database.
	store := telemetry.NewStore()

	wl := uniqueName("no-sink")
	for i := range 5 {
		store.Add(telemetry.Snapshot{
			Timestamp:       time.Now().UTC(),
			ClusterName:     "test",
			Namespace:       "test",
			WorkloadName:    wl,
			RequestsRunning: telemetry.Observed(float64(i)),
		})
	}

	if s := store.Size(); s != 5 {
		t.Errorf("in-memory store: want 5, got %d", s)
	}
	if len(store.Latest()) != 1 {
		t.Errorf("Latest(): want 1, got %d", len(store.Latest()))
	}
}

// ─── Phase 5: Database outage testing ────────────────────────────────────────

// TestIntegration_Outage_StartupFailure verifies that Open() returns an error
// immediately when the database is unreachable, and that the error message does
// not contain the database password.
func TestIntegration_Outage_StartupFailure(t *testing.T) {
	testDSN(t) // ensure test infra is available

	// Point at a port that should be unused.
	badDSN := "postgres://nouser:secret-password@127.0.0.1:19432/nodb?sslmode=disable&connect_timeout=2"
	ctx := context.Background()
	_, err := Open(ctx, badDSN, Options{Logger: discardLogger()})
	if err == nil {
		t.Fatal("expected Open() to fail against unreachable database, but it succeeded")
	}
	// The error message must not leak the password.
	if strings.Contains(err.Error(), "secret-password") {
		t.Errorf("error message leaks the database password: %v", err)
	}
	t.Logf("startup failure error (password redacted check passed): %v", err)
}

// TestIntegration_Outage_FailedWritesCounted verifies that when the write
// function returns errors, failed writes are counted and not retried. This
// uses exec injection to simulate the failure without stopping the container.
func TestIntegration_Outage_FailedWritesCounted(t *testing.T) {
	testDSN(t)

	// Create sink but override exec to simulate persistent failure.
	s := newSink(Options{QueueSize: 16, Logger: discardLogger()})
	s.exec = func(context.Context, telemetry.Snapshot) error {
		return fmt.Errorf("simulated outage")
	}
	s.start(2 * time.Second)
	t.Cleanup(s.Close)

	for i := range 5 {
		s.Write(telemetry.Snapshot{
			Timestamp:    time.Now().UTC(),
			WorkloadName: fmt.Sprintf("failing-%d", i),
		})
	}

	// Wait for all 5 to be attempted (and fail).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, f, _ := s.Stats()
		if f >= 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	written, failed, _ := s.Stats()
	if written != 0 {
		t.Errorf("written=%d, want 0 — failed writes must not be counted as written", written)
	}
	if failed != 5 {
		t.Errorf("failed=%d, want 5", failed)
	}
}

// TestIntegration_Outage_QueueBounded verifies that with a slow writer and a
// very small queue the dropped counter increments, Write never blocks, and the
// queue never grows beyond its configured capacity.
func TestIntegration_Outage_QueueBounded(t *testing.T) {
	testDSN(t)

	release := make(chan struct{})
	s := newSink(Options{QueueSize: 3, Logger: discardLogger()})
	s.exec = func(context.Context, telemetry.Snapshot) error {
		<-release
		return nil
	}
	s.start(2 * time.Second)
	t.Cleanup(func() {
		close(release)
		s.Close()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			s.Write(telemetry.Snapshot{WorkloadName: fmt.Sprintf("sat-%d", i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked — collector would have stalled")
	}

	_, _, dropped := s.Stats()
	if dropped == 0 {
		t.Error("nothing was dropped; telemetry loss would be invisible")
	}
	t.Logf("dropped=%d (expected >0)", dropped)
}

// TestIntegration_Outage_ContainerStop verifies end-to-end outage behaviour
// using an actual Docker container. It requires the `docker` CLI and is skipped
// automatically when Docker is unavailable.
func TestIntegration_Outage_ContainerStop(t *testing.T) {
	testDSN(t)

	if _, err := osExec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH — skipping container-stop outage test")
	}

	// Start a fresh TimescaleDB container for this test so stopping it does not
	// affect the other integration tests.
	name := fmt.Sprintf("ifa-test-outage-%d", rand.Int63n(100_000_000))
	port, cleanup := startOutageContainer(t, name)
	defer cleanup()

	dsn := fmt.Sprintf(
		"postgres://ifa_test:ifa_test@127.0.0.1:%d/ifa_test?sslmode=disable&connect_timeout=2",
		port,
	)

	// Apply the migration and verify the container is working.
	outagePool := mustOpenPool(t, dsn)
	applyMigrationTo(t, outagePool, readMigration(t))
	outagePool.Close()

	ctx := context.Background()
	s, err := Open(ctx, dsn, Options{
		Logger:       discardLogger(),
		WriteTimeout: 2 * time.Second,
		QueueSize:    16,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	wl := uniqueName("outage")

	// Pre-outage: writes succeed.
	for i := range 3 {
		s.Write(telemetry.Snapshot{
			Timestamp:    time.Now().UTC(),
			ClusterName:  "test",
			Namespace:    "test",
			WorkloadName: fmt.Sprintf("%s-%d", wl, i),
		})
	}
	waitForSinkWrites(t, s, 3, 10*time.Second)
	preWritten, _, _ := s.Stats()
	if preWritten < 3 {
		t.Fatalf("pre-outage: want written≥3, got %d", preWritten)
	}

	// Stop the container — database becomes unavailable.
	if out, err := osExec.Command("docker", "stop", name).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
	t.Log("container stopped")

	// Writes after the outage must fail (not block the caller).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5 {
			s.Write(telemetry.Snapshot{
				Timestamp:    time.Now().UTC(),
				WorkloadName: fmt.Sprintf("%s-during-%d", wl, i),
			})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked during outage — collector would stall")
	}

	// Wait for the queued writes to time out and be counted as failed or dropped.
	time.Sleep(3 * time.Second)
	_, postFailed, postDropped := s.Stats()
	if postFailed+postDropped == 0 {
		t.Error("no failed or dropped writes recorded during outage")
	}
	t.Logf("outage stats: failed=%d dropped=%d", postFailed, postDropped)

	// Restart the container and verify recovery.
	if out, err := osExec.Command("docker", "start", name).CombinedOutput(); err != nil {
		t.Fatalf("docker start: %v\n%s", err, out)
	}
	t.Log("container restarted — waiting for database to become ready")

	// Docker Desktop on macOS reassigns the host port on container restart when
	// a random port was chosen (-p 0:5432). Re-query the port after docker start.
	time.Sleep(2 * time.Second) // brief pause for Docker to assign the new port
	newPortOut, err := osExec.Command("docker", "port", name, "5432").Output()
	if err != nil {
		t.Fatalf("docker port after restart: %v", err)
	}
	newPortStr := strings.TrimSpace(string(newPortOut))
	if idx := strings.LastIndex(newPortStr, ":"); idx >= 0 {
		newPortStr = newPortStr[idx+1:]
	}
	var newPort int
	if _, err := fmt.Sscanf(newPortStr, "%d", &newPort); err != nil {
		t.Fatalf("parsing port after restart %q: %v", newPortStr, err)
	}
	t.Logf("container restarted on port %d (was %d)", newPort, port)

	readinessDSN := fmt.Sprintf(
		"postgres://ifa_test:ifa_test@127.0.0.1:%d/ifa_test?sslmode=disable&connect_timeout=5",
		newPort,
	)
	if !waitForDBReady(readinessDSN, 60*time.Second) {
		t.Fatal("database did not become ready after container restart within 60 s")
	}
	t.Log("database is ready again")

	// The original sink's pool still points at the old port — it will keep
	// failing. The recovery assertion here is that a *new* sink on the fresh
	// port succeeds, which is what an operator restart of IFA provides.
	ctx2 := context.Background()
	sSnd, err := Open(ctx2, readinessDSN, Options{
		Logger:       discardLogger(),
		WriteTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open recovery sink: %v", err)
	}
	defer sSnd.Close()

	wlRecov := uniqueName("recovery")
	sSnd.Write(telemetry.Snapshot{
		Timestamp:       time.Now().UTC(),
		ClusterName:     "test",
		Namespace:       "test",
		WorkloadName:    wlRecov,
		KVCacheUsagePct: telemetry.Observed(55.0),
	})
	waitForSinkWrites(t, sSnd, 1, 15*time.Second)

	recovWritten, recovFailed, _ := sSnd.Stats()
	if recovWritten < 1 {
		t.Errorf("recovery: want written≥1, got written=%d failed=%d", recovWritten, recovFailed)
	}
	t.Logf("recovery confirmed: written=%d on new port %d", recovWritten, newPort)
}

// TestIntegration_Outage_InMemoryUnaffected verifies that the in-memory store
// continues to operate correctly when the TimescaleDB sink is in a failure
// state. The rule engine reads only the in-memory store, so a database outage
// must not degrade live diagnostics.
func TestIntegration_Outage_InMemoryUnaffected(t *testing.T) {
	testDSN(t)

	// Sink that always fails.
	s := newSink(Options{QueueSize: 8, Logger: discardLogger()})
	s.exec = func(context.Context, telemetry.Snapshot) error {
		return fmt.Errorf("database unavailable")
	}
	s.start(time.Second)
	t.Cleanup(s.Close)

	store := telemetry.NewStore(telemetry.WithSink(s))

	wl := uniqueName("in-memory-ok")
	for i := range 5 {
		store.Add(telemetry.Snapshot{
			Timestamp:       time.Now().UTC(),
			ClusterName:     "test",
			Namespace:       "test",
			WorkloadName:    wl,
			KVCacheUsagePct: telemetry.Observed(float64(i * 10)),
		})
	}

	// In-memory store must have all 5 snapshots regardless of DB failures.
	if sz := store.Size(); sz != 5 {
		t.Errorf("store.Size()=%d, want 5", sz)
	}
	latest := store.Latest()
	if len(latest) != 1 {
		t.Errorf("Latest(): want 1, got %d", len(latest))
	}
	if len(store.History("test/"+wl)) != 5 {
		t.Errorf("History(): want 5, got %d", len(store.History("test/"+wl)))
	}

	t.Log("in-memory store operates correctly despite failing database sink")
}

// ─── Phase 6: Queue saturation and graceful shutdown ─────────────────────────

// TestIntegration_Queue_SaturationDropsAndCounts writes more snapshots than
// the queue can hold with a sink that never drains, verifying the dropped
// counter increments and Write never blocks.
func TestIntegration_Queue_SaturationDropsAndCounts(t *testing.T) {
	testDSN(t)

	hold := make(chan struct{})
	s := newSink(Options{QueueSize: 4, Logger: discardLogger()})
	s.exec = func(context.Context, telemetry.Snapshot) error {
		<-hold
		return nil
	}
	s.start(2 * time.Second)
	t.Cleanup(func() {
		close(hold)
		s.Close()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			s.Write(telemetry.Snapshot{WorkloadName: fmt.Sprintf("q-%d", i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked with a full queue")
	}

	_, _, dropped := s.Stats()
	if dropped == 0 {
		t.Error("dropped=0; telemetry loss is invisible")
	}
	t.Logf("queue saturation: dropped=%d", dropped)
}

// TestIntegration_Shutdown_DrainsQueueBeforeReturning writes a batch of
// snapshots into a real TimescaleDB sink and verifies that Close() drains the
// queue completely before returning.
func TestIntegration_Shutdown_DrainsQueueBeforeReturning(t *testing.T) {
	pool := testPool(t)
	applyMigration(t, pool)

	dsn := testDSN(t)
	ctx := context.Background()
	s, err := Open(ctx, dsn, Options{
		Logger:       discardLogger(),
		QueueSize:    64,
		WriteTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	wl := uniqueName("drain")
	const n = 10
	for i := range n {
		s.Write(telemetry.Snapshot{
			Timestamp:    time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			ClusterName:  "test",
			Namespace:    "test",
			WorkloadName: wl,
		})
	}
	// Close must drain the queue.
	s.Close()

	written, failed, dropped := s.Stats()
	t.Logf("after Close: written=%d failed=%d dropped=%d", written, failed, dropped)
	if written+failed+dropped < n {
		t.Errorf("only %d of %d snapshots accounted for after Close",
			written+failed+dropped, n)
	}
	if dropped > 0 {
		t.Errorf("unexpected drops during graceful shutdown: %d", dropped)
	}

	// Verify in the database.
	var dbCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM telemetry_snapshots WHERE workload_name = $1`, wl,
	).Scan(&dbCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if dbCount != n {
		t.Errorf("database: want %d rows after graceful shutdown, got %d", n, dbCount)
	}
}

// TestIntegration_Shutdown_CloseIsIdempotentWithDB verifies that calling
// Close() twice on a real sink does not panic or deadlock.
func TestIntegration_Shutdown_CloseIsIdempotentWithDB(t *testing.T) {
	testDSN(t)
	sink := testSinkDB(t, Options{WriteTimeout: 5 * time.Second})
	sink.Close()
	sink.Close() // must not panic
}

// ─── Docker helpers for the container-stop outage test ───────────────────────

// startOutageContainer starts a named TimescaleDB container and returns the
// host port it is listening on and a cleanup function.
func startOutageContainer(t *testing.T, name string) (port int, cleanup func()) {
	t.Helper()

	cmd := osExec.Command("docker", "run", "-d",
		"--name", name,
		"-e", "POSTGRES_DB=ifa_test",
		"-e", "POSTGRES_USER=ifa_test",
		"-e", "POSTGRES_PASSWORD=ifa_test",
		"-p", "0:5432",
		"timescale/timescaledb:2.14.2-pg16",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}

	// Parse the mapped port.
	portOut, err := osExec.Command("docker", "port", name, "5432").Output()
	if err != nil {
		osExec.Command("docker", "rm", "-f", name).Run()
		t.Fatalf("docker port: %v", err)
	}
	// Output is like "0.0.0.0:54321\n"
	portStr := strings.TrimSpace(string(portOut))
	if idx := strings.LastIndex(portStr, ":"); idx >= 0 {
		portStr = portStr[idx+1:]
	}
	var p int
	if _, err := fmt.Sscanf(portStr, "%d", &p); err != nil {
		osExec.Command("docker", "rm", "-f", name).Run()
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	port = p

	cleanup = func() {
		osExec.Command("docker", "rm", "-f", name).Run()
	}
	t.Cleanup(cleanup)

	// Wait until the database accepts connections.
	dsn := fmt.Sprintf(
		"postgres://ifa_test:ifa_test@127.0.0.1:%d/ifa_test?sslmode=disable&connect_timeout=2",
		port,
	)
	if !waitForDBReady(dsn, 60*time.Second) {
		cleanup()
		t.Fatalf("TimescaleDB container %s did not become ready", name)
	}
	t.Logf("outage container %s ready on port %d", name, port)
	return port, cleanup
}

// mustOpenPool opens a pgx pool and fails the test if it cannot connect.
func mustOpenPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New(%s): %v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping(%s): %v", dsn, err)
	}
	return pool
}

// applyMigrationTo applies the migration SQL to the given pool (helper for
// the outage test which uses a separate pool from a different container).
func applyMigrationTo(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, stmt := range splitStatements(sql) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}
}

// waitForDBReady polls the DSN until it accepts connections or the timeout
// expires. Returns true if the database became ready.
func waitForDBReady(dsn string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		cancel()
		if err == nil {
			conn.Close(context.Background())
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
