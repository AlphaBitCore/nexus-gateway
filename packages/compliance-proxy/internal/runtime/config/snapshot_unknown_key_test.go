package config

import (
	"testing"
)

// TestSnapshotFor_UnknownKey covers the default `return nil` branch in
// snapshotFor. The function is package-private and HandleRuntimeConfig
// only iterates KnownRuntimeConfigKeys, so the default arm is
// reachable only via direct call — but it's still a real branch that
// any future caller passing an unexpected key must hit.
func TestSnapshotFor_UnknownKey(t *testing.T) {
	deps := RuntimeDeps{}
	if got := snapshotFor(deps, "no_such_key"); got != nil {
		t.Errorf("snapshotFor(unknown) = %v, want nil", got)
	}
}
