package recommender

import (
	"fmt"
	"strings"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

// Rule codes. The prefix names the family:
//
//	CAP  capacity and scheduling
//	KV   KV-cache pressure
//	LAT  latency
//	EFF  efficiency
//	ERR  errors
//	SCL  scaling
//	OBS  observability of the workload itself
const (
	CodeQueueWithSaturatedGPU Code = "IFA-CAP-001"
	CodeQueueWithIdleGPU      Code = "IFA-CAP-002"
	CodeQueueGrowing          Code = "IFA-CAP-003"
	CodeQueueDeferred         Code = "IFA-CAP-004"

	CodeKVPreemption   Code = "IFA-KV-001"
	CodeKVNearCapacity Code = "IFA-KV-002"

	CodeTTFTAdmissionBound Code = "IFA-LAT-001"
	CodeTTFTPrefillBound   Code = "IFA-LAT-002"
	CodeE2ELatencyHigh     Code = "IFA-LAT-003"
	CodeTailLatencyGap     Code = "IFA-LAT-004"

	CodePrefixCacheIneffective Code = "IFA-EFF-001"
	CodeLowTokenThroughput     Code = "IFA-EFF-002"
	CodeGPUMemoryPressure      Code = "IFA-EFF-003"

	CodeErrorRateHigh Code = "IFA-ERR-001"
	CodeClientAborts  Code = "IFA-ERR-002"

	CodeAtReplicaCeiling Code = "IFA-SCL-001"
	CodeReplicasNotReady Code = "IFA-SCL-002"

	CodeTelemetryIncomplete Code = "IFA-OBS-001"
	CodeTelemetryStale      Code = "IFA-OBS-002"
)

// sourceMetric maps a snapshot field to the runtime metric it came from, so
// evidence can point at something an operator can query themselves.
func sourceMetric(rt telemetry.Runtime, field string) string {
	switch rt {
	case telemetry.RuntimeVLLM:
		switch field {
		case "requests_waiting":
			return "vllm:num_requests_waiting"
		case "requests_running":
			return "vllm:num_requests_running"
		case "waiting_for_capacity", "waiting_deferred":
			return "vllm:num_requests_waiting_by_reason"
		case "kv_cache_usage_percent":
			return "vllm:kv_cache_usage_perc"
		case "preemptions_per_sec":
			return "vllm:num_preemptions_total"
		case "ttft_p95_ms", "ttft_p50_ms":
			return "vllm:time_to_first_token_seconds"
		case "p95_latency_ms", "p99_latency_ms":
			return "vllm:e2e_request_latency_seconds"
		case "queue_time_p95_ms":
			return "vllm:request_queue_time_seconds"
		case "tokens_per_second":
			return "vllm:generation_tokens_total"
		case "prefix_cache_hit_rate_percent":
			return "vllm:prefix_cache_hits_total / vllm:prefix_cache_queries_total"
		}
	case telemetry.RuntimeTriton:
		switch field {
		case "requests_waiting":
			return "nv_inference_pending_request_count"
		case "gpu_utilization_percent":
			return "nv_gpu_utilization"
		case "gpu_memory_used_percent":
			return "nv_gpu_memory_used_bytes / nv_gpu_memory_total_bytes"
		case "error_rate_percent":
			return "nv_inference_request_failure / nv_inference_request_success"
		case "queue_time_p95_ms":
			return "nv_inference_queue_duration_us"
		}
	}
	return ""
}

// DefaultRules returns the built-in rule set.
//
// Rules are written to be readable end to end: the condition, the explanation
// an operator reads, and the evidence that backs it are in one place. Splitting
// them across a framework would make the set look tidier and each rule harder
// to audit.
func DefaultRules() []Rule {
	return []Rule{
		queueWithSaturatedGPU(),
		queueWithIdleGPU(),
		queueGrowing(),
		queueDeferred(),
		kvPreemption(),
		kvNearCapacity(),
		ttftAdmissionBound(),
		ttftPrefillBound(),
		e2eLatencyHigh(),
		tailLatencyGap(),
		prefixCacheIneffective(),
		lowTokenThroughput(),
		gpuMemoryPressure(),
		errorRateHigh(),
		clientAborts(),
		atReplicaCeiling(),
		replicasNotReady(),
		telemetryIncomplete(),
		telemetryStale(),
	}
}

// ── Capacity ────────────────────────────────────────────────────────────────

func queueWithSaturatedGPU() Rule {
	return Rule{
		Code:     CodeQueueWithSaturatedGPU,
		Title:    "Requests queueing while the GPU is saturated",
		Severity: telemetry.SeverityWarning,
		Summary: "Waiting requests and high GPU utilisation held together for the sustain window: " +
			"the workload is out of compute, not misconfigured.",
		Supersedes: []Code{CodeE2ELatencyHigh},
		Eval: func(e *Eval) *Finding {
			t := e.T
			ok := e.Sustained(func(s telemetry.Snapshot) bool {
				return s.RequestsWaiting.Above(t.QueueWaitingRequests) &&
					s.GPUUtilizationPct.Above(t.GPUUtilHighPct)
			})
			if !ok {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.0f requests have been waiting with the GPU above %.0f%% utilisation for %s. "+
						"The accelerator is doing useful work and still cannot keep up, so raising "+
						"concurrency on this replica will lengthen the queue rather than drain it.",
					s.RequestsWaiting.Value, t.GPUUtilHighPct, roundDur(e.Span())),
				Action: "Add replicas, or reduce the work per request — a smaller or quantised model, " +
					"or shorter generations. Check whether an autoscaler is already trying and blocked " +
					"(see IFA-SCL-001 and IFA-SCL-002).",
				Evidence: []telemetry.Evidence{
					evidence("requests_waiting", sourceMetric(s.Runtime, "requests_waiting"),
						s.RequestsWaiting.Value, t.QueueWaitingRequests, ">", "requests"),
					evidence("gpu_utilization_percent", sourceMetric(s.Runtime, "gpu_utilization_percent"),
						s.GPUUtilizationPct.Value, t.GPUUtilHighPct, ">", "percent"),
					observation("requests_running", sourceMetric(s.Runtime, "requests_running"),
						s.RequestsRunning.Or(0), "requests"),
				},
			}
		},
	}
}

