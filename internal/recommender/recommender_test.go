package recommender

import (
	"strings"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

var epoch = time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)

func testThresholds() Thresholds {
	t := DefaultThresholds()
	t.SustainFor = 30 * time.Second
	return t
}

// series builds a window of n snapshots at interval, ending at epoch, applying
// mutate to each. Sustained rules need a window, so almost every test uses one.
func series(n int, interval time.Duration, mutate func(*telemetry.Snapshot)) []telemetry.Snapshot {
	out := make([]telemetry.Snapshot, n)
	start := epoch.Add(-time.Duration(n-1) * interval)
	for i := range out {
		s := telemetry.Snapshot{
			Timestamp:    start.Add(time.Duration(i) * interval),
			Namespace:    "inference",
			WorkloadName: "w",
			Runtime:      telemetry.RuntimeVLLM,
		}
		if mutate != nil {
			mutate(&s)
		}
		out[i] = s
	}
	return out
}

// healthySeries is a workload nothing should fire on. Every test that asserts a
// rule fires starts from it, so a rule that fires for the wrong reason shows up
// as the control breaking.
func healthySeries() []telemetry.Snapshot {
	return series(13, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsRunning = telemetry.Observed(6)
		s.RequestsWaiting = telemetry.Observed(1)
		s.WaitingForCapacity = telemetry.Observed(1)
		s.WaitingDeferred = telemetry.Observed(0)
		s.KVCacheUsagePct = telemetry.Observed(55)
		s.GPUUtilizationPct = telemetry.Observed(65)
		s.GPUMemoryUsedPct = telemetry.Observed(70)
		s.PreemptionsPerSec = telemetry.Observed(0)
		s.TTFTP95Ms = telemetry.Observed(180)
		s.QueueTimeP95Ms = telemetry.Observed(10)
		s.P50LatencyMs = telemetry.Observed(900)
		s.P95LatencyMs = telemetry.Observed(2000)
		s.P99LatencyMs = telemetry.Observed(3400)
		s.TokensPerSecond = telemetry.Observed(850)
		s.PromptTokensPerSec = telemetry.Observed(2400)
		s.PrefixCacheHitRatePct = telemetry.Observed(58)
		s.AbortRatePct = telemetry.Observed(0.3)
		s.Replicas = telemetry.Observed(3)
		s.ReadyReplicas = telemetry.Observed(3)
		s.MaxReplicas = telemetry.Observed(10)
	})
}

// analyze runs the engine over one workload's window.
func analyze(t *testing.T, window []telemetry.Snapshot, th Thresholds) []telemetry.Recommendation {
	t.Helper()
	eng := NewEngine(th)
	eng.now = func() time.Time { return epoch }
	latest := window[len(window)-1]
	return eng.Analyze([]telemetry.Snapshot{latest}, func(string) []telemetry.Snapshot { return window })
}

func codes(recs []telemetry.Recommendation) map[string]telemetry.Recommendation {
	out := make(map[string]telemetry.Recommendation, len(recs))
	for _, r := range recs {
		out[r.Code] = r
	}
	return out
}

func assertFired(t *testing.T, recs []telemetry.Recommendation, code Code) telemetry.Recommendation {
	t.Helper()
	rec, ok := codes(recs)[string(code)]
	if !ok {
		t.Fatalf("expected %s to fire; got %v", code, codeList(recs))
	}
	return rec
}

func assertNotFired(t *testing.T, recs []telemetry.Recommendation, code Code) {
	t.Helper()
	if _, ok := codes(recs)[string(code)]; ok {
		t.Errorf("%s fired but should not have; got %v", code, codeList(recs))
	}
}

func codeList(recs []telemetry.Recommendation) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Code)
	}
	return out
}

// ── The control ─────────────────────────────────────────────────────────────

func TestHealthyWorkloadProducesNoFindings(t *testing.T) {
	recs := analyze(t, healthySeries(), testThresholds())
	if len(recs) != 0 {
		for _, r := range recs {
			t.Logf("%s: %s", r.Code, r.Explanation)
		}
		t.Fatalf("expected no findings for a healthy workload, got %d: %v", len(recs), codeList(recs))
	}
}

