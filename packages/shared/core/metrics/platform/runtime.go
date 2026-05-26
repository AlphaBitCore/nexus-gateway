package platform

import (
	"runtime"
	"runtime/pprof"
	"sort"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// RuntimeSampler captures the L1 universal Go-process metrics defined in
// spec §6.1. One instance per process; safe for concurrent Collect calls.
type RuntimeSampler struct {
	startTime time.Time
}

// NewRuntimeSampler returns a sampler whose uptime is anchored at startTime.
func NewRuntimeSampler(startTime time.Time) *RuntimeSampler {
	return &RuntimeSampler{startTime: startTime}
}

// Collect snapshots the 11 L1 metrics from spec §6.1.
func (r *RuntimeSampler) Collect() []registry.Sample {
	now := time.Now()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	cpuUser, cpuSys, rssBytes := processResourceUsage()

	threads := pprof.Lookup("threadcreate").Count()

	gcPauseP50 := percentilePauseMs(ms, 0.5)

	openFDs := openFDCount()

	uptime := now.Sub(r.startTime).Seconds()

	return []registry.Sample{
		{Name: "runtime.goroutines", Kind: registry.KindGauge, Value: float64(runtime.NumGoroutine())},
		{Name: "runtime.heap_alloc_bytes", Kind: registry.KindGauge, Value: float64(ms.HeapAlloc)},
		{Name: "runtime.heap_sys_bytes", Kind: registry.KindGauge, Value: float64(ms.HeapSys)},
		{Name: "runtime.gc_pause_p50_ms", Kind: registry.KindGauge, Value: gcPauseP50},
		{Name: "runtime.gc_count_total", Kind: registry.KindCounter, Value: float64(ms.NumGC)},
		{Name: "runtime.threads", Kind: registry.KindGauge, Value: float64(threads)},
		{Name: "runtime.open_fds", Kind: registry.KindGauge, Value: float64(openFDs)},
		{Name: "runtime.cpu_user_seconds_total", Kind: registry.KindCounter, Value: cpuUser},
		{Name: "runtime.cpu_system_seconds_total", Kind: registry.KindCounter, Value: cpuSys},
		{Name: "runtime.rss_bytes", Kind: registry.KindGauge, Value: rssBytes},
		{Name: "runtime.uptime_seconds", Kind: registry.KindGauge, Value: uptime},
	}
}

// percentilePauseMs returns the requested percentile (0..1) of the most recent
// GC pause durations from runtime.MemStats. PauseNs is a 256-element ring; only
// the most recent min(NumGC, 256) entries are valid.
func percentilePauseMs(ms runtime.MemStats, p float64) float64 {
	count := ms.NumGC
	if count == 0 {
		return 0
	}
	if count > 256 {
		count = 256
	}
	pauses := make([]float64, count)
	for i := range count {
		pauses[i] = float64(ms.PauseNs[i]) / 1e6
	}
	sort.Float64s(pauses)
	idx := int(float64(len(pauses)-1) * p)
	return pauses[idx]
}

// osReadDirFn is the directory-read function used by openFDCount.
// It is a package-level variable so tests can inject a stub to exercise
// both the error path and the success path on any OS without /proc.
// Production code never reassigns this variable.
var osReadDirFn = osReadDir

// openFDCount provides best-effort open-FD count. On Linux it counts entries
// in /proc/self/fd; on macOS /proc does not exist and this returns 0; on
// Windows the platform shim returns nil/0. The metric is permitted to be 0
// on platforms without /proc.
func openFDCount() int {
	entries, err := osReadDirFn("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}