func queueWithIdleGPU() Rule {
	return Rule{
		Code:     CodeQueueWithIdleGPU,
		Title:    "Requests queueing while the GPU is idle",
		Severity: telemetry.SeverityWarning,
		Summary: "Waiting requests with low GPU utilisation: work is arriving but not reaching the " +
			"accelerator, which points at batching or admission limits rather than capacity.",
		Supersedes: []Code{CodeE2ELatencyHigh},
		Eval: func(e *Eval) *Finding {
			t := e.T
			ok := e.Sustained(func(s telemetry.Snapshot) bool {
				return s.RequestsWaiting.Above(t.QueueWaitingRequests) &&
					s.GPUUtilizationPct.Below(t.GPUUtilLowPct)
			})
			if !ok {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.0f requests have been waiting for %s while GPU utilisation stayed below %.0f%%. "+
						"The accelerator has capacity that the scheduler is not handing work to — "+
						"typically a concurrency cap (max_num_seqs), a batch-size limit, or a "+
						"per-request memory reservation that is far larger than the requests need.",
					s.RequestsWaiting.Value, roundDur(e.Span()), t.GPUUtilLowPct),
				Action: "Raise the runtime's concurrency limit (max_num_seqs for vLLM) or its batch size, " +
					"and check max_model_len — an oversized value reserves KV blocks per sequence and " +
					"caps how many can run at once. Adding replicas will not help here.",
				Evidence: []telemetry.Evidence{
					evidence("requests_waiting", sourceMetric(s.Runtime, "requests_waiting"),
						s.RequestsWaiting.Value, t.QueueWaitingRequests, ">", "requests"),
					evidence("gpu_utilization_percent", sourceMetric(s.Runtime, "gpu_utilization_percent"),
						s.GPUUtilizationPct.Value, t.GPUUtilLowPct, "<", "percent"),
					observation("kv_cache_usage_percent", sourceMetric(s.Runtime, "kv_cache_usage_percent"),
						s.KVCacheUsagePct.Or(0), "percent"),
				},
			}
		},
	}
}

func queueGrowing() Rule {
	return Rule{
		Code:     CodeQueueGrowing,
		Title:    "Queue is growing, not just deep",
		Severity: telemetry.SeverityCritical,
		Summary: "The waiting queue is larger at the end of the window than at the start: arrivals are " +
			"outpacing completions and the backlog will keep building until traffic drops.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			delta, ok := e.Trend(func(s telemetry.Snapshot) telemetry.Metric { return s.RequestsWaiting })
			if !ok {
				return nil
			}
			// A deep-but-flat queue is a steady state; a growing one is not.
			// Requiring both depth and growth keeps this off workloads that
			// simply run with a few requests buffered.
			if !e.Latest.RequestsWaiting.Above(t.QueueWaitingRequests) || delta <= 1 {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"The waiting queue grew by %.1f requests across the last %s and now stands at %.0f. "+
						"Unlike a deep but stable queue, this does not reach equilibrium on its own: "+
						"queueing delay will keep rising for as long as the trend holds.",
					delta, roundDur(e.Span()), s.RequestsWaiting.Value),
				Action: "Treat as a capacity shortfall and shed or scale now rather than waiting for a " +
					"latency alert. Check the companion findings for this workload to see whether the " +
					"bottleneck is compute, KV cache, or admission.",
				Evidence: []telemetry.Evidence{
					observation("requests_waiting", sourceMetric(s.Runtime, "requests_waiting"),
						s.RequestsWaiting.Value, "requests"),
					evidence("requests_waiting_trend", "", delta, 1, ">", "requests per window"),
				},
			}
		},
	}
}

