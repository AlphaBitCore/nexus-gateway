package traffic

import (
	"net/http"
	"strings"
)

// HeaderRequestID is the canonical name of the request-id header — the value a
// response echoes back and a support ticket quotes.
const HeaderRequestID = "X-Nexus-Request-Id"

// HeaderRequestIDAlias is the same id under the spelling most stacks already
// emit. It is not a second id and not a deprecated name: a caller whose
// framework stamps x-request-id on every outbound call is understood without
// changing a line, which is the drop-in compatibility this gateway sells. The
// canonical name wins when both arrive, because a caller who went to the
// trouble of sending the Nexus-specific one meant it.
const HeaderRequestIDAlias = "X-Request-Id"

// ResolveRequestID returns the caller's request id from either accepted
// spelling, or "" when the caller sent neither.
//
// This is the single read site for the header contract. Six handlers in the AI
// Gateway build an audit record, and each used to read the header itself; a
// contract that lives in one function is one that cannot be half-changed, and
// the alias fallback is exactly the kind of rule that gets added to the main
// proxy path and forgotten on the transcription route.
//
// Minting is deliberately NOT done here. Only a service that owns the response
// may invent an id, because the value it invents is the one it must echo back;
// a helper that minted on read would hand a different id to every caller of it
// within the same request.
func ResolveRequestID(h http.Header) string {
	if id := strings.TrimSpace(h.Get(HeaderRequestID)); id != "" {
		return id
	}
	return strings.TrimSpace(h.Get(HeaderRequestIDAlias))
}
