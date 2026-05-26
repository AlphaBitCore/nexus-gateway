//go:build darwin

package platform

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

// TestExtractIORegStringValueBranches covers every early-return in the
// ioreg parser.
func TestExtractIORegStringValueBranches(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: extractIORegStringValue is build-tagged")
	}
	cases := []struct {
		name string
		data string
		key  string
		want string
	}{
		{
			name: "happy path",
			data: `  "IOPlatformUUID" = "12345678-90AB-CDEF-1234-567890ABCDEF"  `,
			key:  "IOPlatformUUID",
			want: "12345678-90AB-CDEF-1234-567890ABCDEF",
		},
		{
			name: "key not present",
			data: `unrelated content with no key`,
			key:  "IOPlatformUUID",
			want: "",
		},
		{
			name: "missing equals after key",
			data: `"IOPlatformUUID" no equals sign here`,
			key:  "IOPlatformUUID",
			want: "",
		},
		{
			name: "missing first quote after equals",
			data: `"IOPlatformUUID" = no-opening-quote-here`,
			key:  "IOPlatformUUID",
			want: "",
		},
		{
			name: "missing closing quote",
			data: `"IOPlatformUUID" = "unterminated`,
			key:  "IOPlatformUUID",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractIORegStringValue([]byte(c.data), c.key); got != c.want {
				t.Errorf("extractIORegStringValue = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDarwinHardwareHelpersFailGracefullyOnBadPath exercises the err-path of
// every darwin shell-out helper.
func TestDarwinHardwareHelpersFailGracefullyOnBadPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: helpers are build-tagged")
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })

	if got := hardwareUUID(); got != "" {
		t.Errorf("hardwareUUID with broken PATH = %q, want empty", got)
	}
	if got := hardwareSerial(); got != "" {
		t.Errorf("hardwareSerial with broken PATH = %q, want empty", got)
	}
	if got := hardwareModel(); got != "" {
		t.Errorf("hardwareModel with broken PATH = %q, want empty", got)
	}
	if got := cpuBrandString(); got != "" {
		t.Errorf("cpuBrandString with broken PATH = %q, want empty", got)
	}
	if got := osVersion(); got != "" {
		t.Errorf("osVersion with broken PATH = %q, want empty", got)
	}
	if got := kernelVersion(); got != "" {
		t.Errorf("kernelVersion with broken PATH = %q, want empty", got)
	}
	if got := totalRAMBytes(); got != 0 {
		t.Errorf("totalRAMBytes with broken PATH = %d, want 0", got)
	}
	if b, err := runShellOnceBytes("ioreg", "-c", "x"); err == nil {
		t.Errorf("runShellOnceBytes with broken PATH must error, got bytes=%q", b)
	}
}

// TestTotalRAMBytesRejectsNonNumericSysctlOutput pins the strconv parse-error
// branch in totalRAMBytes via the runShellOnceFn seam. This replaces the
// earlier PATH-manipulation approach, which was not race-safe when sibling
// test packages modified PATH concurrently.
func TestTotalRAMBytesRejectsNonNumericSysctlOutput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: totalRAMBytes is build-tagged")
	}
	orig := runShellOnceFn
	t.Cleanup(func() { runShellOnceFn = orig })
	runShellOnceFn = func(_ string, _ ...string) (string, error) {
		return "not-a-number", nil
	}

	if got := totalRAMBytes(); got != 0 {
		t.Fatalf("totalRAMBytes(non-numeric sysctl) = %d, want 0", got)
	}
}

// TestTotalRAMBytesShellError_ReturnsZero covers the shell-out error branch
// (e.g. sysctl not found). Must return 0 — a missing sysctl must not crash
// CaptureStaticInfo on a minimal container image.
func TestTotalRAMBytesShellError_ReturnsZero(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: totalRAMBytes is build-tagged")
	}
	orig := runShellOnceFn
	t.Cleanup(func() { runShellOnceFn = orig })
	runShellOnceFn = func(_ string, _ ...string) (string, error) {
		return "", errors.New("executable file not found in $PATH")
	}

	if got := totalRAMBytes(); got != 0 {
		t.Fatalf("totalRAMBytes(shell error) = %d, want 0", got)
	}
}
