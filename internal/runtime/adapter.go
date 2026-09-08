// Package runtime defines the contract between the collector and the
// per-inference-server adapters.
//
// The split is deliberate. Adapters are pure functions from an exposition
// payload to a partially-filled snapshot: they know metric names, units and
// version quirks, and nothing about scheduling, HTTP, or state. Everything that
// needs memory across scrapes — counter-to-rate conversion, counter-reset
// handling, staleness — lives in the collector, so adapters stay trivially
// testable against a fixture and a new runtime can be added without touching
// the collection loop.
package runtime

import "github.com/p95labs/ifa/internal/telemetry"

// Counter names an inference-agnostic cumulative series. Adapters report raw
// counter values under these keys and the collector converts them to rates,
// which is the only place that can see two consecutive scrapes.
type Counter string

const (
	// CounterGenerationTokens counts decode-phase tokens produced.
	CounterGenerationTokens Counter = "generation_tokens"
	// CounterPromptTokens counts prefill tokens processed.
	CounterPromptTokens Counter = "prompt_tokens"
	// CounterRequestsFinished counts requests that reached a terminal state,
	// successfully or otherwise.
	CounterRequestsFinished Counter = "requests_finished"
	// CounterRequestsFailed counts requests the runtime reports as failed.
	// Runtimes that do not expose a failure counter must omit this rather than
	// reporting zero — the difference decides whether an error-rate rule may
	// fire at all.
	CounterRequestsFailed Counter = "requests_failed"
	// CounterRequestsAborted counts requests terminated before completion,
	// which in practice usually means the client disconnected.
	CounterRequestsAborted Counter = "requests_aborted"
	// CounterPreemptions counts requests evicted mid-flight to reclaim
	// KV-cache blocks.
	CounterPreemptions Counter = "preemptions"
	// CounterPrefixCacheQueries and CounterPrefixCacheHits are token counts,
	// not request counts. Their ratio over an interval is the prefix-cache hit
	// rate for traffic in that interval, which is more useful than the
	// lifetime ratio for spotting a workload whose cache stopped working.
	CounterPrefixCacheQueries Counter = "prefix_cache_queries"
	CounterPrefixCacheHits    Counter = "prefix_cache_hits"
)

// Reading is what an adapter extracts from a single scrape.
type Reading struct {
	// Snapshot holds the instantaneous values: gauges and histogram-derived
	// quantiles. Identity fields (workload, namespace, timestamp) are filled
	// in by the collector, not the adapter.
	Snapshot telemetry.Snapshot

	// Counters holds raw cumulative values. Only keys the target actually
	// exposed are present.
	Counters map[Counter]float64

	// Missing lists expected metric names that were absent from the payload.
	// It is surfaced on the API and as a self-metric so that a partially
	// working integration is visible rather than looking like a healthy
	// workload with quiet numbers.
	Missing []string

	// UnparseableLines counts exposition lines the parser rejected. A
	// consistently non-zero value against a supported runtime means the
	// exposition format moved.
	UnparseableLines int
}

// Adapter maps one inference runtime's telemetry onto the shared model.
//
// Implementations must be safe for concurrent use and must not retain state
// between calls.
type Adapter interface {
	// Runtime returns the identifier used in configuration and in the
	// runtime field of a snapshot.
	Runtime() telemetry.Runtime

	// ExpectedMetrics lists the metric names the adapter reads. It backs the
	// `ifa check` command, which tells an operator which of the metrics this
	// integration needs their server actually exposes — the single most common
	// cause of an integration silently producing nothing.
	ExpectedMetrics() []string

	// Parse extracts a Reading from an exposition payload. modelName, when
	// non-empty, restricts extraction to series for that model; runtimes that
	// serve several models at once need it to avoid mixing them.
	//
	// Parse returns an error only for input it cannot process at all. Missing
	// individual metrics are reported through Reading.Missing.
	Parse(payload string, modelName string) (Reading, error)
}
