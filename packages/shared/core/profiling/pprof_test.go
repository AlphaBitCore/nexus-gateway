package profiling

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// Master switch parsing.
func TestEnabled(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "Yes", "on"} {
		t.Setenv(EnvEnabled, v)
		if !Enabled() {
			t.Errorf("Enabled()=false for %q, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "garbage"} {
		t.Setenv(EnvEnabled, v)
		if Enabled() {
			t.Errorf("Enabled()=true for %q, want false", v)
		}
	}
}

// Master switch off (default): Start is a pure no-op even when ADDR/DIR are set
// — no HTTP listener, no signal handler armed.
func TestStart_NoopWhenDisabled(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv(EnvEnabled, "") // default off
	t.Setenv(EnvAddr, addr)  // would serve, but the master switch is off
	t.Setenv(EnvDir, t.TempDir())
	Start("svc-noop")
	time.Sleep(50 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("expected no listener with master switch off, but %s is accepting", addr)
	}
}

// NEXUS_PPROF_ADDR set: /debug/pprof is served over HTTP. Drives startHTTP
// directly so the test does not arm a process-global SIGUSR1 handler that would
// leak into the signal test.
func TestStart_ServesHTTP(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv(EnvAddr, addr)
	startHTTP()

	var resp *http.Response
	for range 100 {
		r, err := http.Get("http://" + addr + "/debug/pprof/")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatalf("pprof http never came up on %s", addr)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/debug/pprof/ status = %d, want 200", resp.StatusCode)
	}
}

// NEXUS_PPROF_DIR set: SIGUSR1 dumps heap/goroutine/allocs/cpu files into the dir.
func TestStart_SignalDumpsFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvCPUSeconds, "1") // keep the CPU window short for the test
	Start("svc-dump")
	// Drain any in-flight CPU capture before returning so the process-global CPU
	// profiler is free for the next test (the signal handler runs the 1s capture
	// asynchronously).
	defer func() {
		for i := 0; i < 80 && cpuRunning.Load(); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}()
	// Give the signal handler goroutine time to register.
	time.Sleep(50 * time.Millisecond)

	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("send SIGUSR1: %v", err)
	}

	// Assert the unconditional snapshots land in THIS test's dir — proof the
	// SIGUSR1 wiring fired. The CPU step is gated by a process-global flag that
	// a leaked handler from another test may hold, so the cpu success path is
	// asserted deterministically in TestDumpProfiles_WritesAll, not here.
	deadline := time.Now().Add(4 * time.Second)
	for {
		got := map[string]bool{}
		for _, e := range mustReadDir(t, dir) {
			for _, w := range []string{"heap", "goroutine", "allocs"} {
				if filepathHasKind(e.Name(), w) {
					got[w] = true
				}
			}
		}
		if got["heap"] && got["goroutine"] && got["allocs"] {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshots missing after SIGUSR1: got %v in %s", got, dir)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Bad dir (cannot mkdir) disables dumps without panicking; Start still returns.
func TestStart_BadDirDisablesDumps(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvAddr, "")
	t.Setenv(EnvDir, "/proc/nonexistent-cannot-mkdir/sub")
	Start("svc-baddir") // must not panic; MkdirAll fails → dumps disabled
}

func TestResolveDumpDir(t *testing.T) {
	t.Setenv(EnvDir, "")
	if got := resolveDumpDir(); got != defaultDir {
		t.Errorf("resolveDumpDir()=%q, want default %q", got, defaultDir)
	}
	t.Setenv(EnvDir, "/var/log/nexus-pprof")
	if got := resolveDumpDir(); got != "/var/log/nexus-pprof" {
		t.Errorf("resolveDumpDir()=%q, want the configured dir", got)
	}
}

func TestResolveCPUSeconds(t *testing.T) {
	for _, v := range []string{"", "bad", "0", "-3"} {
		t.Setenv(EnvCPUSeconds, v)
		if got := resolveCPUSeconds(); got != defaultCPUSeconds {
			t.Errorf("resolveCPUSeconds(%q)=%d, want default %d", v, got, defaultCPUSeconds)
		}
	}
	t.Setenv(EnvCPUSeconds, "7")
	if got := resolveCPUSeconds(); got != 7 {
		t.Errorf("resolveCPUSeconds(7)=%d, want 7", got)
	}
}

// HTTP listener on an already-bound address drives the ListenAndServe error log.
func TestStart_HTTPAddrBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer func() { _ = ln.Close() }()
	t.Setenv(EnvAddr, ln.Addr().String()) // already in use → ListenAndServe fails
	startHTTP()
	time.Sleep(100 * time.Millisecond) // let the goroutine hit the error branch
}

