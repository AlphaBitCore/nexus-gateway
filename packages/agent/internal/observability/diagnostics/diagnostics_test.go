package diagnostics

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostFromURL_HTTPDefaultPort(t *testing.T) {
	if got := hostFromURL("http://hub.example/api"); got != "hub.example:80" {
		t.Errorf("http default port: %q", got)
	}
}

func TestHostFromURL_HTTPSDefaultPort(t *testing.T) {
	if got := hostFromURL("https://hub.example/api"); got != "hub.example:443" {
		t.Errorf("https default port: %q", got)
	}
}

func TestHostFromURL_ExplicitPortHonored(t *testing.T) {
	if got := hostFromURL("https://hub.example:8443/api"); got != "hub.example:8443" {
		t.Errorf("explicit port: %q", got)
	}
}

func TestHostFromURL_EmptyAndMalformed(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"://broken", ""},
		{"not-a-url", "not-a-url:80"}, // url.Parse treats as path; Hostname empty? Actually returns ""
	}
	for _, c := range cases {
		got := hostFromURL(c.in)
		// not-a-url has no scheme so Hostname returns "" → host empty → ""
		if c.in == "not-a-url" {
			if got != "" {
				t.Errorf("not-a-url: got %q want \"\"", got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("input %q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestTail_FileSmallerThanWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line1", "line2", "line3"}
	if len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestTail_TruncatedToLastN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	var content strings.Builder
	for i := 0; i < 100; i++ {
		_, _ = content.WriteString("line\n")
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("expected 5 lines, got %d", len(got))
	}
}

func TestCollect_TailLinesAboveFloorHonored(t *testing.T) {
	// TailLines > 50 must be passed through (covers max returning a).
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	var content strings.Builder
	for i := 0; i < 200; i++ {
		content.WriteString("line\n")
	}
	_ = os.WriteFile(path, []byte(content.String()), 0o644)

	c := &Collector{LogFile: path, TailLines: 100}
	snap := c.Collect(context.Background())
	if len(snap.LogTail) != 100 {
		t.Errorf("TailLines=100 should return 100 lines: got %d", len(snap.LogTail))
	}
}

func TestMax_AGreaterThanB(t *testing.T) {
	if max(5, 3) != 5 {
		t.Errorf("max(5,3): got %d, want 5", max(5, 3))
	}
}

func TestMax_BGreaterOrEqual(t *testing.T) {
	if max(3, 5) != 5 {
		t.Errorf("max(3,5): %d", max(3, 5))
	}
	if max(3, 3) != 3 {
		t.Errorf("max(3,3): %d", max(3, 3))
	}
}

func TestTail_HandlesLargeFileViaWindow(t *testing.T) {
	// File well over the 64 KiB window — should still return at most n
	// lines and not OOM. We can't easily assert exact lines (window cut
	// could land mid-line and the first scanned line gets dropped) but
	// can verify count + bounded memory by checking len.
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		_, _ = f.WriteString("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	}
	_ = f.Close()

	got, err := tail(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 50 {
		t.Errorf("returned more than n: %d", len(got))
	}
}

func TestTail_MissingFileReturnsErr(t *testing.T) {
	_, err := tail("/no/such/file", 10)
	if err == nil {
		t.Errorf("expected error for missing file")
	}
}

// TestTail_DirectoryPathTakesScannerErrBranch covers tail()'s
// scanner.Err() branch. os.Open on a directory succeeds, Stat
// succeeds, but the first Read on the bufio.Scanner returns an
// "is a directory" error on macOS / Linux. The branch was previously
// untestable without filesystem mocking; this path uses a real
// TempDir to exercise it portably.
//
// On filesystems that allow scanning a directory (rare; some FUSE
// variants), the test skips so the rest of the suite stays green.
func TestTail_DirectoryPathTakesScannerErrBranch(t *testing.T) {
	dir := t.TempDir()
	_, err := tail(dir, 10)
	if err == nil {
		t.Skip("filesystem allowed scanner.Read on a directory; scanner.Err branch not exercised")
	}
}

func TestCollect_HubReachableTCP(t *testing.T) {
	// Open a TCP listener and point Collector at it. DialContext should
	// succeed quickly and the snapshot must reflect HubReachable=true.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()

	c := &Collector{HubHTTPURL: "http://" + ln.Addr().String() + "/"}
	snap := c.Collect(context.Background())
	if !snap.HubReachable {
		t.Errorf("HubReachable should be true")
	}
}

func TestCollect_HubUnreachableLeavesFalse(t *testing.T) {
	// Pick a port unlikely to be open (large random) and ensure dial
	// fails fast — the 1.5s timeout caps total wait.
	c := &Collector{HubHTTPURL: "http://127.0.0.1:1/"}
	start := time.Now()
	snap := c.Collect(context.Background())
	if snap.HubReachable {
		t.Errorf("HubReachable should be false for closed port")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("dial took too long: %v (timeout broken?)", d)
	}
}

func TestCollect_LogTailPopulated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	_ = os.WriteFile(path, []byte("a\nb\nc\n"), 0o644)

	c := &Collector{LogFile: path, TailLines: 10}
	snap := c.Collect(context.Background())
	if len(snap.LogTail) != 3 {
		t.Errorf("logTail len = %d, want 3", len(snap.LogTail))
	}
}

func TestCollect_TailLinesFloorIs50(t *testing.T) {
	// tail(c.LogFile, max(c.TailLines, 50)) — when caller passes 0,
	// the floor is 50. Below 50 lines in the file, all lines are returned.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	_ = os.WriteFile(path, []byte("only one line\n"), 0o644)

	c := &Collector{LogFile: path, TailLines: 0}
	snap := c.Collect(context.Background())
	if len(snap.LogTail) != 1 {
		t.Errorf("TailLines=0 should still return file contents under 50-line floor: got %d", len(snap.LogTail))
	}
}

func TestCollect_InterceptionModeForwarded(t *testing.T) {
	c := &Collector{
		InterceptionModeFn: func() string { return "NETransparentProxy" },
	}
	snap := c.Collect(context.Background())
	if snap.InterceptionMode != "NETransparentProxy" {
		t.Errorf("mode: %q", snap.InterceptionMode)
	}
}

func TestCollect_NilInterceptionModeFnLeavesEmpty(t *testing.T) {
	c := &Collector{InterceptionModeFn: nil}
	snap := c.Collect(context.Background())
	if snap.InterceptionMode != "" {
		t.Errorf("nil fn should leave mode empty: %q", snap.InterceptionMode)
	}
}

func TestCollect_NoLogFilePathLeavesEmptyTail(t *testing.T) {
	c := &Collector{LogFile: ""}
	snap := c.Collect(context.Background())
	// Snapshot initializes LogTail = []string{} — but never errors.
	if snap.LogTail == nil {
		t.Errorf("LogTail should be initialized empty, not nil")
	}
	if len(snap.LogTail) != 0 {
		t.Errorf("LogTail should be empty when LogFile=\"\"")
	}
}

func TestCollect_CertPathPassThrough(t *testing.T) {
	c := &Collector{CertPath: "/some/cert.pem"}
	snap := c.Collect(context.Background())
	if snap.CertPath != "/some/cert.pem" {
		t.Errorf("cert path: %q", snap.CertPath)
	}
}

// Sanity check: the test server URL parses through hostFromURL and
// dialing it should succeed — same path Collect() takes.
func TestCollect_AgainstHTTPTestServer(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	c := &Collector{HubHTTPURL: srv.URL}
	snap := c.Collect(context.Background())
	if !snap.HubReachable {
		t.Errorf("httptest server should be reachable: %+v", snap)
	}
}
