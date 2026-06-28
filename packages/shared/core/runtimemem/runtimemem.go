// Package runtimemem applies a Go soft memory limit (GOMEMLIMIT) derived from the
// container's cgroup memory limit when the operator has not set one explicitly.
//
// The Go runtime reads GOMEMLIMIT once at process start, so a value that is unset
// there cannot be supplied later by loading a .env file — it must be applied
// programmatically via debug.SetMemoryLimit. Without any soft limit a burst of
// large request/response bodies can grow the heap until the kernel OOM-kills the
// service (observed under high-concurrency SSE). The AMI/systemd deployment stamps
// GOMEMLIMIT; this auto-set covers container / hand-rolled deployments that do not.
package runtimemem

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// memLimitFraction is the share of the detected cgroup memory limit used for the
// Go soft limit, leaving headroom for non-heap memory (stacks, mmap, the cgroup's
// page cache) so the GC defends a ceiling below the hard OOM-kill limit.
const memLimitFraction = 0.70

// cgroupUnlimitedThreshold: a cgroup-v1 limit_in_bytes at or above this is the
// kernel's "unlimited" sentinel (a near-INT64_MAX page-aligned value), not a real
// cap, so it is treated as no limit.
const cgroupUnlimitedThreshold = int64(1) << 62

// cgroup memory-limit files, as package vars so tests can point them at fixtures.
// Production never reassigns them. cgroup v2 first (unified hierarchy), then v1.
var (
	cgroupV2MaxPath = "/sys/fs/cgroup/memory.max"
	cgroupV1MaxPath = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// AutoSetMemoryLimit sets the Go soft memory limit from the cgroup memory limit
// when GOMEMLIMIT is not already set, and logs a WARN with the chosen value and how
// to override. It is a no-op when GOMEMLIMIT is set (the runtime already applied it)
// or when no cgroup memory limit is detectable (the service is left with no soft
// cap, the previous behavior). Call once at service startup, after any .env load.
func AutoSetMemoryLimit(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) != "" {
		return // explicit GOMEMLIMIT — the Go runtime already applied it at startup
	}
	limit, ok := cgroupMemoryLimit()
	if !ok {
		return // no detectable cgroup limit — leave the heap unbounded (no soft cap)
	}
	target := int64(float64(limit) * memLimitFraction)
	if target <= 0 {
		return
	}
	debug.SetMemoryLimit(target)
	logger.Warn("GOMEMLIMIT not set; auto-applied a Go soft memory limit from the cgroup limit",
		"chosen_bytes", target,
		"chosen_mib", target/(1<<20),
		"cgroup_limit_bytes", limit,
		"fraction", memLimitFraction,
		"override", "set GOMEMLIMIT=<size> (e.g. 22GiB) in the service env "+
			"(/etc/nexus/nexus.env or your deployment env) to pin a fixed value")
}

// cgroupMemoryLimit returns the cgroup memory limit in bytes, preferring the v2
// unified hierarchy and falling back to v1. Returns ok=false when no real (finite)
// limit is configured.
func cgroupMemoryLimit() (int64, bool) {
	if v, ok := readCgroupMax(cgroupV2MaxPath); ok {
		return v, true
	}
	return readCgroupMax(cgroupV1MaxPath)
}

// readCgroupMax parses a single cgroup memory-limit file. It returns ok=false for
// an unreadable file, the cgroup-v2 "max" literal (no limit), an unparseable value,
// a non-positive value, or the v1 near-INT64_MAX "unlimited" sentinel.
func readCgroupMax(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false // cgroup v2 unlimited
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n >= cgroupUnlimitedThreshold {
		return 0, false // unparseable or v1 unlimited sentinel
	}
	return n, true
}
