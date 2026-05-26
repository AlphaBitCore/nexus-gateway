package jwtverifier

// Test-only seams for mqrevocation_test.go. Compiled only under `go test` so
// nothing here ships in production binaries. Names avoid the `Test` prefix to
// keep them out of the test-runner discovery list.

// SeedBloomOnly adds an entry to the bloom filter WITHOUT recording it in the
// exact byJTI map, reproducing a deterministic bloom-false-positive used to
// exercise the introspect-disambiguation branch in IsRevoked.
func SeedBloomOnly(c *MQRevocationChecker, jti string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filter.AddString(jti)
}

// StrictLoad returns the current strict-mode flag for assertions in
// disconnect-timer tests.
func StrictLoad(c *MQRevocationChecker) bool {
	return c.strict.Load()
}

// LastIDLoad returns the most recent replay checkpoint for assertions in
// replay-catchup tests.
func LastIDLoad(c *MQRevocationChecker) int64 {
	return c.lastID.Load()
}

// SetStrict force-toggles the strict-mode flag without waiting for the
// disconnect ticker. Used by tests that need to exercise the strict-mode
// IsRevoked branch deterministically.
func SetStrict(c *MQRevocationChecker, on bool) {
	c.strict.Store(on)
}