func queueDeferred() Rule {
	return Rule{
		Code:     CodeQueueDeferred,
		Title:    "Waiting requests are deferred, not capacity-bound",
		Severity: telemetry.SeverityWarning,
		Runtimes: []telemetry.Runtime{telemetry.RuntimeVLLM},
		Summary: "Most waiting requests are blocked on a transient constraint rather than on scheduling " +
			"capacity, so adding replicas will not shorten the queue.",
		Supersedes: []Code{CodeQueueWithSaturatedGPU, CodeQueueWithIdleGPU},
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.WaitingDeferred.OK || !s.WaitingForCapacity.OK {
				return nil
			}
			total := s.WaitingDeferred.Value + s.WaitingForCapacity.Value
			if total <= t.QueueWaitingRequests || s.WaitingDeferred.Value <= s.WaitingForCapacity.Value {
				return nil
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.0f of %.0f waiting requests are deferred rather than waiting for capacity. "+
						"vLLM defers a request when something other than scheduler space blocks it — "+
						"a LoRA-adapter budget, a KV transfer in flight, or a blocked request state. "+
						"Scaling out will not move these.",
					s.WaitingDeferred.Value, total),
				Action: "Check the LoRA adapter budget (--max-loras / --max-cpu-loras) and any KV-transfer " +
					"or disaggregated-prefill configuration before changing replica counts.",
				Evidence: []telemetry.Evidence{
					observation("waiting_deferred", sourceMetric(s.Runtime, "waiting_deferred"),
						s.WaitingDeferred.Value, "requests"),
					observation("waiting_for_capacity", sourceMetric(s.Runtime, "waiting_for_capacity"),
						s.WaitingForCapacity.Value, "requests"),
				},
			}
		},
	}
}

// ── KV cache ────────────────────────────────────────────────────────────────

func kvPreemption() Rule {
	return Rule{
		Code:     CodeKVPreemption,
		Title:    "KV cache exhausted: requests are being preempted",
		Severity: telemetry.SeverityCritical,
		Summary: "The runtime is evicting in-flight requests to reclaim KV-cache blocks. Preempted work " +
			"is recomputed from scratch, so this both wastes GPU time and adds latency.",
		Supersedes: []Code{CodeKVNearCapacity},
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.PreemptionsPerSec.Above(t.PreemptionsPerSec) {
				return nil
			}
			ev := []telemetry.Evidence{
				evidence("preemptions_per_sec", sourceMetric(s.Runtime, "preemptions_per_sec"),
					s.PreemptionsPerSec.Value, t.PreemptionsPerSec, ">", "per second"),
			}
			if s.KVCacheUsagePct.OK {
				ev = append(ev, observation("kv_cache_usage_percent",
					sourceMetric(s.Runtime, "kv_cache_usage_percent"), s.KVCacheUsagePct.Value, "percent"))
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"The engine is preempting %.3f requests per second to free KV-cache blocks. "+
						"This is the symptom that KV-cache utilisation only hints at: a workload can sit "+
						"at 95%% cache occupancy indefinitely without harm, but a non-zero preemption rate "+
						"means generated tokens are being thrown away and recomputed.",
					s.PreemptionsPerSec.Value),
				Action: "Give the cache more room or ask less of it: raise gpu_memory_utilization, lower " +
					"max_num_seqs so fewer sequences compete for blocks, reduce max_model_len if it is " +
					"set well above real prompt lengths, or move to a GPU with more HBM.",
				Evidence: ev,
			}
		},
	}
}

