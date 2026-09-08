// Package dcgm reads NVIDIA DCGM Exporter's Prometheus output.
//
// DCGM is the only source of real accelerator utilisation here. Inference
// runtimes do not report it — vLLM knows how full its KV cache is, not how busy
// the SMs are — so without a DCGM endpoint the GPU fields stay unmeasured and
// the GPU rules stay dormant. Substituting KV-cache occupancy for GPU
// utilisation, which is tempting because both are percentages, produces
// confidently wrong diagnoses: a workload with a full cache and an idle GPU is
// exactly the misconfiguration IFA-CAP-002 exists to catch, and the substitution
// makes it invisible.
package dcgm

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/p95labs/ifa/internal/promtext"
	"github.com/p95labs/ifa/internal/telemetry"
)

// DCGM Exporter metric names. Field values are documented at
// https://docs.nvidia.com/datacenter/dcgm/latest/dcgm-api/dcgm-api-field-ids.html
const (
	MetricGPUUtil     = "DCGM_FI_DEV_GPU_UTIL"      // percent
	MetricMemCopyUtil = "DCGM_FI_DEV_MEM_COPY_UTIL" // percent
	MetricFBUsed      = "DCGM_FI_DEV_FB_USED"       // MiB
	MetricFBFree      = "DCGM_FI_DEV_FB_FREE"       // MiB
	MetricSMClock     = "DCGM_FI_DEV_SM_CLOCK"      // MHz
	MetricGPUTemp     = "DCGM_FI_DEV_GPU_TEMP"      // Celsius
)

// LabelGPU is the device index label DCGM Exporter puts on every series.
const LabelGPU = "gpu"

// Device holds one GPU's metrics from one scrape.
type Device struct {
	Index          int
	UtilizationPct telemetry.Metric
	MemCopyUtilPct telemetry.Metric
	MemoryUsedMiB  telemetry.Metric
	MemoryFreeMiB  telemetry.Metric
	MemoryUsedPct  telemetry.Metric
	SMClockMHz     telemetry.Metric
	TemperatureC   telemetry.Metric
}

// Parse returns one Device per GPU found, ordered by device index.
//
// An empty result is not an error: a DCGM Exporter with no GPUs visible, or a
// URL that points at some other exporter, both legitimately produce no devices,
// and the caller decides what to do about it.
func Parse(payload string) ([]Device, error) {
	mf, err := promtext.ParseString(payload)
	if err != nil {
		return nil, fmt.Errorf("dcgm: %w", err)
	}

	indexes := map[int]bool{}
	for _, name := range []string{MetricGPUUtil, MetricFBUsed, MetricFBFree, MetricGPUTemp, MetricSMClock, MetricMemCopyUtil} {
		for _, s := range mf.Select(name, nil) {
			idx, ok := gpuIndex(s.Labels)
			if ok {
				indexes[idx] = true
			}
		}
	}
	if len(indexes) == 0 {
		return nil, nil
	}

	ordered := make([]int, 0, len(indexes))
	for i := range indexes {
		ordered = append(ordered, i)
	}
	sort.Ints(ordered)

	devices := make([]Device, 0, len(ordered))
	for _, idx := range ordered {
		sel := promtext.Labels{LabelGPU: strconv.Itoa(idx)}
		d := Device{
			Index:          idx,
			UtilizationPct: telemetry.ObservedIf(mf.Max(MetricGPUUtil, sel)),
			MemCopyUtilPct: telemetry.ObservedIf(mf.Max(MetricMemCopyUtil, sel)),
			MemoryUsedMiB:  telemetry.ObservedIf(mf.Max(MetricFBUsed, sel)),
			MemoryFreeMiB:  telemetry.ObservedIf(mf.Max(MetricFBFree, sel)),
			SMClockMHz:     telemetry.ObservedIf(mf.Max(MetricSMClock, sel)),
			TemperatureC:   telemetry.ObservedIf(mf.Max(MetricGPUTemp, sel)),
		}
		// DCGM reports used and free framebuffer, not total. A device that
		// reports only one of them cannot yield a percentage, and reporting
		// used-MiB as a percentage would be an order-of-magnitude error.
		if d.MemoryUsedMiB.OK && d.MemoryFreeMiB.OK {
			if total := d.MemoryUsedMiB.Value + d.MemoryFreeMiB.Value; total > 0 {
				d.MemoryUsedPct = telemetry.Observed(d.MemoryUsedMiB.Value / total * 100)
			}
		}
		devices = append(devices, d)
	}
	return devices, nil
}

func gpuIndex(labels promtext.Labels) (int, bool) {
	raw, ok := labels[LabelGPU]
	if !ok {
		return 0, false
	}
	idx, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return idx, true
}