// A workload with no telemetry at all must not be diagnosed. Every threshold
// comparison against an unmeasured value has to be false, or the tool reports
// problems for runtimes it cannot see.
func TestWorkloadWithNoMeasurementsProducesNoThresholdFindings(t *testing.T) {
	window := series(13, 5*time.Second, nil)
	recs := analyze(t, window, testThresholds())
	for _, r := range recs {
		if r.Code != string(CodeTelemetryStale) && r.Code != string(CodeTelemetryIncomplete) {
			t.Errorf("rule %s fired on entirely absent data: %s", r.Code, r.Explanation)
		}
	}
}

// ── Sustain semantics ───────────────────────────────────────────────────────

// The window a rule evaluates has to genuinely cover the sustain period. An
// earlier implementation compared the span of samples at-or-after the cutoff
// against the sustain window; because the oldest such sample sits just inside
// the boundary, that span was always fractionally short and no sustained rule
// could ever fire.
func TestSustainedFiresWhenHistoryCoversTheWindow(t *testing.T) {
	th := testThresholds()
	th.SustainFor = 30 * time.Second

	// Samples every 7 seconds: none lands exactly on the 30-second boundary.
	window := series(12, 7*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(95)
	})
	recs := analyze(t, window, th)
	assertFired(t, recs, CodeQueueWithSaturatedGPU)
}

func TestSustainedDoesNotFireBeforeTheWindowIsCovered(t *testing.T) {
	th := testThresholds()
	th.SustainFor = 30 * time.Second

	// Only 10 seconds of history: the condition may be real but has not been
	// observed for long enough to distinguish from a spike.
	window := series(3, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(95)
	})
	recs := analyze(t, window, th)
	assertNotFired(t, recs, CodeQueueWithSaturatedGPU)
}

func TestSustainedDoesNotFireOnASpike(t *testing.T) {
	th := testThresholds()
	window := healthySeries()
	// One bad sample in the middle of an otherwise quiet window.
	window[len(window)-3].RequestsWaiting = telemetry.Observed(60)
	window[len(window)-3].GPUUtilizationPct = telemetry.Observed(99)

	recs := analyze(t, window, th)
	assertNotFired(t, recs, CodeQueueWithSaturatedGPU)
}

func TestSustainForZeroEvaluatesTheLatestSampleOnly(t *testing.T) {
	th := testThresholds()
	th.SustainFor = 0

	window := healthySeries()
	last := &window[len(window)-1]
	last.RequestsWaiting = telemetry.Observed(40)
	last.GPUUtilizationPct = telemetry.Observed(95)

	recs := analyze(t, window, th)
	assertFired(t, recs, CodeQueueWithSaturatedGPU)
}

// ── Capacity rules ──────────────────────────────────────────────────────────

// The two queue rules are the reason the engine exists: identical queue depth,
// opposite diagnosis, opposite fix. They must never both fire.
func TestQueueRulesAreMutuallyExclusive(t *testing.T) {
	th := testThresholds()

	saturated := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(95)
	})
	idle := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(9)
	})

	satRecs := analyze(t, saturated, th)
	assertFired(t, satRecs, CodeQueueWithSaturatedGPU)
	assertNotFired(t, satRecs, CodeQueueWithIdleGPU)

	idleRecs := analyze(t, idle, th)
	assertFired(t, idleRecs, CodeQueueWithIdleGPU)
	assertNotFired(t, idleRecs, CodeQueueWithSaturatedGPU)
}

// Without GPU utilisation neither queue rule can say which situation it is, so
// neither may fire. A deployment with no DCGM endpoint is the common case.
func TestQueueRulesStayDormantWithoutGPUUtilisation(t *testing.T) {
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
	})
	recs := analyze(t, window, testThresholds())
	assertNotFired(t, recs, CodeQueueWithSaturatedGPU)
	assertNotFired(t, recs, CodeQueueWithIdleGPU)
}

