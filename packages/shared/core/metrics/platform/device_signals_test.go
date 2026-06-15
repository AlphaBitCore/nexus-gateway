package platform

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// runtime.go — percentilePauseMs, openFDCount

// TestPercentilePauseMsCountZero pins the early-return for the no-GC case.
func TestPercentilePauseMsCountZero(t *testing.T) {
	var ms runtime.MemStats // NumGC=0
	if got := percentilePauseMs(ms, 0.5); got != 0 {
		t.Fatalf("p50 with NumGC=0 = %v, want 0", got)
	}
}

// TestPercentilePauseMsSmallSample exercises the NumGC <= 256 branch.
func TestPercentilePauseMsSmallSample(t *testing.T) {
	var ms runtime.MemStats
	ms.NumGC = 5
	ms.PauseNs[0] = 5_000_000
	ms.PauseNs[1] = 1_000_000
	ms.PauseNs[2] = 4_000_000
	ms.PauseNs[3] = 2_000_000
	ms.PauseNs[4] = 3_000_000

	if got := percentilePauseMs(ms, 0.5); got != 3.0 {
		t.Fatalf("p50 = %v, want 3.0", got)
	}
	if got := percentilePauseMs(ms, 0); got != 1.0 {
		t.Fatalf("p0 = %v, want 1.0", got)
	}
	if got := percentilePauseMs(ms, 1.0); got != 5.0 {
		t.Fatalf("p100 = %v, want 5.0", got)
	}
}

// TestPercentilePauseMsCapsAt256 exercises the count > 256 clamp.
func TestPercentilePauseMsCapsAt256(t *testing.T) {
	var ms runtime.MemStats
	ms.NumGC = 1000
	for i := range 256 {
		ms.PauseNs[i] = uint64((i + 1) * 1_000_000)
	}
	if got := percentilePauseMs(ms, 1.0); got != 256.0 {
		t.Fatalf("p100 capped = %v, want 256.0", got)
	}
	if got := percentilePauseMs(ms, 0.5); got != 128.0 {
		t.Fatalf("p50 capped = %v, want 128.0", got)
	}
}

// TestOpenFDCountReturnsZeroOnMissingDir pins the documented fall-back.
func TestOpenFDCountReturnsZeroOnMissingDir(t *testing.T) {
	got := openFDCount()
	if runtime.GOOS == "linux" {
		if got <= 0 {
			t.Fatalf("on linux openFDCount must be > 0, got %d", got)
		}
	} else {
		if got != 0 {
			t.Fatalf("on %s openFDCount must be 0 (no /proc), got %d", runtime.GOOS, got)
		}
	}
}

// TestOsReadDirSuccessAndError covers both branches of the non-Windows
// osReadDir shim.
func TestOsReadDirSuccessAndError(t *testing.T) {
	tmp := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(tmp, n), []byte{}, 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	entries, err := osReadDir(tmp)
	if err != nil {
		t.Fatalf("readdir success path errored: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want 2", entries)
	}

	_, err = osReadDir(filepath.Join(tmp, "does-not-exist"))
	if err == nil {
		t.Fatalf("expected error for missing dir")
	}
}

// staticinfo.go — friendlyOSName + primaryOutboundIP error path

// TestFriendlyOSNameMapping pins the GOOS → marketing name table.
func TestFriendlyOSNameMapping(t *testing.T) {
	cases := map[string]string{
		"darwin":    "macOS",
		"linux":     "Linux",
		"windows":   "Windows",
		"freebsd":   "freebsd",
		"plan9":     "plan9",
		"openbsd":   "openbsd",
		"netbsd":    "netbsd",
		"solaris":   "solaris",
		"dragonfly": "dragonfly",
	}
	for goos, want := range cases {
		if got := friendlyOSName(goos); got != want {
			t.Errorf("friendlyOSName(%q) = %q, want %q", goos, got, want)
		}
	}
}

// fingerprint.go — exported alias + primaryMAC nil-safe

// TestComputeDeviceFingerprintMatchesInternal asserts the exported alias.
func TestComputeDeviceFingerprintMatchesInternal(t *testing.T) {
	got := ComputeDeviceFingerprint()
	want := computeDeviceFingerprint()
	if got != want {
		t.Fatalf("ComputeDeviceFingerprint = %q, computeDeviceFingerprint = %q (must match)", got, want)
	}
	if got == "" {
		t.Fatalf("fingerprint unexpectedly empty on real host")
	}
	if len(got) != 32 {
		t.Fatalf("fingerprint length = %d, want 32 hex chars", len(got))
	}
	for _, c := range got {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("fingerprint contains non-hex char %q in %q", c, got)
		}
	}
}

