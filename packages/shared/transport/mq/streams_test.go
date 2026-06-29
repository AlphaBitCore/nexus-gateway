package mq

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

func TestEventsStorage_DefaultMemoryFileOverride(t *testing.T) {
	// Default (env unset) is MemoryStorage: the audit stream is a delay-tolerant
	// RAM burst buffer, so on a disk-bandwidth-bound box its messages must not hit
	// the volume on the steady-state path (overflow goes to the producer spill).
	t.Setenv("NEXUS_EVENTS_STORAGE", "")
	if got := eventsStorage(); got != jetstream.MemoryStorage {
		t.Fatalf("default storage = %v, want MemoryStorage", got)
	}
	// NEXUS_EVENTS_STORAGE=file forces the legacy durable file-backed stream.
	for _, v := range []string{"file", "FILE", " File "} {
		t.Setenv("NEXUS_EVENTS_STORAGE", v)
		if got := eventsStorage(); got != jetstream.FileStorage {
			t.Fatalf("storage(%q) = %v, want FileStorage", v, got)
		}
	}
	// Any other value falls back to the memory default.
	t.Setenv("NEXUS_EVENTS_STORAGE", "memory")
	if got := eventsStorage(); got != jetstream.MemoryStorage {
		t.Fatalf("storage(memory) = %v, want MemoryStorage", got)
	}
}

func TestParseByteSize(t *testing.T) {
	const fb int64 = 8 * 1024 * 1024 * 1024
	cases := []struct {
		in   string
		want int64
	}{
		{"", fb},
		{"   ", fb},
		{"garbage", fb},
		{"0", fb},    // non-positive → fallback
		{"-5GB", fb}, // negative → fallback
		{"8GB", 8 * 1024 * 1024 * 1024},
		{"32gb", 32 * 1024 * 1024 * 1024}, // case-insensitive
		{"512MB", 512 * 1024 * 1024},
		{"1024KB", 1024 * 1024},
		{"1073741824", 1073741824},  // bare bytes
		{"1073741824B", 1073741824}, // trailing B
		{" 2GB ", 2 * 1024 * 1024 * 1024},
	}
	for _, c := range cases {
		if got := parseByteSize(c.in, fb); got != c.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEventsMaxBytes_EnvAliasAndDefault(t *testing.T) {
	t.Setenv("NEXUS_EVENTS_MAX_BYTES", "")
	t.Setenv("NEXUS_STREAM_MAX_BYTES", "")
	// Default is "auto" = a fraction of total RAM. Pin meminfoPath to a fixture so
	// the computed value is deterministic across hosts (Linux CI reads /proc/meminfo;
	// macOS has none and would fall back).
	origMeminfo := meminfoPath
	defer func() { meminfoPath = origMeminfo }()
	dir := t.TempDir()
	memKB := int64(16_000_000) // ~16 GiB in kB
	memFixture := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(memFixture, []byte("MemTotal:       16000000 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meminfoPath = memFixture
	wantAuto := int64(float64(memKB*1024) * eventsMaxBytesRAMFraction)
	if got := eventsMaxBytes(); got != wantAuto {
		t.Fatalf("auto default = %d, want %d (15%% of fixture RAM)", got, wantAuto)
	}
	// No readable meminfo → the fixed fallback default.
	meminfoPath = filepath.Join(dir, "absent")
	if got := eventsMaxBytes(); got != defaultEventsMaxBytes {
		t.Fatalf("fallback = %d, want %d", got, defaultEventsMaxBytes)
	}
	// Alias (rig name) is honoured when the primary is unset.
	t.Setenv("NEXUS_STREAM_MAX_BYTES", "2GB")
	if got := eventsMaxBytes(); got != 2*1024*1024*1024 {
		t.Fatalf("alias = %d, want 2GiB", got)
	}
	// Primary wins over the alias.
	t.Setenv("NEXUS_EVENTS_MAX_BYTES", "4GB")
	if got := eventsMaxBytes(); got != 4*1024*1024*1024 {
		t.Fatalf("primary = %d, want 4GiB", got)
	}
}

func TestStreamNameRouting(t *testing.T) {
	cases := []struct {
		queue string
		want  string
	}{
		{"nexus.event.traffic", "NEXUS_EVENTS"},
		{"nexus.event.audit.admin", "NEXUS_EVENTS"},
		{"nexus.auth.revocation", "NEXUS_AUTH"},
		{"nexus.auth.future.subject", "NEXUS_AUTH"},
		{"nexus.other.subject", "NEXUS_DEFAULT"},
		{"", "NEXUS_DEFAULT"},
	}
	for _, tc := range cases {
		if got := streamName(tc.queue); got != tc.want {
			t.Errorf("streamName(%q) = %q, want %q", tc.queue, got, tc.want)
		}
	}
}
