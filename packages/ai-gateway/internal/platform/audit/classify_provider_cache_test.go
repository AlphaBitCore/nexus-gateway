package audit

import "testing"

// TestClassifyProviderCache exercises the full truth table the helper exists
// to enforce. Three proxy stamping sites (proxy.go non-stream main path,
// proxy_cache.go stream broker joiner, proxy_cache.go non-stream broker leader)
// all delegate here, so a future caller-side edit can't reintroduce the
// "cache-write classified as na" bug this helper was extracted to prevent.
func TestClassifyProviderCache(t *testing.T) {
	zero := 0
	pos := 42
	posAlt := 100

	tests := []struct {
		name     string
		read     *int
		creation *int
		want     ProviderCacheStatus
	}{
		// hit: read > 0 wins regardless of creation
		{name: "read>0, creation nil → hit", read: &pos, creation: nil, want: ProviderCacheHit},
		{name: "read>0, creation 0 → hit", read: &pos, creation: &zero, want: ProviderCacheHit},
		{name: "read>0, creation>0 → hit (refresh write atop read)", read: &pos, creation: &posAlt, want: ProviderCacheHit},

		// miss: read zero/nil with at least one cache field non-nil ⇒ called provider, model supports cache
		{name: "read 0, creation nil → miss (provider called, no cache use)", read: &zero, creation: nil, want: ProviderCacheMiss},
		{name: "read 0, creation 0 → miss", read: &zero, creation: &zero, want: ProviderCacheMiss},
		{name: "read nil, creation>0 → miss (first-turn cache WRITE — the bug fix scenario)", read: nil, creation: &pos, want: ProviderCacheMiss},
		{name: "read 0, creation>0 → miss (refresh write, no read hit)", read: &zero, creation: &pos, want: ProviderCacheMiss},

		// na: only when both fields are nil (provider not called OR model unsupported)
		{name: "both nil → na", read: nil, creation: nil, want: ProviderCacheNA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyProviderCache(tc.read, tc.creation)
			if got != tc.want {
				t.Errorf("ClassifyProviderCache(%v, %v) = %q, want %q", deref(tc.read), deref(tc.creation), got, tc.want)
			}
		})
	}
}

func deref(p *int) any {
	if p == nil {
		return "nil"
	}
	return *p
}
