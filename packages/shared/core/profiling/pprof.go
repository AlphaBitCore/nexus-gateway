// Package profiling exposes the Go runtime profiler on an operator-gated,
// default-OFF switch. The blank net/http/pprof import registers the HTTP
// handlers at init, but Start mounts nothing unless NEXUS_PPROF_ENABLED is
// truthy — so a build carries the capability at ZERO runtime cost until an
// operator turns it on, the right default for a binary that may be load-tested
// in place without a rebuild.
//
// When enabled, profiles are captured ON DEMAND (never continuously, so an
// idle-but-enabled service adds no measurable overhead to a load test): send
// SIGUSR1 and the service dumps heap+goroutine+allocs snapshots plus an
// N-second CPU profile as .pprof FILES into NEXUS_PPROF_DIR. Writes are buffered
// (bufio) so the file I/O is large-chunk and does not compete with the hot path.
// Collect the files off the box afterwards — no live network tunnel needed.
//
// Env (read by every service via Start):
//   - NEXUS_PPROF_ENABLED   master switch; default false. "1"/"true"/"yes"/"on" enables.
//   - NEXUS_PPROF_DIR       dump directory (default /tmp/nexus-pprof).
//   - NEXUS_PPROF_CPU_SECONDS  CPU-profile window per SIGUSR1 (default 20).
//   - NEXUS_PPROF_ADDR      optional: also serve /debug/pprof over HTTP (loopback only).
package profiling

import (
	"bufio"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on http.DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// EnvEnabled is the master switch; default false. Nothing is mounted unless
	// it is truthy, so a disabled service pays zero profiling overhead.
	EnvEnabled = "NEXUS_PPROF_ENABLED"
	// EnvDir is the SIGUSR1 dump directory.
	EnvDir = "NEXUS_PPROF_DIR"
	// EnvCPUSeconds overrides the SIGUSR1 CPU-profile window (default 20s).
	EnvCPUSeconds = "NEXUS_PPROF_CPU_SECONDS"
	// EnvAddr, when set (and EnvEnabled on), also serves /debug/pprof over HTTP.
	EnvAddr = "NEXUS_PPROF_ADDR"

	defaultDir        = "/tmp/nexus-pprof"
	defaultCPUSeconds = 20
	// writeBufBytes buffers profile writes so the flush is a few large syscalls
	// rather than many small ones — keeps a capture from contending with the
	// service's own I/O during a load test.
	writeBufBytes = 256 << 10
)

// Enabled reports whether the master switch NEXUS_PPROF_ENABLED is truthy.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnabled))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Start wires the opt-in profiler for service `name` (the dump-file prefix). It
// is a pure no-op — no goroutine, no handler, no listener — unless
// NEXUS_PPROF_ENABLED is truthy. Call once at startup.
func Start(name string) {
	if !Enabled() {
		return
	}
	startSignalDump(name)
	startHTTP()
}

func startHTTP() {
	addr := os.Getenv(EnvAddr)
	if addr == "" {
		return
	}
	go func() {
		slog.Info("pprof http listening (loopback profiling)", "addr", addr)
		if err := http.ListenAndServe(addr, nil); err != nil { //nolint:gosec // loopback-only, operator-gated
			slog.Warn("pprof http server stopped", "error", err)
		}
	}()
}

// cpuRunning guards against overlapping CPU profiles — the runtime allows only
// one at a time, so a second SIGUSR1 mid-capture skips its CPU step.
var cpuRunning atomic.Bool

// resolveDumpDir returns NEXUS_PPROF_DIR, or the default when unset.
func resolveDumpDir() string {
	if d := os.Getenv(EnvDir); d != "" {
		return d
	}
	return defaultDir
}

// resolveCPUSeconds returns a positive NEXUS_PPROF_CPU_SECONDS, or the default.
func resolveCPUSeconds() int {
	if v, err := strconv.Atoi(os.Getenv(EnvCPUSeconds)); err == nil && v > 0 {
		return v
	}
	return defaultCPUSeconds
}

func startSignalDump(name string) {
	dir := resolveDumpDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("pprof dir create failed; SIGUSR1 dumps disabled", "dir", dir, "error", err)
		return
	}
	cpuSecs := resolveCPUSeconds()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	slog.Info("pprof file dumps armed — send SIGUSR1 to capture", "dir", dir, "cpuSeconds", cpuSecs, "service", name)
	go func() {
		for range ch {
			dumpProfiles(name, dir, cpuSecs)
		}
	}()
}