// TestPrimaryMACReturnsMACOrEmpty asserts the helper is safe on every host.
func TestPrimaryMACReturnsMACOrEmpty(t *testing.T) {
	got := primaryMAC()
	if got == "" {
		return
	}
	parts := strings.Split(got, ":")
	if len(parts) != 6 {
		t.Fatalf("primaryMAC = %q, want 6 colon-separated octets", got)
	}
	for _, p := range parts {
		if len(p) != 2 {
			t.Fatalf("primaryMAC octet %q has length %d, want 2", p, len(p))
		}
	}
}

// Darwin-only branch coverage for extractIORegStringValue, hardware
// shell helpers, and totalRAMBytes lives in coverage_gaps_darwin_test.go
// (build-tagged //go:build darwin). Keeping them in this untagged file
// caused the linux build to fail with `undefined: extractIORegStringValue`
// because Go compiles every symbol reference regardless of runtime skips.

// sampler / runtime parity — sample names are stable for downstream contract

// runtime_rusage_unix.go — getRusageFn seam tests

// TestProcessResourceUsage_GetrusageFails_DegradesGracefully asserts that
// when the syscall.Getrusage call fails (e.g. unsupported in a sandbox),
// processResourceUsage returns all-zeroes rather than an error or garbage
// values. Zeroes are safe: the metric emits 0 counters and the agent
// shadow reducer treats missing CPU as "unknown" rather than crashing.
func TestProcessResourceUsage_GetrusageFails_DegradesGracefully(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("getRusageFn seam is unix-only")
	}
	orig := getRusageFn
	t.Cleanup(func() { getRusageFn = orig })
	getRusageFn = func(_ int, _ *syscall.Rusage) error {
		return errors.New("operation not supported")
	}

	cpuUser, cpuSys, rssBytes := processResourceUsage()
	if cpuUser != 0 || cpuSys != 0 || rssBytes != 0 {
		t.Errorf("Getrusage failure must yield all-zeroes; got user=%v sys=%v rss=%v",
			cpuUser, cpuSys, rssBytes)
	}
}

// fingerprint.go — netInterfacesFn and computeFingerprintSignalsFn seam tests

// TestPrimaryMAC_InterfacesError_ReturnsEmpty asserts that when the OS
// NIC enumeration fails (permission denied, restricted container), primaryMAC
// returns "" instead of propagating the error. An empty MAC is the correct
// safe fallback — the fingerprint continues with the remaining three signals.
func TestPrimaryMAC_InterfacesError_ReturnsEmpty(t *testing.T) {
	orig := netInterfacesFn
	t.Cleanup(func() { netInterfacesFn = orig })
	netInterfacesFn = func() ([]net.Interface, error) {
		return nil, errors.New("permission denied")
	}

	if got := primaryMAC(); got != "" {
		t.Errorf("net.Interfaces error must yield empty MAC; got %q", got)
	}
}

// TestPrimaryMAC_AllLoopbackInterfaces_ReturnsEmpty asserts that when every
// interface is a loopback (common in minimal container networking), primaryMAC
// returns "" rather than returning a loopback hardware address. Loopback MACs
// are not stable device identifiers and must not pollute the fingerprint.
func TestPrimaryMAC_AllLoopbackInterfaces_ReturnsEmpty(t *testing.T) {
	orig := netInterfacesFn
	t.Cleanup(func() { netInterfacesFn = orig })
	netInterfacesFn = func() ([]net.Interface, error) {
		return []net.Interface{
			{Flags: net.FlagLoopback, HardwareAddr: net.HardwareAddr{0, 0, 0, 0, 0, 1}},
		}, nil
	}

	if got := primaryMAC(); got != "" {
		t.Errorf("loopback-only interfaces must yield empty MAC; got %q", got)
	}
}

// TestComputeDeviceFingerprint_AllSignalsEmpty_ReturnsEmpty asserts the
// privacy contract in computeDeviceFingerprint: when NONE of the four
// hardware signals can be collected (sandboxed runtime), the function
// returns "" instead of the well-known hash of "|||" — preventing a
// constant placeholder from being mistaken for a real device fingerprint.
func TestComputeDeviceFingerprint_AllSignalsEmpty_ReturnsEmpty(t *testing.T) {
	orig := computeFingerprintSignalsFn
	t.Cleanup(func() { computeFingerprintSignalsFn = orig })
	computeFingerprintSignalsFn = func() (uuid, serial, mac, cpu string) {
		return "", "", "", ""
	}

	if got := computeDeviceFingerprint(); got != "" {
		t.Errorf("all-empty signals must return empty fingerprint; got %q", got)
	}
}