func kvNearCapacity() Rule {
	return Rule{
		Code:     CodeKVNearCapacity,
		Title:    "KV cache near capacity",
		Severity: telemetry.SeverityWarning,
		Summary: "KV-cache occupancy has stayed high for the sustain window. Not a fault on its own — " +
			"it means the cache is being used — but it leaves no headroom for a traffic spike.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			ok := e.Sustained(func(s telemetry.Snapshot) bool {
				return s.KVCacheUsagePct.Above(t.KVCacheHighPct)
			})
			if !ok {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"KV-cache occupancy has been above %.0f%% for %s, currently %.1f%%. No requests have "+
						"been preempted yet, so this is headroom rather than harm: a burst of long prompts "+
						"would start evicting.",
					t.KVCacheHighPct, roundDur(e.Span()), s.KVCacheUsagePct.Value),
				Action: "No action needed if traffic is steady. If bursts are expected, lower max_num_seqs " +
					"or raise gpu_memory_utilization to build headroom before preemption starts.",
				Evidence: []telemetry.Evidence{
					evidence("kv_cache_usage_percent", sourceMetric(s.Runtime, "kv_cache_usage_percent"),
						s.KVCacheUsagePct.Value, t.KVCacheHighPct, ">", "percent"),
				},
			}
		},
	}
}

// ── Latency ─────────────────────────────────────────────────────────────────

func ttftAdmissionBound() Rule {
	return Rule{
		Code:     CodeTTFTAdmissionBound,
		Title:    "Time to first token dominated by queueing",
		Severity: telemetry.SeverityWarning,
		Summary: "TTFT is above target and most of it is time spent waiting to be admitted rather than " +
			"time spent on prefill — a capacity problem wearing a latency costume.",
		Supersedes: []Code{CodeE2ELatencyHigh},
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			share, ok := queueShareOfTTFT(s)
			if !ok || !s.TTFTP95Ms.Above(t.TTFTP95Ms) || share < t.QueueShareOfTTFTPct {
				return nil
			}
			prefill := s.TTFTP95Ms.Value - s.QueueTimeP95Ms.Value
			remainder := fmt.Sprintf("Prefill accounts for roughly the remaining %.0fms", prefill)
			if prefill <= 0 {
				// The two percentiles come from separate histograms with
				// separate bucket boundaries, so the split is an estimate and
				// the arithmetic can cross over. Say what is known rather than
				// reporting a negative prefill time.
				remainder = "Queueing accounts for essentially all of it"
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"p95 TTFT is %.0fms against a %.0fms target, and about %.0f%% of it (%.0fms) is queue "+
						"time before the scheduler admits the request. %s, so tuning prompt handling "+
						"would barely move the number a user sees.",
					s.TTFTP95Ms.Value, t.TTFTP95Ms, share, s.QueueTimeP95Ms.Value, remainder),
				Action: "Add serving capacity, or admit requests sooner by raising the concurrency limit if " +
					"the GPU has headroom. Routing long prompts to a separate pool also helps, because they " +
					"are what holds admission slots open.",
				Evidence: []telemetry.Evidence{
					evidence("ttft_p95_ms", sourceMetric(s.Runtime, "ttft_p95_ms"),
						s.TTFTP95Ms.Value, t.TTFTP95Ms, ">", "ms"),
					evidence("queue_time_share_of_ttft", "", share, t.QueueShareOfTTFTPct, ">=", "percent"),
					observation("queue_time_p95_ms", sourceMetric(s.Runtime, "queue_time_p95_ms"),
						s.QueueTimeP95Ms.Value, "ms"),
				},
			}
		},
	}
}

func ttftPrefillBound() Rule {
	return Rule{
		Code:     CodeTTFTPrefillBound,
		Title:    "Time to first token dominated by prefill",
		Severity: telemetry.SeverityWarning,
		Summary: "TTFT is above target but requests are admitted promptly, so the cost is in processing " +
			"the prompt itself rather than in waiting.",
		Supersedes: []Code{CodeE2ELatencyHigh},
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			share, ok := queueShareOfTTFT(s)
			if !ok || !s.TTFTP95Ms.Above(t.TTFTP95Ms) || share >= t.QueueShareOfTTFTPct {
				return nil
			}
			ev := []telemetry.Evidence{
				evidence("ttft_p95_ms", sourceMetric(s.Runtime, "ttft_p95_ms"),
					s.TTFTP95Ms.Value, t.TTFTP95Ms, ">", "ms"),
				evidence("queue_time_share_of_ttft", "", share, t.QueueShareOfTTFTPct, "<", "percent"),
			}
			if s.PrefixCacheHitRatePct.OK {
				ev = append(ev, observation("prefix_cache_hit_rate_percent",
					sourceMetric(s.Runtime, "prefix_cache_hit_rate_percent"),
					s.PrefixCacheHitRatePct.Value, "percent"))
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"p95 TTFT is %.0fms against a %.0fms target, but only about %.0f%% of that is queue time. "+
						"Requests are being admitted promptly and then spending the time in prefill, which "+
						"points at prompt length or prefill configuration rather than capacity.",
					s.TTFTP95Ms.Value, t.TTFTP95Ms, share),
				Action: "Enable chunked prefill so long prompts stop blocking the batch, and check that " +
					"prefix caching is on and actually hitting. Adding replicas will not reduce the cost of " +
					"processing a long prompt.",
				Evidence: ev,
			}
		},
	}
}