func TestQueueBoundary(t *testing.T) {
	th := testThresholds()
	th.QueueWaitingRequests = 8

	tests := []struct {
		name    string
		waiting float64
		want    bool
	}{
		{"below threshold", 7, false},
		{"exactly at threshold", 8, false}, // the comparison is strictly greater
		{"above threshold", 9, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
				s.RequestsWaiting = telemetry.Observed(tc.waiting)
				s.GPUUtilizationPct = telemetry.Observed(95)
			})
			recs := analyze(t, window, th)
			_, fired := codes(recs)[string(CodeQueueWithSaturatedGPU)]
			if fired != tc.want {
				t.Errorf("waiting=%v: fired=%v, want %v", tc.waiting, fired, tc.want)
			}
		})
	}
}

func TestQueueGrowthNeedsGrowthNotJustDepth(t *testing.T) {
	th := testThresholds()

	flat := series(18, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(95)
	})
	assertNotFired(t, analyze(t, flat, th), CodeQueueGrowing)

	growing := series(18, 5*time.Second, nil)
	for i := range growing {
		growing[i].RequestsWaiting = telemetry.Observed(float64(10 + i*4))
		growing[i].GPUUtilizationPct = telemetry.Observed(95)
	}
	assertFired(t, analyze(t, growing, th), CodeQueueGrowing)

	// A draining queue is the opposite of a problem, however deep it is.
	draining := series(18, 5*time.Second, nil)
	for i := range draining {
		draining[i].RequestsWaiting = telemetry.Observed(float64(90 - i*4))
		draining[i].GPUUtilizationPct = telemetry.Observed(95)
	}
	assertNotFired(t, analyze(t, draining, th), CodeQueueGrowing)
}

func TestDeferredQueueSupersedesCapacityDiagnosis(t *testing.T) {
	th := testThresholds()
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(30)
		s.GPUUtilizationPct = telemetry.Observed(95)
		// Most of the queue is blocked on something scaling cannot fix.
		s.WaitingDeferred = telemetry.Observed(26)
		s.WaitingForCapacity = telemetry.Observed(4)
	})
	recs := analyze(t, window, th)
	assertFired(t, recs, CodeQueueDeferred)
	assertNotFired(t, recs, CodeQueueWithSaturatedGPU)
}

// ── KV cache ────────────────────────────────────────────────────────────────

func TestPreemptionSupersedesCacheUtilisation(t *testing.T) {
	th := testThresholds()

	// Full cache, no preemption: worth mentioning, not worth waking anyone.
	full := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.KVCacheUsagePct = telemetry.Observed(97)
		s.PreemptionsPerSec = telemetry.Observed(0)
	})
	recs := analyze(t, full, th)
	warn := assertFired(t, recs, CodeKVNearCapacity)
	if warn.Severity != telemetry.SeverityWarning {
		t.Errorf("KV utilisation severity = %s, want warning", warn.Severity)
	}
	assertNotFired(t, recs, CodeKVPreemption)

	// Full cache and preempting: the same utilisation, a different situation.
	preempting := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.KVCacheUsagePct = telemetry.Observed(97)
		s.PreemptionsPerSec = telemetry.Observed(1.4)
	})
	recs = analyze(t, preempting, th)
	crit := assertFired(t, recs, CodeKVPreemption)
	if crit.Severity != telemetry.SeverityCritical {
		t.Errorf("preemption severity = %s, want critical", crit.Severity)
	}
	assertNotFired(t, recs, CodeKVNearCapacity)
}

// ── Latency ─────────────────────────────────────────────────────────────────

func TestTTFTSplitsByWhereTheTimeGoes(t *testing.T) {
	th := testThresholds()
	th.TTFTP95Ms = 1000
	th.QueueShareOfTTFTPct = 50

	tests := []struct {
		name    string
		ttft    float64
		queue   float64
		want    Code
		notWant Code
	}{
		{"waiting dominates", 4000, 3600, CodeTTFTAdmissionBound, CodeTTFTPrefillBound},
		{"prefill dominates", 4000, 200, CodeTTFTPrefillBound, CodeTTFTAdmissionBound},
		{"exactly at the split", 4000, 2000, CodeTTFTAdmissionBound, CodeTTFTPrefillBound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
				s.TTFTP95Ms = telemetry.Observed(tc.ttft)
				s.QueueTimeP95Ms = telemetry.Observed(tc.queue)
			})
			recs := analyze(t, window, th)
			assertFired(t, recs, tc.want)
			assertNotFired(t, recs, tc.notWant)
		})
	}
}