// runtime.go — osReadDirFn seam: success path (openFDCount returns count)

// TestOpenFDCount_SuccessPath_ReturnsEntryCount asserts that when osReadDir
// succeeds (injected on non-Linux hosts where /proc/self/fd does not exist),
// openFDCount returns the entry count rather than 0. This validates that the
// success branch is exercised deterministically across all OS platforms.
func TestOpenFDCount_SuccessPath_ReturnsEntryCount(t *testing.T) {
	orig := osReadDirFn
	t.Cleanup(func() { osReadDirFn = orig })
	osReadDirFn = func(_ string) ([]string, error) {
		return []string{"0", "1", "2", "3"}, nil
	}

	if got := openFDCount(); got != 4 {
		t.Errorf("openFDCount with 4 injected entries = %d, want 4", got)
	}
}

// staticinfo.go — dialContextFn seam: error + non-UDPAddr paths

// TestPrimaryOutboundIP_DialError_ReturnsEmpty asserts the safe degradation
// when the dial fails (no route, restricted sandbox, network namespace). An
// empty IP is the correct result — callers treat "" as "unknown IP" and the
// StaticInfo.PrimaryIP field is advisory only.
func TestPrimaryOutboundIP_DialError_ReturnsEmpty(t *testing.T) {
	orig := dialContextFn
	t.Cleanup(func() { dialContextFn = orig })
	dialContextFn = func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, errors.New("no route to host")
	}

	if got := primaryOutboundIP(); got != "" {
		t.Errorf("dial error must yield empty IP; got %q", got)
	}
}

// TestPrimaryOutboundIP_NonUDPAddr_ReturnsEmpty asserts the type-assertion
// branch: if the returned conn.LocalAddr() is not a *net.UDPAddr (would
// require a very unusual OS or mock), the function returns "" rather than
// panicking or returning a bogus value.
func TestPrimaryOutboundIP_NonUDPAddr_ReturnsEmpty(t *testing.T) {
	orig := dialContextFn
	t.Cleanup(func() { dialContextFn = orig })
	dialContextFn = func(_ context.Context, _, _ string) (net.Conn, error) {
		return &stubNonUDPConn{}, nil
	}

	if got := primaryOutboundIP(); got != "" {
		t.Errorf("non-UDPAddr LocalAddr must yield empty IP; got %q", got)
	}
}

// stubNonUDPConn is a minimal net.Conn implementation whose LocalAddr returns
// a *net.TCPAddr — not *net.UDPAddr — to exercise the type-assertion failure
// branch in primaryOutboundIP.
type stubNonUDPConn struct{}

func (*stubNonUDPConn) Read(_ []byte) (int, error)         { return 0, nil }
func (*stubNonUDPConn) Write(_ []byte) (int, error)        { return 0, nil }
func (*stubNonUDPConn) Close() error                       { return nil }
func (*stubNonUDPConn) LocalAddr() net.Addr                { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (*stubNonUDPConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (*stubNonUDPConn) SetDeadline(_ time.Time) error      { return nil }
func (*stubNonUDPConn) SetReadDeadline(_ time.Time) error  { return nil }
func (*stubNonUDPConn) SetWriteDeadline(_ time.Time) error { return nil }

// TestSampleNamesAreStableContract pins the runtime metric NAMES exactly.
func TestSampleNamesAreStableContract(t *testing.T) {
	rs := NewRuntimeSampler(time.Now().Add(-time.Minute))
	samples := rs.Collect()

	want := map[string]registry.MetricKind{
		"runtime.goroutines":               registry.KindGauge,
		"runtime.heap_alloc_bytes":         registry.KindGauge,
		"runtime.heap_sys_bytes":           registry.KindGauge,
		"runtime.gc_pause_p50_ms":          registry.KindGauge,
		"runtime.gc_count_total":           registry.KindCounter,
		"runtime.threads":                  registry.KindGauge,
		"runtime.open_fds":                 registry.KindGauge,
		"runtime.cpu_user_seconds_total":   registry.KindCounter,
		"runtime.cpu_system_seconds_total": registry.KindCounter,
		"runtime.rss_bytes":                registry.KindGauge,
		"runtime.uptime_seconds":           registry.KindGauge,
	}
	got := map[string]registry.MetricKind{}
	for _, s := range samples {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		gk, ok := got[name]
		if !ok {
			t.Errorf("missing metric: %s", name)
			continue
		}
		if gk != kind {
			t.Errorf("metric %s kind = %v, want %v", name, gk, kind)
		}
	}
}