func e2eLatencyHigh() Rule {
	return Rule{
		Code:     CodeE2ELatencyHigh,
		Title:    "End-to-end p95 latency above target",
		Severity: telemetry.SeverityWarning,
		Summary: "A symptom-level rule: request latency is above target and no more specific diagnosis " +
			"applied. When a capacity or prefill rule fires for the same workload it supersedes this one.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			ok := e.Sustained(func(s telemetry.Snapshot) bool {
				return s.P95LatencyMs.Above(t.E2EP95Ms)
			})
			if !ok {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"p95 end-to-end latency has been above %.0fms for %s, currently %.0fms. No queueing, "+
						"cache-pressure or throughput rule fired alongside it, so the cause is not one this "+
						"rule set recognises from the runtime's own metrics.",
					t.E2EP95Ms, roundDur(e.Span()), s.P95LatencyMs.Value),
				Action: "Compare generation length and prompt length against what the target assumes — " +
					"a rise in output tokens per request raises end-to-end latency without any resource " +
					"looking unhealthy. Check downstream dependencies if requests fan out.",
				Evidence: []telemetry.Evidence{
					evidence("p95_latency_ms", sourceMetric(s.Runtime, "p95_latency_ms"),
						s.P95LatencyMs.Value, t.E2EP95Ms, ">", "ms"),
					observation("tokens_per_second", sourceMetric(s.Runtime, "tokens_per_second"),
						s.TokensPerSecond.Or(0), "tokens per second"),
				},
			}
		},
	}
}

func tailLatencyGap() Rule {
	return Rule{
		Code:     CodeTailLatencyGap,
		Title:    "Tail latency far above p95",
		Severity: telemetry.SeverityInfo,
		Summary: "p99 is several times p95, which means a small share of requests behaves very differently " +
			"from the rest — usually long prompts sharing a batch with short ones.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.P95LatencyMs.OK || !s.P99LatencyMs.OK || s.P95LatencyMs.Value <= 0 {
				return nil
			}
			ratio := s.P99LatencyMs.Value / s.P95LatencyMs.Value
			if ratio <= t.TailRatioP99P95 {
				return nil
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"p99 latency (%.0fms) is %.1f× p95 (%.0fms). A gap that wide is a mixture, not a "+
						"distribution: a minority of requests is doing substantially more work, or is being "+
						"held behind requests that are.",
					s.P99LatencyMs.Value, ratio, s.P95LatencyMs.Value),
				Action: "Look at the request-size distribution before tuning anything. If long-context " +
					"requests are the minority, routing them to a separate deployment removes the " +
					"head-of-line blocking without changing capacity.",
				Evidence: []telemetry.Evidence{
					evidence("p99_over_p95_ratio", "", ratio, t.TailRatioP99P95, ">", "ratio"),
					observation("p95_latency_ms", sourceMetric(s.Runtime, "p95_latency_ms"), s.P95LatencyMs.Value, "ms"),
					observation("p99_latency_ms", sourceMetric(s.Runtime, "p99_latency_ms"), s.P99LatencyMs.Value, "ms"),
				},
			}
		},
	}
}

// ── Efficiency ──────────────────────────────────────────────────────────────

