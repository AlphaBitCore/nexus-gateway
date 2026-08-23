package proxy

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// The AST gate proves every record-building function stamps; this proves the
// stamp carries the values. Both are needed — a call that lands nothing passes
// the first, and a correct helper nobody calls passes the second.
func TestStampCallerAttribution(t *testing.T) {
	h := http.Header{}
	h.Set("X-Nexus-End-User-Id", "user-42")
	h.Set("X-Nexus-Session-Id", "sess-7")
	h.Set("X-Nexus-Client-Tags", "team=platform,env=prod")

	rec := &audit.Record{}
	stampCallerAttribution(rec, h)

	if rec.EndUserID != "user-42" {
		t.Errorf("EndUserID = %q, want user-42", rec.EndUserID)
	}
	if rec.SessionID != "sess-7" {
		t.Errorf("SessionID = %q, want sess-7", rec.SessionID)
	}
	if rec.ClientTags["team"] != "platform" || rec.ClientTags["env"] != "prod" {
		t.Errorf("ClientTags = %v, want the two pairs sent", rec.ClientTags)
	}

	// Absent headers leave the columns empty rather than writing blanks, so a
	// row with no attribution is distinguishable from one attributed to "".
	bare := &audit.Record{}
	stampCallerAttribution(bare, http.Header{})
	if bare.EndUserID != "" || bare.SessionID != "" || bare.ClientTags != nil {
		t.Errorf("a request with no tags produced %q / %q / %v", bare.EndUserID, bare.SessionID, bare.ClientTags)
	}

	stampCallerAttribution(nil, h) // must not panic
}
