package clockoffset

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// memStore is a small in-memory secretstore.Store used to exercise the
// OffsetStore round-trip and malformed-value paths without touching the
// platform-native vault or the encrypted file fallback.
//
// setErr, when non-nil, makes Set return that error without mutating the
// underlying map. Tokenmanager tests use it to simulate keychain write
// failures after a successful network refresh. Nil (default) behaves as a
// normal in-memory store for every existing caller.
type memStore struct {
	mu     sync.Mutex
	data   map[string][]byte
	setErr error
}

func newMemStore() *memStore {
	return &memStore{data: make(map[string][]byte)}
}

func (m *memStore) Set(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	m.data[key] = cp
	return nil
}

func (m *memStore) setSetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setErr = err
}

func (m *memStore) Get(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, secretstore.ErrNotFound
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

func (m *memStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memStore) Close() error { return nil }

func TestParseServerDate_Valid(t *testing.T) {
	h := http.Header{}
	h.Set("Date", "Sun, 19 Apr 2026 10:30:00 GMT")

	got, err := ParseServerDate(h)
	if err != nil {
		t.Fatalf("ParseServerDate: %v", err)
	}
	want := time.Date(2026, 4, 19, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseServerDate_Missing(t *testing.T) {
	h := http.Header{}
	if _, err := ParseServerDate(h); err == nil {
		t.Fatal("expected error for missing Date header")
	}
}

func TestParseServerDate_Malformed(t *testing.T) {
	h := http.Header{}
	h.Set("Date", "not a real date")
	if _, err := ParseServerDate(h); err == nil {
		t.Fatal("expected error for malformed Date header")
	}
}

func TestComputeOffset_LocalAhead(t *testing.T) {
	server := time.Date(2026, 4, 19, 10, 30, 0, 0, time.UTC)
	local := server.Add(3 * time.Minute)

	got := ComputeOffset(server, local)
	if got != -3*time.Minute {
		t.Fatalf("got %v, want %v", got, -3*time.Minute)
	}
}

func TestComputeOffset_LocalBehind(t *testing.T) {
	server := time.Date(2026, 4, 19, 10, 30, 0, 0, time.UTC)
	local := server.Add(-3 * time.Minute)

	got := ComputeOffset(server, local)
	if got != 3*time.Minute {
		t.Fatalf("got %v, want %v", got, 3*time.Minute)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration
		want   DriftLevel
	}{
		{"zero", 0, DriftNormal},
		{"4m59s", 4*time.Minute + 59*time.Second, DriftNormal},
		{"5m", 5 * time.Minute, DriftInfo},
		{"14m59s", 14*time.Minute + 59*time.Second, DriftInfo},
		{"15m", 15 * time.Minute, DriftWarn},
		{"59m59s", 59*time.Minute + 59*time.Second, DriftWarn},
		{"1h", time.Hour, DriftError},
		{"2h", 2 * time.Hour, DriftError},
		{"-4m59s", -(4*time.Minute + 59*time.Second), DriftNormal},
		{"-5m", -5 * time.Minute, DriftInfo},
		{"-14m59s", -(14*time.Minute + 59*time.Second), DriftInfo},
		{"-15m", -15 * time.Minute, DriftWarn},
		{"-59m59s", -(59*time.Minute + 59*time.Second), DriftWarn},
		{"-1h", -time.Hour, DriftError},
		{"-2h", -2 * time.Hour, DriftError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.offset)
			if got != tc.want {
				t.Fatalf("Classify(%v) = %v, want %v", tc.offset, got, tc.want)
			}
		})
	}
}

func TestDriftLevel_String(t *testing.T) {
	cases := []struct {
		level DriftLevel
		want  string
	}{
		{DriftNormal, "normal"},
		{DriftInfo, "info"},
		{DriftWarn, "warn"},
		{DriftError, "error"},
	}
	for _, tc := range cases {
		if got := tc.level.String(); got != tc.want {
			t.Fatalf("DriftLevel(%d).String() = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestOffsetStore_RoundTrip_Positive(t *testing.T) {
	os := NewOffsetStore(newMemStore())
	if err := os.Save(3 * time.Minute); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := os.Load()
	if got != 3*time.Minute {
		t.Fatalf("Load = %v, want %v", got, 3*time.Minute)
	}
}

func TestOffsetStore_RoundTrip_Negative(t *testing.T) {
	os := NewOffsetStore(newMemStore())
	if err := os.Save(-42 * time.Hour); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := os.Load()
	if got != -42*time.Hour {
		t.Fatalf("Load = %v, want %v", got, -42*time.Hour)
	}
}

func TestOffsetStore_Load_Empty(t *testing.T) {
	os := NewOffsetStore(newMemStore())
	if got := os.Load(); got != 0 {
		t.Fatalf("Load on empty store = %v, want 0", got)
	}
}

func TestOffsetStore_Load_Malformed(t *testing.T) {
	s := newMemStore()
	// Write raw junk under the storage key — simulates corrupt prior write.
	if err := s.Set(offsetStoreKey, []byte("not-an-int64")); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	os := NewOffsetStore(s)
	if got := os.Load(); got != 0 {
		t.Fatalf("Load on malformed store = %v, want 0", got)
	}
}