// Unknown profile name → pprof.Lookup returns nil → writeLookup is a no-op.
func TestWriteLookup_UnknownProfile(t *testing.T) {
	dir := t.TempDir()
	writeLookup("svc", dir, "no-such-profile", "ts")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("unknown profile should write nothing, got %d files", len(entries))
	}
}

// A dir path that is actually a regular file makes every os.Create fail — covers
// the snapshot and cpu file-create error branches without panicking.
func TestDumpProfiles_UncreatableDir(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notadir")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	_ = f.Close()
	cpuRunning.Store(false)
	dumpProfiles("svc", f.Name(), 1) // Join under a file → all Create calls fail
}

// dumpProfiles called directly (no signal race) writes all four profile kinds,
// with a non-empty CPU profile after the window.
func TestDumpProfiles_WritesAll(t *testing.T) {
	dir := t.TempDir()
	cpuRunning.Store(false)
	dumpProfiles("svc", dir, 1) // 1s CPU window

	want := map[string]bool{"heap": false, "goroutine": false, "allocs": false, "cpu": false, "memstats": false}
	for _, e := range mustReadDir(t, dir) {
		for k := range want {
			if filepathHasKind(e.Name(), k) {
				want[k] = true
				if k == "cpu" {
					if fi, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || fi.Size() == 0 {
						t.Fatalf("cpu profile empty: %v", err)
					}
				}
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing %s profile in %s", k, dir)
		}
	}
}

// CPU profile already running (our flag set) → the CPU step is skipped, snapshots
// still write.
func TestDumpProfiles_CPUAlreadySkips(t *testing.T) {
	dir := t.TempDir()
	cpuRunning.Store(true)
	defer cpuRunning.Store(false)
	dumpProfiles("svc", dir, 1)
	// heap/goroutine/allocs snapshots written; no cpu file (skipped).
	for _, e := range mustReadDir(t, dir) {
		if filepathHasKind(e.Name(), "cpu") {
			t.Fatalf("cpu profile should have been skipped, found %s", e.Name())
		}
	}
}

// A CPU profile already running at the runtime level makes pprof.StartCPUProfile
// fail inside dumpProfiles — covers that error branch.
func TestDumpProfiles_StartCPUFails(t *testing.T) {
	dir := t.TempDir()
	sink, err := os.CreateTemp(t.TempDir(), "globalcpu")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer func() { _ = sink.Close() }()
	if err := pprof.StartCPUProfile(sink); err != nil {
		t.Fatalf("seed global cpu profile: %v", err)
	}
	defer pprof.StopCPUProfile()
	cpuRunning.Store(false)
	dumpProfiles("svc", dir, 1) // inner StartCPUProfile fails (already running)
}

func TestWriteMemStats(t *testing.T) {
	dir := t.TempDir()
	writeMemStats("svc", dir, "ts")
	entries := mustReadDir(t, dir)
	if len(entries) != 1 || !filepathHasKind(entries[0].Name(), "memstats") {
		t.Fatalf("expected one memstats file, got %v", names(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read memstats: %v", err)
	}
	if !containsToken(string(b), "NumGC=") || !containsToken(string(b), "Goroutines=") {
		t.Errorf("memstats file missing GC/goroutine fields:\n%s", b)
	}
}

func TestWriteMemStats_BadDir(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notadir")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	_ = f.Close()
	writeMemStats("svc", f.Name(), "ts") // Join under a file → create fails, no panic
}

func TestRecentPauses(t *testing.T) {
	var m runtime.MemStats
	// Fewer GCs than the ring → clamp to NumGC, newest-first.
	m.NumGC = 3
	m.PauseNs[0], m.PauseNs[1], m.PauseNs[2] = 10, 20, 30
	got := recentPauses(&m)
	if len(got) != 3 || got[0] != 30 || got[2] != 10 {
		t.Errorf("recentPauses small = %v, want [30 20 10]", got)
	}
	// More GCs than the ring → clamp to the ring length.
	m.NumGC = uint32(len(m.PauseNs) + 50)
	if g := recentPauses(&m); len(g) != len(m.PauseNs) {
		t.Errorf("recentPauses big len = %d, want %d", len(g), len(m.PauseNs))
	}
}

func names(es []os.DirEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name()
	}
	return out
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	return entries
}

func filepathHasKind(filename, kind string) bool {
	return len(filename) > len(kind) && (containsToken(filename, "-"+kind+"-"))
}

func containsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if s[i:i+len(tok)] == tok {
			return true
		}
	}
	return false
}