// The two percentiles come from separate histograms with separate bucket
// boundaries, so interpolation error can make queue time exceed TTFT. The rule
// must still produce sensible output rather than "117% of it" and a negative
// prefill time.
func TestTTFTSplitSurvivesQueueTimeExceedingTTFT(t *testing.T) {
	th := testThresholds()
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.TTFTP95Ms = telemetry.Observed(6900)
		s.QueueTimeP95Ms = telemetry.Observed(8100)
	})
	rec := assertFired(t, analyze(t, window, th), CodeTTFTAdmissionBound)

	if strings.Contains(rec.Explanation, "-") && strings.Contains(rec.Explanation, "ms") {
		t.Errorf("explanation reports a negative duration: %s", rec.Explanation)
	}
	for _, e := range rec.Evidence {
		if e.Metric == "queue_time_share_of_ttft" && e.Observed > 100 {
			t.Errorf("queue share reported as %.1f%%, which is not a share", e.Observed)
		}
	}
}

// Without queue-time telemetry the engine cannot attribute a slow TTFT, so it
// must not guess. Older vLLM builds do not expose request_queue_time_seconds.
func TestTTFTRulesStayDormantWithoutQueueTime(t *testing.T) {
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.TTFTP95Ms = telemetry.Observed(9000)
	})
	recs := analyze(t, window, testThresholds())
	assertNotFired(t, recs, CodeTTFTAdmissionBound)
	assertNotFired(t, recs, CodeTTFTPrefillBound)
}

func TestSpecificLatencyDiagnosisSupersedesTheGenericOne(t *testing.T) {
	th := testThresholds()
	th.E2EP95Ms = 1000

	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.P95LatencyMs = telemetry.Observed(30000)
		s.TTFTP95Ms = telemetry.Observed(9000)
		s.QueueTimeP95Ms = telemetry.Observed(8500)
	})
	recs := analyze(t, window, th)
	assertFired(t, recs, CodeTTFTAdmissionBound)
	assertNotFired(t, recs, CodeE2ELatencyHigh)
}

func TestGenericLatencyRuleFiresWhenNothingElseExplainsIt(t *testing.T) {
	th := testThresholds()
	th.E2EP95Ms = 1000

	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.P95LatencyMs = telemetry.Observed(4000)
		s.P99LatencyMs = telemetry.Observed(5000)
		s.TTFTP95Ms = telemetry.Observed(120) // fast to first token
		s.QueueTimeP95Ms = telemetry.Observed(5)
	})
	assertFired(t, analyze(t, window, th), CodeE2ELatencyHigh)
}

func TestTailRatio(t *testing.T) {
	th := testThresholds()
	th.TailRatioP99P95 = 3

	tests := []struct {
		name     string
		p95, p99 float64
		want     bool
	}{
		{"tight tail", 1000, 1500, false},
		{"exactly at the ratio", 1000, 3000, false},
		{"wide tail", 1000, 4200, true},
		{"zero p95 is not a ratio", 0, 5000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
				s.P95LatencyMs = telemetry.Observed(tc.p95)
				s.P99LatencyMs = telemetry.Observed(tc.p99)
			})
			recs := analyze(t, window, th)
			_, fired := codes(recs)[string(CodeTailLatencyGap)]
			if fired != tc.want {
				t.Errorf("p95=%v p99=%v: fired=%v, want %v", tc.p95, tc.p99, fired, tc.want)
			}
		})
	}
}

// ── Efficiency ──────────────────────────────────────────────────────────────