func prefixCacheIneffective() Rule {
	return Rule{
		Code:     CodePrefixCacheIneffective,
		Title:    "Prefix cache is not paying for itself",
		Severity: telemetry.SeverityInfo,
		Runtimes: []telemetry.Runtime{telemetry.RuntimeVLLM},
		Summary: "Prefix-cache hit rate is low while prompt-token throughput is significant: prefill work " +
			"that could be skipped is being redone.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.PrefixCacheHitRatePct.OK || !s.PromptTokensPerSec.OK {
				return nil
			}
			// A cache miss rate is meaningless when almost no prefill is
			// happening, and dividing tiny counter deltas produces noise.
			if s.PromptTokensPerSec.Value < 50 {
				return nil
			}
			if s.PrefixCacheHitRatePct.Value >= t.PrefixCacheHitLowPct {
				return nil
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"Only %.1f%% of the %.0f prompt tokens per second this workload processes are being "+
						"served from the prefix cache. If requests share a system prompt or a conversation "+
						"history, that prefill is being paid for repeatedly.",
					s.PrefixCacheHitRatePct.Value, s.PromptTokensPerSec.Value),
				Action: "Confirm prefix caching is enabled. If it is, the usual cause is routing: requests " +
					"that share a prefix are being spread across replicas, so no replica sees the prefix " +
					"twice. Session or prefix-aware routing at the load balancer fixes it.",
				Evidence: []telemetry.Evidence{
					evidence("prefix_cache_hit_rate_percent",
						sourceMetric(s.Runtime, "prefix_cache_hit_rate_percent"),
						s.PrefixCacheHitRatePct.Value, t.PrefixCacheHitLowPct, "<", "percent"),
					observation("prompt_tokens_per_sec", "vllm:prompt_tokens_total",
						s.PromptTokensPerSec.Value, "tokens per second"),
				},
			}
		},
	}
}

func lowTokenThroughput() Rule {
	return Rule{
		Code:     CodeLowTokenThroughput,
		Title:    "GPU busy but token throughput low",
		Severity: telemetry.SeverityInfo,
		Summary: "High GPU utilisation with few generated tokens per second: the accelerator is busy with " +
			"something other than producing output, typically prefill.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.GPUUtilizationPct.Above(t.GPUUtilHighPct) ||
				!s.TokensPerSecond.OK || s.TokensPerSecond.Value >= t.TokensPerSecondLow {
				return nil
			}
			ev := []telemetry.Evidence{
				evidence("gpu_utilization_percent", sourceMetric(s.Runtime, "gpu_utilization_percent"),
					s.GPUUtilizationPct.Value, t.GPUUtilHighPct, ">", "percent"),
				evidence("tokens_per_second", sourceMetric(s.Runtime, "tokens_per_second"),
					s.TokensPerSecond.Value, t.TokensPerSecondLow, "<", "tokens per second"),
			}
			if s.PromptTokensPerSec.OK {
				ev = append(ev, observation("prompt_tokens_per_sec", "vllm:prompt_tokens_total",
					s.PromptTokensPerSec.Value, "tokens per second"))
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"GPU utilisation is %.0f%% but only %.0f tokens per second are being generated. "+
						"Utilisation counts any kernel activity, so a prefill-heavy workload looks fully "+
						"busy while producing very little user-visible output.",
					s.GPUUtilizationPct.Value, s.TokensPerSecond.Value),
				Action: "Compare prompt-token throughput with generation-token throughput. If prefill " +
					"dominates, chunked prefill and prefix caching recover more than extra replicas would.",
				Evidence: ev,
			}
		},
	}
}

func gpuMemoryPressure() Rule {
	return Rule{
		Code:     CodeGPUMemoryPressure,
		Title:    "GPU memory near capacity",
		Severity: telemetry.SeverityWarning,
		Summary: "Device memory has stayed near full for the sustain window, leaving no room for a longer " +
			"prompt or an additional concurrent sequence.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			ok := e.Sustained(func(s telemetry.Snapshot) bool {
				return s.GPUMemoryUsedPct.Above(t.GPUMemoryHighPct)
			})
			if !ok {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"GPU memory has been above %.0f%% for %s, currently %.1f%%. For a workload with a "+
						"preallocated KV cache this is often by design — check whether the number moves at "+
						"all before treating it as a leak.",
					t.GPUMemoryHighPct, roundDur(e.Span()), s.GPUMemoryUsedPct.Value),
				Action: "If the figure is static it is the runtime's own reservation and is expected. " +
					"If it climbs, look for another process sharing the device, or a growing set of loaded " +
					"adapters or models.",
				Evidence: []telemetry.Evidence{
					evidence("gpu_memory_used_percent", sourceMetric(s.Runtime, "gpu_memory_used_percent"),
						s.GPUMemoryUsedPct.Value, t.GPUMemoryHighPct, ">", "percent"),
				},
			}
		},
	}
}

// ── Errors ──────────────────────────────────────────────────────────────────

