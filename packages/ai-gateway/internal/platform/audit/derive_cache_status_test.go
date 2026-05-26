package audit

import (
	"errors"
	"testing"
)

// TestDeriveCacheStatus exercises the 8 valid (gateway, provider) combinations
// from cost-estimation-architecture.md § 6.4 plus the impossible combos that
// the function rejects, plus the empty-input boundary cases.
//
// Acceptance criterion: SDD T12.3.
func TestDeriveCacheStatus(t *testing.T) {
	tests := []struct {
		name    string
		gw      GatewayCacheStatus
		pv      ProviderCacheStatus
		want    CacheStatus
		wantErr bool
	}{
		// --- 8 valid combinations (§ 6.4 derivation table) ---
		{
			name: "gateway hit + provider na → HIT (gateway served; upstream not called)",
			gw:   GatewayCacheHit, pv: ProviderCacheNA,
			want: CacheStatusHit,
		},
		{
			name: "gateway hit_inflight + provider na → HIT (singleflight coalesce)",
			gw:   GatewayCacheHitInflight, pv: ProviderCacheNA,
			want: CacheStatusHit,
		},
		{
			name: "gateway miss + provider hit → HIT (provider prompt-cache discount)",
			gw:   GatewayCacheMiss, pv: ProviderCacheHit,
			want: CacheStatusHit,
		},
		{
			name: "gateway miss + provider miss → MISS (full upstream cost)",
			gw:   GatewayCacheMiss, pv: ProviderCacheMiss,
			want: CacheStatusMiss,
		},
		{
			name: "gateway miss + provider na → MISS (provider doesn't support prompt cache)",
			gw:   GatewayCacheMiss, pv: ProviderCacheNA,
			want: CacheStatusMiss,
		},
		{
			name: "gateway skipped + provider hit → HIT (bypass + discount)",
			gw:   GatewayCacheSkipped, pv: ProviderCacheHit,
			want: CacheStatusHit,
		},
		{
			name: "gateway skipped + provider miss → MISS (bypass, no discount)",
			gw:   GatewayCacheSkipped, pv: ProviderCacheMiss,
			want: CacheStatusMiss,
		},
		{
			name: "gateway skipped + provider na → MISS (bypass, provider unsupported)",
			gw:   GatewayCacheSkipped, pv: ProviderCacheNA,
			want: CacheStatusMiss,
		},

		// --- 4 invalid combinations (gateway-served cannot pair with non-na provider) ---
		{
			name: "INVALID gateway hit + provider hit (gateway never called provider)",
			gw:   GatewayCacheHit, pv: ProviderCacheHit,
			wantErr: true,
		},
		{
			name: "INVALID gateway hit + provider miss (gateway never called provider)",
			gw:   GatewayCacheHit, pv: ProviderCacheMiss,
			wantErr: true,
		},
		{
			name: "INVALID gateway hit_inflight + provider hit (joiner did not call provider)",
			gw:   GatewayCacheHitInflight, pv: ProviderCacheHit,
			wantErr: true,
		},
		{
			name: "INVALID gateway hit_inflight + provider miss (joiner did not call provider)",
			gw:   GatewayCacheHitInflight, pv: ProviderCacheMiss,
			wantErr: true,
		},

		// --- empty-input boundaries (zero values are valid) ---
		{
			name: "both empty → MISS (no cache activity recorded)",
			gw:   "", pv: "",
			want: CacheStatusMiss,
		},
		{
			name: "gateway empty + provider hit → HIT (e.g., compliance-proxy event without gateway state)",
			gw:   "", pv: ProviderCacheHit,
			want: CacheStatusHit,
		},
		{
			name: "gateway miss + provider empty → MISS (provider state not yet stamped)",
			gw:   GatewayCacheMiss, pv: "",
			want: CacheStatusMiss,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveCacheStatus(tc.gw, tc.pv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error for gw=%q pv=%q, got nil (result=%q)", tc.gw, tc.pv, got)
				}
				if got != "" {
					t.Fatalf("want empty result on invalid combo gw=%q pv=%q, got %q", tc.gw, tc.pv, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for gw=%q pv=%q: %v", tc.gw, tc.pv, err)
			}
			if got != tc.want {
				t.Fatalf("DeriveCacheStatus(%q, %q) = %q, want %q", tc.gw, tc.pv, got, tc.want)
			}
		})
	}
}

// TestUnifiedCacheStatus_HonorsCallerPreset verifies the audit-writer-side
// helper respects a caller-preset CacheStatus (ai-guard's classify cache
// uses this pattern) and only derives when the unified field is empty.
func TestUnifiedCacheStatus_HonorsCallerPreset(t *testing.T) {
	t.Run("caller-preset HIT is preserved", func(t *testing.T) {
		rec := &Record{CacheStatus: CacheStatusHit}
		got := unifiedCacheStatus(rec)
		if got != CacheStatusHit {
			t.Fatalf("want HIT preserved, got %q", got)
		}
	})

	t.Run("caller-preset MISS is preserved", func(t *testing.T) {
		rec := &Record{CacheStatus: CacheStatusMiss}
		got := unifiedCacheStatus(rec)
		if got != CacheStatusMiss {
			t.Fatalf("want MISS preserved, got %q", got)
		}
	})

	t.Run("empty cache fields → empty result (no cache phase ran)", func(t *testing.T) {
		rec := &Record{}
		got := unifiedCacheStatus(rec)
		if got != "" {
			t.Fatalf("want empty result, got %q", got)
		}
	})

	t.Run("derived from internal fields when CacheStatus empty", func(t *testing.T) {
		rec := &Record{GatewayCacheStatus: GatewayCacheHit, ProviderCacheStatus: ProviderCacheNA}
		got := unifiedCacheStatus(rec)
		if got != CacheStatusHit {
			t.Fatalf("want HIT (derived), got %q", got)
		}
	})

	t.Run("invalid combo logs warning and returns empty", func(t *testing.T) {
		rec := &Record{GatewayCacheStatus: GatewayCacheHit, ProviderCacheStatus: ProviderCacheHit}
		got := unifiedCacheStatus(rec)
		if got != "" {
			t.Fatalf("want empty on invalid combo, got %q", got)
		}
	})
}

// Ensure the helper's empty-input handling agrees with DeriveCacheStatus
// directly — protects against future regression where the helper diverges.
func TestDeriveCacheStatus_NoErrorsAreSentinel(t *testing.T) {
	_, err := DeriveCacheStatus(GatewayCacheHit, ProviderCacheHit)
	if err == nil || errors.Is(err, nil) {
		t.Fatalf("invalid combo should return non-nil error, got %v", err)
	}
}
