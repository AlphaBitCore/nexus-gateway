package overrides

import (
	"testing"
)

// TestOverrideStateFromDB covers the package-internal scan-time
// constructor — empty slice yields zero value, non-empty yields a
// copy (mutating the source after construction must NOT poison the
// wrapper).
func TestOverrideStateFromDB(t *testing.T) {
	t.Run("empty yields zero value", func(t *testing.T) {
		o := overrideStateFromDB(nil)
		if len(o.raw) != 0 {
			t.Errorf("expected zero raw; got len=%d", len(o.raw))
		}
	})
	t.Run("non-empty copies bytes", func(t *testing.T) {
		src := []byte(`{"k":"v"}`)
		o := overrideStateFromDB(src)
		// Mutate source — wrapper must be unaffected.
		src[0] = 'X'
		if string(o.raw) != `{"k":"v"}` {
			t.Errorf("source mutation leaked into wrapper: %q", string(o.raw))
		}
	})
}