func errorRateHigh() Rule {
	return Rule{
		Code:     CodeErrorRateHigh,
		Title:    "Elevated error rate",
		Severity: telemetry.SeverityCritical,
		Summary: "The runtime's own failure counter is above threshold. Only fires for runtimes that " +
			"expose one — vLLM does not.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			if !e.Sustained(func(sn telemetry.Snapshot) bool {
				return sn.ErrorRatePct.Above(t.ErrorRatePct)
			}) {
				return nil
			}
			s := e.Latest
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.2f%% of requests are failing inside the runtime, against a %.1f%% threshold.",
					s.ErrorRatePct.Value, t.ErrorRatePct),
				Action: "Read the runtime logs. The usual causes are out-of-memory kills on long prompts, " +
					"requests exceeding max_model_len, and model or adapter load failures.",
				Evidence: []telemetry.Evidence{
					evidence("error_rate_percent", sourceMetric(s.Runtime, "error_rate_percent"),
						s.ErrorRatePct.Value, t.ErrorRatePct, ">", "percent"),
				},
			}
		},
	}
}

func clientAborts() Rule {
	return Rule{
		Code:     CodeClientAborts,
		Title:    "Clients are abandoning requests",
		Severity: telemetry.SeverityWarning,
		Summary: "A significant share of requests finished as aborted, which usually means callers timed " +
			"out or disconnected before the response completed.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			// Rates derived from integer counters over one scrape interval are
			// lumpy: a single abort in a quiet interval reads as a large
			// percentage. Requiring the condition to persist is what separates
			// a real abandonment problem from counter granularity.
			if !e.Sustained(func(sn telemetry.Snapshot) bool {
				return sn.AbortRatePct.Above(t.AbortRatePct)
			}) {
				return nil
			}
			s := e.Latest
			ev := []telemetry.Evidence{
				evidence("abort_rate_percent", "vllm:request_success_total{finished_reason=\"abort\"}",
					s.AbortRatePct.Value, t.AbortRatePct, ">", "percent"),
			}
			if s.TTFTP95Ms.OK {
				ev = append(ev, observation("ttft_p95_ms", sourceMetric(s.Runtime, "ttft_p95_ms"),
					s.TTFTP95Ms.Value, "ms"))
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.1f%% of finished requests were aborted. The runtime records an abort when the caller "+
						"goes away, so this is a client-visible symptom even though no request failed: users "+
						"or upstream timeouts are giving up before the response arrives.",
					s.AbortRatePct.Value),
				Action: "Compare against TTFT for this workload. If TTFT is also high the aborts are the " +
					"downstream effect and the latency is the thing to fix; if TTFT is fine, look at " +
					"client-side timeouts and any proxy in front of the endpoint.",
				Evidence: ev,
			}
		},
	}
}

// ── Scaling ─────────────────────────────────────────────────────────────────

func atReplicaCeiling() Rule {
	return Rule{
		Code:     CodeAtReplicaCeiling,
		Title:    "Saturated at the replica ceiling",
		Severity: telemetry.SeverityCritical,
		Summary: "The workload is queueing while already running at its maximum replica count, so no " +
			"autoscaler can resolve it.",
		Supersedes: []Code{CodeQueueWithSaturatedGPU},
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.Replicas.OK || !s.MaxReplicas.OK || s.MaxReplicas.Value <= 0 {
				return nil
			}
			if s.Replicas.Value < s.MaxReplicas.Value {
				return nil
			}
			ok := e.Sustained(func(sn telemetry.Snapshot) bool {
				return sn.RequestsWaiting.Above(t.QueueWaitingRequests)
			})
			if !ok {
				return nil
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.0f requests have been queueing for %s while the deployment sits at its ceiling of "+
						"%.0f replicas. Horizontal scaling has nothing left to give, so the queue will not "+
						"drain until traffic falls or each replica does more.",
					s.RequestsWaiting.Value, roundDur(e.Span()), s.MaxReplicas.Value),
				Action: "Raise the maximum replica count if there is cluster capacity, or reduce per-request " +
					"cost. If GPU nodes are the constraint, this is a cluster-capacity decision rather than " +
					"a workload one.",
				Evidence: []telemetry.Evidence{
					observation("replicas", "", s.Replicas.Value, "replicas"),
					observation("max_replicas", "", s.MaxReplicas.Value, "replicas"),
					evidence("requests_waiting", sourceMetric(s.Runtime, "requests_waiting"),
						s.RequestsWaiting.Value, t.QueueWaitingRequests, ">", "requests"),
				},
			}
		},
	}
}