func TestPrefixCacheRuleIgnoresWorkloadsWithNegligiblePrefill(t *testing.T) {
	th := testThresholds()

	// A near-idle workload has a meaningless hit rate; dividing tiny counter
	// deltas produces noise, not a finding.
	quiet := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.PrefixCacheHitRatePct = telemetry.Observed(0)
		s.PromptTokensPerSec = telemetry.Observed(3)
	})
	assertNotFired(t, analyze(t, quiet, th), CodePrefixCacheIneffective)

	busy := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.PrefixCacheHitRatePct = telemetry.Observed(1.5)
		s.PromptTokensPerSec = telemetry.Observed(9000)
	})
	assertFired(t, analyze(t, busy, th), CodePrefixCacheIneffective)
}

func TestLowThroughputNeedsBothSignals(t *testing.T) {
	th := testThresholds()

	// Low throughput with an idle GPU is just a quiet workload.
	idle := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.GPUUtilizationPct = telemetry.Observed(10)
		s.TokensPerSecond = telemetry.Observed(5)
	})
	assertNotFired(t, analyze(t, idle, th), CodeLowTokenThroughput)

	busy := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.GPUUtilizationPct = telemetry.Observed(95)
		s.TokensPerSecond = telemetry.Observed(40)
	})
	assertFired(t, analyze(t, busy, th), CodeLowTokenThroughput)
}

// ── Scaling ─────────────────────────────────────────────────────────────────

func TestReplicaCeiling(t *testing.T) {
	th := testThresholds()

	atCeiling := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(30)
		s.GPUUtilizationPct = telemetry.Observed(95)
		s.Replicas = telemetry.Observed(6)
		s.ReadyReplicas = telemetry.Observed(6)
		s.MaxReplicas = telemetry.Observed(6)
	})
	recs := analyze(t, atCeiling, th)
	assertFired(t, recs, CodeAtReplicaCeiling)
	// At the ceiling, "add replicas" is not available advice, so the generic
	// capacity finding is suppressed in favour of the specific one.
	assertNotFired(t, recs, CodeQueueWithSaturatedGPU)

	headroom := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(30)
		s.GPUUtilizationPct = telemetry.Observed(95)
		s.Replicas = telemetry.Observed(3)
		s.ReadyReplicas = telemetry.Observed(3)
		s.MaxReplicas = telemetry.Observed(12)
	})
	recs = analyze(t, headroom, th)
	assertNotFired(t, recs, CodeAtReplicaCeiling)
	assertFired(t, recs, CodeQueueWithSaturatedGPU)
}

func TestReplicaRulesStayDormantWithoutKubernetesContext(t *testing.T) {
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(30)
		s.GPUUtilizationPct = telemetry.Observed(95)
	})
	recs := analyze(t, window, testThresholds())
	assertNotFired(t, recs, CodeAtReplicaCeiling)
	assertNotFired(t, recs, CodeReplicasNotReady)
}

func TestReplicasNotReady(t *testing.T) {
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(30)
		s.Replicas = telemetry.Observed(6)
		s.ReadyReplicas = telemetry.Observed(2)
	})
	assertFired(t, analyze(t, window, testThresholds()), CodeReplicasNotReady)
}

// ── Observability of the workload ───────────────────────────────────────────

func TestStaleTelemetry(t *testing.T) {
	th := testThresholds()
	th.StaleAfter = time.Minute

	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.KVCacheUsagePct = telemetry.Observed(50)
	})
	// Shift the whole window into the past.
	for i := range window {
		window[i].Timestamp = window[i].Timestamp.Add(-10 * time.Minute)
	}
	assertFired(t, analyze(t, window, th), CodeTelemetryStale)

	fresh := healthySeries()
	assertNotFired(t, analyze(t, fresh, th), CodeTelemetryStale)
}

func TestIncompleteTelemetryIsReported(t *testing.T) {
	window := healthySeries()
	window[len(window)-1].MetricsMissing = []string{"vllm:kv_cache_usage_perc"}
	rec := assertFired(t, analyze(t, window, testThresholds()), CodeTelemetryIncomplete)
	if rec.Severity != telemetry.SeverityInfo {
		t.Errorf("severity = %s, want info", rec.Severity)
	}
}

