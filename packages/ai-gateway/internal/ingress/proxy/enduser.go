// enduser.go — extraction of the caller-declared end-user identifier.
//
// The value is an opaque correlation tag that ties gateway traffic to the
// same end user as seen by the other products in the Nexus family: it is
// persisted verbatim onto traffic_event.end_user_id so those systems can
// join on it. The gateway never validates it, never resolves it against
// Nexus identities, and never feeds it into quota / routing / IAM — it is
// caller-asserted and therefore meaningful only within the caller's
// virtual-key scope, where the only traffic a forged value can
// misattribute is the caller's own.
package proxy

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// endUserHeader is the only carrier. The tag correlates traffic across
// the Nexus product family, so it is something a caller declares TO us —
// not something inferred from a field they addressed to an upstream
// provider. A protocol's native end-user field (the OpenAI shape's
// `user`, the Anthropic shape's `metadata.user_id`) identifies the
// caller's own end user to that provider and means something else; reading
// it here would file traffic under an identifier nobody chose for this
// purpose. One header also means attribution does not depend on which
// wire shape a caller happens to speak.
const endUserHeader = "X-Nexus-End-User-Id"

// sessionHeader carries the caller's session/conversation tag — the grain
// between the end-user tag (one per user) and X-Request-Id (one per
// request), so an external system can group a conversation's requests.
// Header-only: no chat protocol carries a reliable native session field,
// and a session is an application-level concept the caller names anyway.
const sessionHeader = "X-Nexus-Session-Id"

// endUserMaxBytes caps the stored value. The tag is an identifier, not
// free text — 256 bytes is the size comparable identifiers are specified
// at — and the cap keeps an abusive caller from inflating every
// traffic_event row through an unbounded field. The session tag shares the
// cap for the same reason.
const endUserMaxBytes = 256

// extractSessionID resolves the caller-declared session tag. Same contract
// as the end-user tag: opaque, caller-asserted, VK-scoped, capped.
func extractSessionID(h http.Header) string {
	return capEndUser(strings.TrimSpace(h.Get(sessionHeader)))
}

// extractEndUserID resolves the caller-declared end-user tag from the
// header, trimmed and capped. Returns "" when the caller declared
// nothing; the column stays NULL and nothing downstream changes.
func extractEndUserID(h http.Header) string {
	return capEndUser(strings.TrimSpace(h.Get(endUserHeader)))
}

// capEndUser enforces endUserMaxBytes without leaving a torn multi-byte
// rune at the cut.
func capEndUser(s string) string {
	if len(s) <= endUserMaxBytes {
		return s
	}
	s = s[:endUserMaxBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
