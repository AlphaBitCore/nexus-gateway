package agent

import "unicode/utf8"

// CutText returns s truncated to at most maxBytes bytes without splitting a
// UTF-8 sequence: the cut backs up to the nearest rune boundary. Every
// truncation that can reach a persisted TEXT column must cut on a rune
// boundary — Postgres rejects invalid UTF-8 byte sequences outright, which
// fails the whole row write (and, for synced records, wedges the sync slot).
func CutText(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
