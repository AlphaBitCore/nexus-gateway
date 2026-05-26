package breakglass

import (
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeProbeClient implements shadowProbeClient for unit tests.
type fakeProbeClient struct {
	lastReportedAt    time.Time
	heartbeatInterval time.Duration
}

func (f *fakeProbeClient) LastReportedAtTime() time.Time    { return f.lastReportedAt }
func (f *fakeProbeClient) HeartbeatInterval() time.Duration { return f.heartbeatInterval }

// newTestThingClient builds a minimal *thingclient.Client without starting
// any network connections. Used to exercise NewShadowProbe's concrete-type
// constructor without a real Hub.
func newTestThingClient(t *testing.T) *thingclient.Client {
	t.Helper()
	c, err := thingclient.New(thingclient.Config{
		HubURL:            "ws://127.0.0.1:0",
		ThingType:         "compliance-proxy",
		ThingID:           "test-thing",
		Token:             "test-token",
		Logger:            slog.Default(),
		MetricsRegisterer: prometheus.NewRegistry(), // isolated registry per test
		HeartbeatInterval: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("thingclient.New: %v", err)
	}
	return c
}

// TestNewShadowProbe_WiresClient verifies that NewShadowProbe returns a
// non-nil *ShadowProbe whose StaleAfter() reflects the client's heartbeat
// interval — proving the client was wired correctly.
func TestNewShadowProbe_WiresClient(t *testing.T) {
	c := newTestThingClient(t)
	p := NewShadowProbe(c)
	if p == nil {
		t.Fatal("NewShadowProbe returned nil")
	}
	// The client's HeartbeatInterval is 15s; StaleAfter must be 2× = 30s.
	want := 30 * time.Second
	if got := p.StaleAfter(); got != want {
		t.Errorf("StaleAfter() = %v, want %v", got, want)
	}
	// HasReported must be false for a freshly constructed client (no reports sent).
	if p.HasReported() {
		t.Error("HasReported() = true for a brand-new client, want false")
	}
}

// TestShadowProbe_HasReported covers the zero-time (never reported) and
// non-zero (has reported) branches.
func TestShadowProbe_HasReported(t *testing.T) {
	tests := []struct {
		name           string
		lastReportedAt time.Time
		want           bool
	}{
		{
			name:           "zero time — never reported",
			lastReportedAt: time.Time{},
			want:           false,
		},
		{
			name:           "non-zero time — has reported",
			lastReportedAt: time.Now().Add(-5 * time.Second),
			want:           true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ShadowProbe{client: &fakeProbeClient{lastReportedAt: tc.lastReportedAt}}
			got := p.HasReported()
			if got != tc.want {
				t.Errorf("HasReported() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShadowProbe_LastReportAge covers zero-time (returns 0) and recent
// report (returns a positive duration less than an upper bound).
func TestShadowProbe_LastReportAge(t *testing.T) {
	t.Run("zero time returns 0", func(t *testing.T) {
		p := &ShadowProbe{client: &fakeProbeClient{lastReportedAt: time.Time{}}}
		if age := p.LastReportAge(); age != 0 {
			t.Errorf("LastReportAge() = %v, want 0", age)
		}
	})

	t.Run("recent report returns positive age", func(t *testing.T) {
		reported := time.Now().Add(-2 * time.Second)
		p := &ShadowProbe{client: &fakeProbeClient{lastReportedAt: reported}}
		age := p.LastReportAge()
		if age <= 0 {
			t.Errorf("LastReportAge() = %v, want > 0", age)
		}
		// Generous upper bound: the age should be much less than 1 minute
		// even on a slow CI machine.
		if age > 1*time.Minute {
			t.Errorf("LastReportAge() = %v, unexpectedly large (> 1m)", age)
		}
	})
}

// TestShadowProbe_StaleAfter verifies the 2× heartbeat multiplier.
func TestShadowProbe_StaleAfter(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"15s heartbeat → 30s stale", 15 * time.Second, 30 * time.Second},
		{"30s heartbeat → 60s stale", 30 * time.Second, 60 * time.Second},
		{"1m heartbeat → 2m stale", 1 * time.Minute, 2 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ShadowProbe{client: &fakeProbeClient{heartbeatInterval: tc.interval}}
			got := p.StaleAfter()
			if got != tc.want {
				t.Errorf("StaleAfter() = %v, want %v", got, tc.want)
			}
		})
	}
}