// dumpProfiles writes the instant snapshots immediately, then runs the CPU
// profile for cpuSecs. Safe to call repeatedly; overlapping CPU captures are
// skipped (the snapshots still write).
func dumpProfiles(name, dir string, cpuSecs int) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	writeMemStats(name, dir, ts) // GC count / pauses / heap sizes — read before the GC below
	runtime.GC()                 // settle the heap before snapshotting it
	writeLookup(name, dir, "heap", ts)
	writeLookup(name, dir, "goroutine", ts)
	writeLookup(name, dir, "allocs", ts)

	if !cpuRunning.CompareAndSwap(false, true) {
		slog.Warn("pprof CPU profile already running; skipped this SIGUSR1's CPU capture")
		return
	}
	defer cpuRunning.Store(false)
	path := filepath.Join(dir, fmt.Sprintf("%s-cpu-%s.pprof", name, ts))
	f, err := os.Create(path) //nolint:gosec // operator-supplied profiling dir
	if err != nil {
		slog.Warn("pprof cpu file create failed", "path", path, "error", err)
		return
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, writeBufBytes)
	if err := pprof.StartCPUProfile(bw); err != nil {
		slog.Warn("pprof start cpu failed", "error", err)
		return
	}
	slog.Info("pprof CPU profile capturing", "file", path, "seconds", cpuSecs)
	time.Sleep(time.Duration(cpuSecs) * time.Second)
	pprof.StopCPUProfile()
	_ = bw.Flush() // best-effort; a short file on flush failure is self-evident
	slog.Info("pprof CPU profile written", "file", path)
}

// writeMemStats writes a human-readable runtime.MemStats summary — GC count and
// pause history, heap sizes, alloc counters, live goroutines — so a load-test
// operator can read GC pressure straight out of the file without a tool.
func writeMemStats(name, dir, ts string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	path := filepath.Join(dir, fmt.Sprintf("%s-memstats-%s.txt", name, ts))
	f, err := os.Create(path) //nolint:gosec // operator-supplied profiling dir
	if err != nil {
		slog.Warn("pprof memstats create failed", "path", path, "error", err)
		return
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, writeBufBytes)
	_, _ = fmt.Fprintf(bw, "captured_utc=%s service=%s\n", ts, name)
	_, _ = fmt.Fprintf(bw, "NumGC=%d NumForcedGC=%d GCCPUFraction=%.5f\n", m.NumGC, m.NumForcedGC, m.GCCPUFraction)
	_, _ = fmt.Fprintf(bw, "PauseTotalNs=%d LastGC_unixns=%d NextGC_bytes=%d\n", m.PauseTotalNs, m.LastGC, m.NextGC)
	_, _ = fmt.Fprintf(bw, "HeapAlloc=%d HeapSys=%d HeapInuse=%d HeapIdle=%d HeapReleased=%d HeapObjects=%d\n",
		m.HeapAlloc, m.HeapSys, m.HeapInuse, m.HeapIdle, m.HeapReleased, m.HeapObjects)
	_, _ = fmt.Fprintf(bw, "Alloc=%d TotalAlloc=%d Sys=%d Mallocs=%d Frees=%d Live=%d\n",
		m.Alloc, m.TotalAlloc, m.Sys, m.Mallocs, m.Frees, m.Mallocs-m.Frees)
	_, _ = fmt.Fprintf(bw, "Goroutines=%d\n", runtime.NumGoroutine())
	_, _ = fmt.Fprintf(bw, "RecentPauseNs(most-recent-first)=%v\n", recentPauses(&m))
	_ = bw.Flush()
	slog.Info("pprof memstats written", "file", path, "numGC", m.NumGC)
}

// recentPauses returns the GC pause durations newest-first from the MemStats
// circular buffer (up to the last 256 GCs).
func recentPauses(m *runtime.MemStats) []uint64 {
	n := int(m.NumGC)
	if n > len(m.PauseNs) {
		n = len(m.PauseNs)
	}
	out := make([]uint64, 0, n)
	for i := range n {
		idx := (int(m.NumGC) - 1 - i + len(m.PauseNs)) % len(m.PauseNs)
		out = append(out, m.PauseNs[idx])
	}
	return out
}

// writeLookup snapshots a named runtime profile to <dir>/<name>-<which>-<ts>.pprof
// in binary form (debug=0) for `go tool pprof`, buffered.
func writeLookup(name, dir, which, ts string) {
	prof := pprof.Lookup(which)
	if prof == nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.pprof", name, which, ts))
	f, err := os.Create(path) //nolint:gosec // operator-supplied profiling dir
	if err != nil {
		slog.Warn("pprof snapshot create failed", "which", which, "path", path, "error", err)
		return
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, writeBufBytes)
	_ = prof.WriteTo(bw, 0) // best-effort; a short file on write failure is self-evident
	_ = bw.Flush()
	slog.Info("pprof snapshot written", "file", path)
}
