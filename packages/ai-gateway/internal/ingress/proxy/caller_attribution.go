package proxy

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// stampCallerAttribution copies the caller-declared correlation tags onto the
// audit record: the end-user tag, the session tag, and the client tag bag.
//
// Every endpoint that produces a traffic row has to do this, which is why it is
// one call rather than three lines repeated. The proxy path did it inline and
// the five gateway-native handlers — realtime, transcription, guardrail, video
// submit and video follow — did not do it at all, so a caller attributing spend
// per end user got rows with a null end_user_id on every one of those families
// while the same headers worked on /v1/chat/completions. Nothing failed; the
// attribution was simply absent, which is the shape a customer discovers a
// quarter later when the report does not add up.
//
// All three are opaque and caller-supplied. They are persisted to
// traffic_event.{end_user_id, session_id, details.clientTags} and read by
// nothing in the request path.
func stampCallerAttribution(rec *audit.Record, h http.Header) {
	if rec == nil {
		return
	}
	rec.EndUserID = extractEndUserID(h)
	rec.SessionID = extractSessionID(h)
	rec.ClientTags = extractClientTags(h)
}
