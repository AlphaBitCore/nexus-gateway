package platform

import (
	"testing"
	"time"
)

func TestRuntimeSamplerProducesAllL1Metrics(t *testing.T) {
	rs := NewRuntimeSampler(time.Now().Add(-time.Hour))
	samples := rs.Collect()

	want := []string{
		"runtime.goroutines",
		"runtime.heap_alloc_bytes",
		"runtime.heap_sys_bytes",
		"runtime.gc_pause_p50_ms",
		"runtime.gc_count_total",
		"runtime.threads",
		"runtime.open_fds",
		"runtime.cpu_user_seconds_total",
		"runtime.cpu_system_seconds_total",
		"runtime.rss_bytes",
		"runtime.uptime_seconds",
	}
	got := map[string]bool{}
	for _, s := range samples {
		got[s.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing metric: %s", w)
		}
	}
}

func TestRuntimeUptimeIsAtLeastOneHour(t *testing.T) {
	rs := NewRuntimeSampler(time.Now().Add(-time.Hour - time.Second))
	samples := rs.Collect()
	for _, s := range samples {
		if s.Name == "runtime.uptime_seconds" {
			if s.Value < 3600 {
				t.Fatalf("uptime=%v, want >= 3600", s.Value)
			}
			return
		}
	}
	t.Fatal("uptime sample not produced")
}
