package runtimemem

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

// writeTemp writes content to a temp file and returns its path.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestReadCgroupMax(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" sentinel below means "use a missing path"
		missing bool
		wantN   int64
		wantOK  bool
	}{
		{name: "valid_number", content: "536870912\n", wantN: 536870912, wantOK: true},
		{name: "v2_max_literal", content: "max\n", wantOK: false},
		{name: "empty", content: "\n", wantOK: false},
		{name: "unparseable", content: "not-a-number", wantOK: false},
		{name: "zero", content: "0", wantOK: false},
		{name: "negative", content: "-1", wantOK: false},
		{name: "v1_unlimited_sentinel", content: "9223372036854771712", wantOK: false},
		{name: "missing_file", missing: true, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing")
			if !tc.missing {
				path = writeTemp(t, "mem", tc.content)
			}
			n, ok := readCgroupMax(path)
			if ok != tc.wantOK || (tc.wantOK && n != tc.wantN) {
				t.Fatalf("readCgroupMax(%q) = (%d,%v), want (%d,%v)", tc.content, n, ok, tc.wantN, tc.wantOK)
			}
		})
	}
}

func TestCgroupMemoryLimit_PrefersV2(t *testing.T) {
	origV2, origV1 := cgroupV2MaxPath, cgroupV1MaxPath
	defer func() { cgroupV2MaxPath, cgroupV1MaxPath = origV2, origV1 }()

	// v2 present → its value wins over v1.
	cgroupV2MaxPath = writeTemp(t, "v2", "1073741824")
	cgroupV1MaxPath = writeTemp(t, "v1", "2147483648")
	if n, ok := cgroupMemoryLimit(); !ok || n != 1073741824 {
		t.Fatalf("v2-present: got (%d,%v), want (1073741824,true)", n, ok)
	}

	// v2 "max" (no limit) → fall through to v1.
	cgroupV2MaxPath = writeTemp(t, "v2max", "max")
	if n, ok := cgroupMemoryLimit(); !ok || n != 2147483648 {
		t.Fatalf("v2-max-fallthrough: got (%d,%v), want (2147483648,true)", n, ok)
	}

	// Neither present → no limit.
	cgroupV2MaxPath = filepath.Join(t.TempDir(), "none2")
	cgroupV1MaxPath = filepath.Join(t.TempDir(), "none1")
	if n, ok := cgroupMemoryLimit(); ok || n != 0 {
		t.Fatalf("none-present: got (%d,%v), want (0,false)", n, ok)
	}
}

func TestAutoSetMemoryLimit_NoOpWhenGOMEMLIMITSet(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "1GiB")
	before := debug.SetMemoryLimit(-1) // read-only
	AutoSetMemoryLimit(slog.Default())
	if after := debug.SetMemoryLimit(-1); after != before {
		t.Fatalf("limit changed despite GOMEMLIMIT set: before=%d after=%d", before, after)
	}
}

func TestAutoSetMemoryLimit_NoOpWhenNoCgroup(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	origV2, origV1 := cgroupV2MaxPath, cgroupV1MaxPath
	defer func() { cgroupV2MaxPath, cgroupV1MaxPath = origV2, origV1 }()
	cgroupV2MaxPath = filepath.Join(t.TempDir(), "none2")
	cgroupV1MaxPath = filepath.Join(t.TempDir(), "none1")

	before := debug.SetMemoryLimit(-1)
	AutoSetMemoryLimit(nil) // also exercises the nil-logger default
	if after := debug.SetMemoryLimit(-1); after != before {
		t.Fatalf("limit changed despite no cgroup limit: before=%d after=%d", before, after)
	}
}

func TestAutoSetMemoryLimit_AppliesFractionOfCgroup(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	origV2, origV1 := cgroupV2MaxPath, cgroupV1MaxPath
	before := debug.SetMemoryLimit(-1)
	defer func() {
		cgroupV2MaxPath, cgroupV1MaxPath = origV2, origV1
		debug.SetMemoryLimit(before) // restore the process-wide soft limit
	}()

	cgroupLimit := int64(1) << 30 // 1 GiB (var, not const, so the *0.70 isn't a const conversion)
	cgroupV2MaxPath = writeTemp(t, "v2", "1073741824")
	cgroupV1MaxPath = filepath.Join(t.TempDir(), "none1")

	AutoSetMemoryLimit(slog.Default())

	want := int64(float64(cgroupLimit) * memLimitFraction)
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Fatalf("soft limit = %d, want %d (70%% of %d)", got, want, cgroupLimit)
	}
}