// ── Engine behaviour ────────────────────────────────────────────────────────

// Recommendation IDs are what clients deduplicate on. They must depend on the
// rule and the workload, not on how many workloads happen to be in the fleet or
// on the order the engine walked them.
func TestRecommendationIDsAreStableAndUnique(t *testing.T) {
	th := testThresholds()
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(95)
		s.KVCacheUsagePct = telemetry.Observed(99)
	})

	first := analyze(t, window, th)
	second := analyze(t, window, th)
	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("unstable finding count: %d then %d", len(first), len(second))
	}

	seen := map[string]bool{}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("ID changed between runs: %q then %q", first[i].ID, second[i].ID)
		}
		if seen[first[i].ID] {
			t.Errorf("duplicate ID %q", first[i].ID)
		}
		seen[first[i].ID] = true
		if first[i].ID != first[i].Code+":inference/w" {
			t.Errorf("ID %q is not derived from code and workload", first[i].ID)
		}
	}
}

func TestOutputIsOrderedBySeverityThenCode(t *testing.T) {
	th := testThresholds()
	window := series(18, 5*time.Second, nil)
	for i := range window {
		window[i].RequestsWaiting = telemetry.Observed(float64(10 + i*4))
		window[i].GPUUtilizationPct = telemetry.Observed(95)
		window[i].KVCacheUsagePct = telemetry.Observed(99)
		window[i].P95LatencyMs = telemetry.Observed(1000)
		window[i].P99LatencyMs = telemetry.Observed(9000)
	}

	recs := analyze(t, window, th)
	if len(recs) < 3 {
		t.Fatalf("expected several findings, got %v", codeList(recs))
	}
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		if cur.Severity.Rank() < prev.Severity.Rank() {
			t.Fatalf("severity out of order at %d: %s after %s", i, cur.Severity, prev.Severity)
		}
		if cur.Severity == prev.Severity && cur.Code < prev.Code {
			t.Fatalf("codes out of order within a severity: %s after %s", cur.Code, prev.Code)
		}
	}
}

// Every finding has to carry the numbers behind it, or it is an alert rather
// than a diagnosis.
func TestEveryFindingCarriesEvidenceAndAdvice(t *testing.T) {
	th := testThresholds()
	windows := [][]telemetry.Snapshot{
		series(18, 5*time.Second, func(s *telemetry.Snapshot) {
			s.RequestsWaiting = telemetry.Observed(40)
			s.GPUUtilizationPct = telemetry.Observed(95)
			s.KVCacheUsagePct = telemetry.Observed(99)
			s.PreemptionsPerSec = telemetry.Observed(2)
			s.GPUMemoryUsedPct = telemetry.Observed(99)
			s.TTFTP95Ms = telemetry.Observed(8000)
			s.QueueTimeP95Ms = telemetry.Observed(7000)
			s.P95LatencyMs = telemetry.Observed(20000)
			s.P99LatencyMs = telemetry.Observed(90000)
			s.TokensPerSecond = telemetry.Observed(10)
			s.PromptTokensPerSec = telemetry.Observed(9000)
			s.PrefixCacheHitRatePct = telemetry.Observed(1)
			s.ErrorRatePct = telemetry.Observed(20)
			s.AbortRatePct = telemetry.Observed(30)
			s.Replicas = telemetry.Observed(4)
			s.ReadyReplicas = telemetry.Observed(1)
			s.MaxReplicas = telemetry.Observed(4)
			s.MetricsMissing = []string{"something"}
		}),
	}

	fired := map[string]bool{}
	for _, w := range windows {
		for _, rec := range analyze(t, w, th) {
			fired[rec.Code] = true
			if rec.Explanation == "" {
				t.Errorf("%s has no explanation", rec.Code)
			}
			if rec.SuggestedAction == "" {
				t.Errorf("%s suggests no action", rec.Code)
			}
			if len(rec.Evidence) == 0 {
				t.Errorf("%s carries no evidence", rec.Code)
			}
			if rec.ObservedAt.IsZero() {
				t.Errorf("%s has no observation time", rec.Code)
			}
			for _, e := range rec.Evidence {
				if e.Metric == "" {
					t.Errorf("%s has an unnamed evidence entry", rec.Code)
				}
			}
		}
	}
	if len(fired) < 5 {
		t.Errorf("expected this pathological workload to trigger several rules, got %v", fired)
	}
}

