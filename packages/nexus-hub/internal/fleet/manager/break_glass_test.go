package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// TestShadowReportRequest_BreakGlassJSONFields locks in the wire contract for
// the four break-glass extension fields on ShadowReportRequest. Any rename or
// tag drift would silently corrupt the proxy->hub contract, so explicit key
// assertions catch it.
func TestShadowReportRequest_BreakGlassJSONFields(t *testing.T) {
	req := ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"killswitch": map[string]any{"engaged": true}},
		ReportedVer:  4,
		KeyVersions:  map[string]int64{"killswitch": 4},
		Reason:       "break_glass",
		SourceIP:     "10.0.0.7",
		ActorTokenID: "a1b2c3d4",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "reported", "reportedVer", "keyVersions", "reason", "sourceIp", "actorTokenId"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
	if m["reason"] != "break_glass" {
		t.Errorf("reason = %v, want break_glass", m["reason"])
	}
	if m["actorTokenId"] != "a1b2c3d4" {
		t.Errorf("actorTokenId = %v, want a1b2c3d4", m["actorTokenId"])
	}
}

// TestShadowReportRequest_OmitsBreakGlassFieldsWhenEmpty ensures a normal
// (non-break-glass) report does not leak empty break-glass fields onto the
// wire. omitempty tags carry this contract; any drift would make normal
// reports look like break-glass reports on the receiving side.
func TestShadowReportRequest_OmitsBreakGlassFieldsWhenEmpty(t *testing.T) {
	req := ShadowReportRequest{
		ID:          "proxy-1",
		Reported:    map[string]any{"k": "v"},
		ReportedVer: 1,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"keyVersions", "reason", "sourceIp", "actorTokenId"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected JSON key %q for non-break-glass report", k)
		}
	}
}

// TestHandleBreakGlassReport_RejectsMissingKeyVersions asserts the
// reconciliation helper refuses a malformed break-glass report. This is the
// guard against a data-plane regression that forgets to attach per-key
// versions — without them, the Hub has no authoritative target to adopt.
//
// The Manager is constructed with nil deps because the function returns
// before touching the store when keyVersions is empty.
func TestHandleBreakGlassReport_RejectsMissingKeyVersions(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-test", slog.Default())

	req := ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"killswitch": map[string]any{"engaged": true}},
		ReportedVer:  4,
		Reason:       "break_glass",
		ActorTokenID: "a1b2c3d4",
		// KeyVersions intentionally nil.
	}
	err := mgr.handleBreakGlassReport(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing keyVersions, got nil")
	}
}
