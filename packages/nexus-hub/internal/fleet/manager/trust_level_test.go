package manager

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// trust_level_test.go covers the pure functions in trust_level.go without
// touching the store: computeTrustLevel, versionAtLeast, splitVersion. The
// Manager-bound helpers (updateTrustLevel, ComputeAndStoreTrustLevel) are
// exercised in manager_pgxmock_test.go via the PgxPool seam.

func TestComputeTrustLevel(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(24 * time.Hour)

	tests := []struct {
		name                string
		status              string
		agent               *store.ThingAgentRecord
		hasActiveAssignment bool
		minVersion          string
		want                int
	}{
		{
			name:   "revoked → 0",
			status: "revoked",
			agent:  &store.ThingAgentRecord{},
			want:   0,
		},
		{
			name:   "expired cert → 0",
			status: "online",
			agent:  &store.ThingAgentRecord{CertExpiresAt: &past},
			want:   0,
		},
		{
			name:                "no assignment → 1",
			status:              "online",
			agent:               &store.ThingAgentRecord{CertExpiresAt: &future, Version: "1.0.0"},
			hasActiveAssignment: false,
			want:                1,
		},
		{
			name:                "with assignment, no minVersion → 3",
			status:              "online",
			agent:               &store.ThingAgentRecord{CertExpiresAt: &future, Version: "1.0.0"},
			hasActiveAssignment: true,
			minVersion:          "",
			want:                3,
		},
		{
			name:                "with assignment, version >= min → 3",
			status:              "online",
			agent:               &store.ThingAgentRecord{CertExpiresAt: &future, Version: "1.2.3"},
			hasActiveAssignment: true,
			minVersion:          "1.2.0",
			want:                3,
		},
		{
			name:                "with assignment, version < min → 2",
			status:              "online",
			agent:               &store.ThingAgentRecord{CertExpiresAt: &future, Version: "1.0.0"},
			hasActiveAssignment: true,
			minVersion:          "1.2.0",
			want:                2,
		},
		{
			name:                "nil CertExpiresAt is treated as valid (no expiry check fires)",
			status:              "online",
			agent:               &store.ThingAgentRecord{Version: "1.0.0"},
			hasActiveAssignment: true,
			minVersion:          "",
			want:                3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTrustLevel(tt.status, tt.agent, tt.hasActiveAssignment, tt.minVersion)
			if got != tt.want {
				t.Errorf("computeTrustLevel = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		min     string
		want    bool
	}{
		{"1.2.3", "1.2.0", true},
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.4", false},
		{"1.2.3", "1.3.0", false},
		{"2.0.0", "1.99.99", true},
		{"v1.2.3", "v1.2.0", true}, // v-prefix stripped
		{"v1.2.3", "1.2.3", true},  // mixed
		{"", "1.0.0", false},       // empty is less than any nonzero
		{"1.0.0", "", true},        // anything >= empty
		{"1.0", "1.0.0", true},     // 1.0 == 1.0.0 (missing → 0)
		{"1.2", "1.2.0", true},     // partial equal
		{"1.2.0", "1.2", true},     // reverse partial equal
		{"abc", "0.0.0", true},     // non-numeric → 0
		{"1.0.0", "abc", true},     // both → 0/0 equal → true
	}
	for _, tt := range tests {
		t.Run(tt.version+"_vs_"+tt.min, func(t *testing.T) {
			got := versionAtLeast(tt.version, tt.min)
			if got != tt.want {
				t.Errorf("versionAtLeast(%q,%q) = %v, want %v", tt.version, tt.min, got, tt.want)
			}
		})
	}
}

func TestSplitVersion(t *testing.T) {
	if got := splitVersion(""); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := splitVersion("1.2.3"); len(got) != 3 || got[0] != "1" || got[1] != "2" || got[2] != "3" {
		t.Errorf("1.2.3 → %v", got)
	}
	if got := splitVersion("1"); len(got) != 1 || got[0] != "1" {
		t.Errorf("1 → %v", got)
	}
}