// Rule metadata backs the API catalogue and the docs, so it has to be complete
// and internally consistent.
func TestRuleCatalogueIsWellFormed(t *testing.T) {
	eng := NewEngine(DefaultThresholds())
	rules := eng.Rules()
	if len(rules) < 10 {
		t.Fatalf("expected a substantial rule set, got %d", len(rules))
	}

	known := map[Code]bool{}
	for _, r := range rules {
		known[r.Code] = true
	}

	seen := map[Code]bool{}
	for _, r := range rules {
		if seen[r.Code] {
			t.Errorf("duplicate rule code %s", r.Code)
		}
		seen[r.Code] = true

		if r.Title == "" || r.Summary == "" {
			t.Errorf("%s is missing a title or summary", r.Code)
		}
		if r.Eval == nil {
			t.Errorf("%s has no evaluator", r.Code)
		}
		switch r.Severity {
		case telemetry.SeverityInfo, telemetry.SeverityWarning, telemetry.SeverityCritical:
		default:
			t.Errorf("%s has severity %q", r.Code, r.Severity)
		}
		for _, sup := range r.Supersedes {
			if !known[sup] {
				t.Errorf("%s supersedes unknown code %s", r.Code, sup)
			}
			if sup == r.Code {
				t.Errorf("%s supersedes itself", r.Code)
			}
		}
	}
}

func TestRuntimeScopedRulesDoNotFireForOtherRuntimes(t *testing.T) {
	th := testThresholds()
	window := series(12, 5*time.Second, func(s *telemetry.Snapshot) {
		s.Runtime = telemetry.RuntimeTriton
		s.PrefixCacheHitRatePct = telemetry.Observed(1)
		s.PromptTokensPerSec = telemetry.Observed(9000)
		s.RequestsWaiting = telemetry.Observed(30)
		s.WaitingDeferred = telemetry.Observed(26)
		s.WaitingForCapacity = telemetry.Observed(4)
	})
	recs := analyze(t, window, th)
	assertNotFired(t, recs, CodePrefixCacheIneffective)
	assertNotFired(t, recs, CodeQueueDeferred)
}

func TestAnalyzeWithoutHistoryDoesNotPanic(t *testing.T) {
	eng := NewEngine(DefaultThresholds())
	snap := telemetry.Snapshot{Timestamp: time.Now(), WorkloadName: "w", Runtime: telemetry.RuntimeVLLM}
	if recs := eng.Analyze([]telemetry.Snapshot{snap}, nil); recs == nil {
		t.Error("Analyze returned nil rather than an empty slice")
	}
	if recs := eng.Analyze(nil, nil); len(recs) != 0 {
		t.Errorf("Analyze on no input returned %d findings", len(recs))
	}
}

func BenchmarkAnalyze(b *testing.B) {
	eng := NewEngine(DefaultThresholds())
	window := series(120, time.Second, func(s *telemetry.Snapshot) {
		s.RequestsWaiting = telemetry.Observed(40)
		s.GPUUtilizationPct = telemetry.Observed(95)
		s.KVCacheUsagePct = telemetry.Observed(97)
		s.TTFTP95Ms = telemetry.Observed(3000)
		s.QueueTimeP95Ms = telemetry.Observed(2500)
	})
	latest := make([]telemetry.Snapshot, 0, 50)
	for i := 0; i < 50; i++ {
		s := window[len(window)-1]
		s.WorkloadName = "w" + string(rune('a'+i%26))
		latest = append(latest, s)
	}
	lookup := func(string) []telemetry.Snapshot { return window }

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if len(eng.Analyze(latest, lookup)) == 0 {
			b.Fatal("no findings")
		}
	}
}
