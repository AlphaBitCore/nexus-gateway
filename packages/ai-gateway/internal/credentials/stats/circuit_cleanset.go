package credstats

// circuit clean-set helpers (T1b). A credential is "clean" once a success has
// confirmed it has no circuit hash in Redis (never failed). Clean credentials
// skip the per-success circuit HGet + auth_fails reset entirely. Membership is
// dropped the instant a failure is recorded for the credential.

// isClean reports whether the credential is in the confirmed-clean set.
func (b *Buffer) isClean(credentialID string) bool {
	if b.clean == nil {
		return false
	}
	b.cleanMu.Lock()
	_, ok := b.clean[credentialID]
	b.cleanMu.Unlock()
	return ok
}

// markClean records the credential as confirmed-clean (no circuit hash). Clears
// the set if it would exceed the cap (credentials re-confirm on next success).
func (b *Buffer) markClean(credentialID string) {
	if b.clean == nil {
		return
	}
	b.cleanMu.Lock()
	if len(b.clean) >= cleanSetCap {
		b.clean = make(map[string]struct{})
	}
	b.clean[credentialID] = struct{}{}
	b.cleanMu.Unlock()
}

// unmarkClean drops the credential from the clean set — called the moment a
// failure (401/403/429) is recorded, since the credential now has (or is about
// to have) a circuit hash that the success path must observe.
func (b *Buffer) unmarkClean(credentialID string) {
	if b.clean == nil {
		return
	}
	b.cleanMu.Lock()
	delete(b.clean, credentialID)
	b.cleanMu.Unlock()
}