func replicasNotReady() Rule {
	return Rule{
		Code:     CodeReplicasNotReady,
		Title:    "Replicas unavailable while the queue builds",
		Severity: telemetry.SeverityCritical,
		Summary: "Fewer replicas are ready than desired while requests queue: capacity has been asked for " +
			"and is not arriving.",
		Eval: func(e *Eval) *Finding {
			t := e.T
			s := e.Latest
			if !s.Replicas.OK || !s.ReadyReplicas.OK {
				return nil
			}
			missing := s.Replicas.Value - s.ReadyReplicas.Value
			if missing < 1 {
				return nil
			}
			if !s.RequestsWaiting.Above(t.QueueWaitingRequests) {
				return nil
			}
			return &Finding{
				Window: e.Span(),
				Explanation: fmt.Sprintf(
					"%.0f of %.0f replicas are not ready while %.0f requests wait. For GPU workloads the "+
						"usual causes are pods pending on unavailable accelerators, a long model download "+
						"or load, or a failing readiness probe during warm-up.",
					missing, s.Replicas.Value, s.RequestsWaiting.Value),
				Action: "Check pod status and events for the deployment. Pending pods point at cluster GPU " +
					"capacity; CrashLoopBackOff or a slow-passing readiness probe points at model loading.",
				Evidence: []telemetry.Evidence{
					observation("replicas", "", s.Replicas.Value, "replicas"),
					observation("ready_replicas", "", s.ReadyReplicas.Value, "replicas"),
					evidence("requests_waiting", sourceMetric(s.Runtime, "requests_waiting"),
						s.RequestsWaiting.Value, t.QueueWaitingRequests, ">", "requests"),
				},
			}
		},
	}
}

// ── Observability of the observed workload ──────────────────────────────────

func telemetryIncomplete() Rule {
	return Rule{
		Code:     CodeTelemetryIncomplete,
		Title:    "Telemetry incomplete for this workload",
		Severity: telemetry.SeverityInfo,
		Summary: "Expected metrics were absent from the target's exposition, so some rules cannot run for " +
			"this workload. Reported so that partial coverage does not read as a clean bill of health.",
		Eval: func(e *Eval) *Finding {
			s := e.Latest
			if len(s.MetricsMissing) == 0 {
				return nil
			}
			return &Finding{
				Explanation: fmt.Sprintf(
					"The scrape target did not expose %d expected metric(s): %s. Rules that depend on them "+
						"are not evaluated for this workload, so an absence of findings here is not evidence "+
						"of health.",
					len(s.MetricsMissing), strings.Join(s.MetricsMissing, ", ")),
				Action: "Check the runtime version and its metrics flags — vLLM needs --disable-log-stats " +
					"left off — and confirm the configured runtime matches what the target actually runs.",
				Evidence: []telemetry.Evidence{
					observation("metrics_missing", "", float64(len(s.MetricsMissing)), "metrics"),
				},
			}
		},
	}
}

func telemetryStale() Rule {
	return Rule{
		Code:     CodeTelemetryStale,
		Title:    "Telemetry is stale",
		Severity: telemetry.SeverityWarning,
		Summary: "No fresh scrape has succeeded for this workload within the staleness window. Its other " +
			"findings describe the past, not the present.",
		Eval: func(e *Eval) *Finding {
			age := e.Latest.Age(e.Now)
			if age <= e.T.StaleAfter {
				return nil
			}
			return &Finding{
				Explanation: fmt.Sprintf(
					"The most recent successful scrape for this workload is %s old, beyond the %s staleness "+
						"window. Every other finding for it is being evaluated against telemetry that may no "+
						"longer describe the workload.",
					roundDur(age), roundDur(e.T.StaleAfter)),
				Action: "Check that the target is still reachable and still exposes metrics: a deleted pod, " +
					"a changed Service, or a NetworkPolicy blocking the scrape all produce this.",
				Evidence: []telemetry.Evidence{
					evidence("telemetry_age_seconds", "", age.Seconds(), e.T.StaleAfter.Seconds(), ">", "seconds"),
				},
			}
		},
	}
}

// queueShareOfTTFT estimates how much of time-to-first-token is spent waiting
// for admission rather than on prefill.
//
// The two inputs are percentiles of different histograms with different bucket
// boundaries — vLLM gives TTFT and queue time separate bucket sets — so the
// ratio is an estimate and can legitimately exceed 1 when interpolation error
// runs in opposite directions. Clamping keeps the rules from reporting a share
// above 100%% and a negative prefill time, either of which reads as a bug to
// whoever is looking at the output at the time.
func queueShareOfTTFT(s telemetry.Snapshot) (float64, bool) {
	if !s.TTFTP95Ms.OK || !s.QueueTimeP95Ms.OK || s.TTFTP95Ms.Value <= 0 {
		return 0, false
	}
	share := s.QueueTimeP95Ms.Value / s.TTFTP95Ms.Value * 100
	if share > 100 {
		share = 100
	}
	return share, true
}

// roundDur trims a duration to whole seconds for human-readable text.
func roundDur(d time.Duration) time.Duration {
	return d.Round(time.Second)
}
