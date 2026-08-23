package profiling

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
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
	t.Cleanup(Start("svc-noop"))
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
	t.Cleanup(Start("svc-dump"))
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
	t.Cleanup(Start("svc-baddir")) // must not panic; MkdirAll fails → dumps disabled
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
	busy := ln.Addr().String()

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(restore)

	t.Setenv(EnvAddr, busy) // already in use → the bind must fail
	startHTTP()
	time.Sleep(100 * time.Millisecond)

	out := buf.String()
	// The claim is the defect, not the failure: an operator who reads "listening"
	// and then profiles that address is measuring whatever else holds the port.
	if strings.Contains(out, "pprof http listening") {
		t.Errorf("claimed to be listening on a port it could not bind:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, busy) {
		t.Errorf("want an ERROR naming %s, got:\n%s", busy, out)
	}
}

// A bindable address must log the success line exactly once, and carry the
// RESOLVED address — an operator given ":0" needs the port that was chosen, not
// the wildcard they asked for.
func TestStart_HTTPAddrLogsResolvedAddr(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(restore)

	t.Setenv(EnvAddr, "127.0.0.1:0")
	startHTTP()
	time.Sleep(50 * time.Millisecond)

	out := buf.String()
	if strings.Count(out, "pprof http listening") != 1 {
		t.Fatalf("want exactly one listening line, got:\n%s", out)
	}
	if strings.Contains(out, "addr=127.0.0.1:0") {
		t.Errorf("logged the requested wildcard port instead of the resolved one:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("a successful bind must not log an error:\n%s", out)
	}
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

// dumpSignal is the platform seam that got GOOS=windows out of the enforced set
// of the agent cross-build gate. The assertion is per-platform on purpose: on
// Unix the capture signal must be a real one and named in the boot line an
// operator greps for, and on Windows it must be absent rather than borrowed from
// SIGBREAK or SIGINT — binding a profile capture to a keystroke or to a shutdown
// signal would be worse than having no capture at all.
func TestDumpSignal_MatchesThePlatformContract(t *testing.T) {
	sig, name := dumpSignal()
	if runtime.GOOS == "windows" {
		if sig != nil || name != "" {
			t.Errorf("windows must have no capture signal, got %v/%q", sig, name)
		}
		return
	}
	if sig == nil {
		t.Fatal("unix must have a capture signal")
	}
	if name != "SIGUSR1" {
		t.Errorf("signal name = %q; the boot line advertises it, so it must be the real name", name)
	}
	if sig != syscall.SIGUSR1 {
		t.Errorf("capture signal = %v, want SIGUSR1", sig)
	}
}

// With no capture signal, startSignalDump must say so and NOT create the dump
// directory: an operator reading "dumps armed" on a platform that cannot arm them
// is the failure this split exists to prevent.
func TestStartSignalDump_NoSignalIsAnnouncedNotArmed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this asserts the no-signal branch; on windows it is the only branch and is covered above")
	}
	orig := dumpSignalFn
	dumpSignalFn = func() (os.Signal, string) { return nil, "" }
	t.Cleanup(func() { dumpSignalFn = orig })

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(restore)

	dir := filepath.Join(t.TempDir(), "never-created")
	t.Setenv(EnvDir, dir)
	t.Cleanup(startSignalDump("svc"))

	out := buf.String()
	if strings.Contains(out, "dumps armed") {
		t.Errorf("claimed to arm dumps with no capture signal:\n%s", out)
	}
	if !strings.Contains(out, "unavailable on this platform") {
		t.Errorf("want an explicit unavailable line, got:\n%s", out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the dump directory must not be created when nothing can trigger a dump")
	}
}

// The exposure of the pprof endpoint is REPORTED, not assumed. The line used to
// say "(loopback profiling)" for whatever address the operator gave — and
// .env.example suggests ":6060", a wildcard bind — so it asserted the endpoint
// was unreachable off-box while advertising the opposite. A heap profile on a
// gateway carries request-body bytes, so the label is about real content.
func TestStartHTTP_ReportsTheActualExposure(t *testing.T) {
	t.Run("loopback bind says loopback", func(t *testing.T) {
		var buf bytes.Buffer
		restore := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
		defer slog.SetDefault(restore)

		t.Setenv(EnvAddr, "127.0.0.1:0")
		startHTTP()
		time.Sleep(50 * time.Millisecond)
		if !strings.Contains(buf.String(), "exposure=\"loopback only\"") {
			t.Errorf("want a loopback exposure, got:\n%s", buf.String())
		}
	})

	t.Run("wildcard bind says reachable off-host", func(t *testing.T) {
		var buf bytes.Buffer
		restore := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
		defer slog.SetDefault(restore)

		t.Setenv(EnvAddr, ":0")
		startHTTP()
		time.Sleep(50 * time.Millisecond)
		out := buf.String()
		if strings.Contains(out, "loopback only") {
			t.Errorf("a wildcard bind must not be reported as loopback:\n%s", out)
		}
		if !strings.Contains(out, "REACHABLE OFF-HOST") {
			t.Errorf("want the off-host warning on a wildcard bind, got:\n%s", out)
		}
	})
}

// A disarmed handler must stop consuming the capture signal.
//
// This is the property behind the SIGUSR1 test's intermittence. signal.Notify
// registrations are process-global and Go delivers a signal to EVERY registered
// channel, so an arming that is never torn down does not go quiet — it keeps
// running a stop-the-world runtime.GC() plus three profile writes on every
// later signal. Across a test binary those handlers accumulate, one signal ends
// up costing N dumps, and the test with the tightest deadline is the one that
// fails, only under a loaded parallel run.
//
// Asserted by arming twice into two different directories, disarming the first,
// and sending one signal: the disarmed directory must stay empty while the
// armed one fills. Without the disarm both fill, which is the leak.
func TestStartSignalDump_DisarmStopsTheHandler(t *testing.T) {
	if sig, _ := dumpSignalFn(); sig == nil {
		t.Skip("no capture signal on this platform")
	}
	t.Setenv(EnvEnabled, "true")

	disarmedDir := t.TempDir()
	t.Setenv(EnvDir, disarmedDir)
	disarm := startSignalDump("svc-disarmed")

	armedDir := t.TempDir()
	t.Setenv(EnvDir, armedDir)
	t.Cleanup(startSignalDump("svc-armed"))

	disarm()

	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("send SIGUSR1: %v", err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for len(mustReadDir(t, armedDir)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the armed handler never fired; the test cannot say anything about the disarmed one")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The armed handler has run, so a still-registered disarmed one would have
	// been delivered the same signal and written by now.
	if got := mustReadDir(t, disarmedDir); len(got) != 0 {
		t.Errorf("the disarmed handler still wrote %d file(s); signal.Stop did not take effect", len(got))
	}
}
