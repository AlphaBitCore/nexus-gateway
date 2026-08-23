package traffic

// View-time normalize spill-fetch tier (Phase 3, D2) + DSAR erasure regression
// (Phase 3, C1). These drive GetTrafficEventNormalized through pgxmock for the
// store legs and a stub SpillStore for the out-of-band body fetch, and assert
// the observable business outcomes of the 4-tier resolve ladder:
//
//	(a) inline body present     → recompute            [traffic_cov_test.go]
//	(b) else a spill ref present → fetch RAW + recompute [here]
//	(c) else stored sidecar      → return persisted     [here, via spill-gone]
//	(d) else                     → 404 unavailable       [here, via spill-gone]
//
// The DSAR regression proves erasure correctness now rests on the body scrub:
// once the payload body is nulled (step 1) and the sidecar normalized is nulled
// (step 1b), the view-time recompute can resurrect NO prompt/response text.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
)

// TestGetTrafficEventNormalized_SpillFetch_Recompute is tier (b): the request
// body spilled out-of-band (inline NULL, spill ref present). The handler must
// fetch the RAW spilled bytes and recompute — a spilled row normalizes exactly
// like an inline-captured row, NOT fall straight to the sidecar. The stub spill
// store returns a real OpenAI chat body; the recomputed payload must carry the
// user's message text.
func TestGetTrafficEventNormalized_SpillFetch_Recompute(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// No sha256 on the ref → integrity gate is skipped (the stub store can't
	// reproduce a recorded digest; the gate itself is covered separately).
	spillRef := []byte(`{"backend":"test","key":"req-k","contentType":"application/json"}`)
	h.spillStore = &testSpillStore{
		data:        []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"spilled prompt text"}]}`),
		contentType: "application/json",
	}

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-spill").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o-mini", "/v1/chat/completions",
			nil, "", nil, "",
			"application/json", "", spillRef, nil, "", "", false, false))
	// No sidecar query expected — the spill fetch satisfies tier (b).

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-spill/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-spill")
	if err := h.GetTrafficEventNormalized(c); err != nil {
		t.Fatalf("GetTrafficEventNormalized: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got trafficstore.TrafficEventNormalized
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(got.RequestNormalized) == 0 {
		t.Fatal("requestNormalized empty — spilled body was not fetched + recomputed")
	}
	if got.RequestStatus == nil || *got.RequestStatus != "ok" {
		t.Fatalf("requestStatus = %v, want ok", got.RequestStatus)
	}
	if !strings.Contains(string(got.RequestNormalized), "spilled prompt text") {
		t.Fatalf("normalized payload missing the spilled prompt text: %s", got.RequestNormalized)
	}
}

// TestGetTrafficEventNormalized_SpillFetch_RawSSE proves the fetch feeds RAW
// bytes to the normalizer, not the UI-wrapped form. A spilled SSE response, if
// it had gone through resolveSpillBody, would arrive quoted as a JSON string and
// the SSE codecs would never fire. Fetching raw keeps the event-stream frames
// intact so the response direction normalizes to a real projection.
func TestGetTrafficEventNormalized_SpillFetch_RawSSE(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	spillRef := []byte(`{"backend":"test","key":"resp-k","contentType":"text/event-stream"}`)
	sse := "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\ndata: [DONE]\n\n"
	h.spillStore = &testSpillStore{data: []byte(sse), contentType: "text/event-stream"}

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-sse-spill").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o-mini", "/v1/chat/completions",
			nil, "", nil, "",
			"", "text/event-stream", nil, spillRef, "", "", false, false))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-sse-spill/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-sse-spill")
	if err := h.GetTrafficEventNormalized(c); err != nil {
		t.Fatalf("GetTrafficEventNormalized: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got trafficstore.TrafficEventNormalized
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ResponseNormalized) == 0 {
		t.Fatal("responseNormalized empty — raw spilled SSE was not normalized (UI-wrapping bug?)")
	}
	if got.ResponseStatus == nil {
		t.Fatal("responseStatus must be set for a recomputed SSE response")
	}
}

// The spill object aged out to retention (Get errors). The fetch must degrade
// that direction to empty WITHOUT erroring the endpoint — a gone blob is a
// missing payload, not a server fault, and the caller must be able to tell the
// difference.
//
// This used to end in a 200 carrying the stored traffic_event_normalized
// sidecar. The table is being dropped, so the row is now genuinely unavailable
// and the answer is 404. The property under test is unchanged and is the one
// that mattered: the spill error does not become a 500.
func TestGetTrafficEventNormalized_SpillGone_NotFoundNotError(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	spillRef := []byte(`{"backend":"test","key":"gone-k"}`)
	h.spillStore = &testSpillStore{getErr: errStub("spill object aged out")}

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-gone").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o-mini", "/v1/chat/completions",
			nil, "", nil, "",
			"application/json", "", spillRef, nil, "", "", false, false))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-gone/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-gone")
	if err := h.GetTrafficEventNormalized(c); err != nil {
		t.Fatalf("GetTrafficEventNormalized: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a spill object that aged out must read as 404, not %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestGetTrafficEventNormalized_SpillIntegrityFail_GracefulUnavailable: the
// spilled blob fails the sha256 integrity gate (tampered / cross-node overwrite).
// rawSpillBody must REFUSE the bytes (never normalize forged content) and the
// direction degrades to empty; with no sidecar present the endpoint returns the
// explicit "unavailable" 404 — tier (d) — not a 500.
func TestGetTrafficEventNormalized_SpillIntegrityFail_GracefulUnavailable(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// Recorded sha256 will not match the stub store's bytes → integrity refusal.
	spillRef := []byte(`{"backend":"test","key":"tampered-k","sha256":"deadbeefdeadbeef"}`)
	h.spillStore = &testSpillStore{data: []byte(`{"forged":"content"}`), contentType: "application/json"}

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-tamper").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o-mini", "/v1/chat/completions",
			nil, "", nil, "",
			"application/json", "", spillRef, nil, "", "", false, false))
	mock.ExpectQuery(`FROM traffic_event_normalized`).
		WithArgs("evt-tamper").
		WillReturnError(errNoRowsStub())

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-tamper/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-tamper")
	_ = h.GetTrafficEventNormalized(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 unavailable on integrity refusal + no sidecar, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "forged") {
		t.Fatal("forged spill content leaked into the response — integrity gate did not refuse it")
	}
}

// TestGetTrafficEventNormalized_ResponseSpillGone_RequestInlineStill200 covers
// the per-direction nature of the ladder: the request body is inline (recomputes
// fine) while the response body spilled and is gone. The response fetch must
// warn + degrade to empty without sinking the whole row — the endpoint still 200s
// with the request projection.
func TestGetTrafficEventNormalized_ResponseSpillGone_RequestInlineStill200(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	respSpill := []byte(`{"backend":"test","key":"resp-gone"}`)
	h.spillStore = &testSpillStore{getErr: errStub("response spill aged out")}
	reqBody := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"inline survives"}]}`)

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-mixed").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o-mini", "/v1/chat/completions",
			reqBody, "", nil, "",
			"application/json", "text/event-stream", nil, respSpill, "", "", false, false))
	// No sidecar query: the inline request body satisfies tier (a) for the row.

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-mixed/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-mixed")
	if err := h.GetTrafficEventNormalized(c); err != nil {
		t.Fatalf("GetTrafficEventNormalized: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got trafficstore.TrafficEventNormalized
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(string(got.RequestNormalized), "inline survives") {
		t.Fatalf("request projection lost: %s", got.RequestNormalized)
	}
	if len(got.ResponseNormalized) != 0 {
		t.Fatalf("response should be empty (spill gone), got %s", got.ResponseNormalized)
	}
}

// TestGetTrafficEventNormalized_BadSpillRef_GracefulFallback: the stored spill
// ref is not valid JSON (corrupt column). rawSpillBody's decode must fail, the
// direction degrades to empty, and the row falls through to tier (d) — a clean
// 404, never a 500.
func TestGetTrafficEventNormalized_BadSpillRef_GracefulFallback(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	h.spillStore = &testSpillStore{data: []byte(`{"x":1}`)}

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-badref").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o", "/v1/chat/completions",
			nil, "", nil, "",
			"application/json", "", []byte("not-json"), nil, "", "", false, false))
	mock.ExpectQuery(`FROM traffic_event_normalized`).
		WithArgs("evt-badref").
		WillReturnError(errNoRowsStub())

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-badref/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-badref")
	_ = h.GetTrafficEventNormalized(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on corrupt spill ref + no sidecar, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestGetTrafficEventNormalized_SpillReadError_GracefulFallback: the spill object
// resolves but the stream truncates mid-read (io.ReadAll error). rawSpillBody
// must surface the error so the direction degrades — the endpoint falls back to
// the sidecar, never erroring.
func TestGetTrafficEventNormalized_SpillReadError_GracefulFallback(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	spillRef := []byte(`{"backend":"test","key":"truncated"}`)
	h.spillStore = &testSpillStore{readErr: errStub("connection reset mid-read")}

	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-readerr").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o", "/v1/chat/completions",
			nil, "", nil, "",
			"application/json", "", spillRef, nil, "", "", false, false))
	c, rec := echoCtx(http.MethodGet, "/traffic/evt-readerr/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-readerr")
	if err := h.GetTrafficEventNormalized(c); err != nil {
		t.Fatalf("GetTrafficEventNormalized: %v", err)
	}
	// The read failed halfway through, so the bytes are untrustworthy and the
	// direction degrades to empty. What must NOT happen is a 500 or a partial
	// normalize over a truncated body; 404 is the honest answer now that there
	// is no sidecar tier behind it.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a mid-read failure must degrade to 404, not %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestGetTrafficEventNormalized_DSARScrubbedBody_EmptyNormalized is the C1
// regression: after a GDPR erasure, the subject's payload body is nulled
// (dsarstore step 1: inline_*_body + *_spill_ref → NULL). This simulates that
// exact post-erasure DB state and asserts the view-time recompute resurrects NO
// prompt/response text — erasure correctness rests entirely on the body scrub.
//
// Dropping traffic_event_normalized strengthens this property rather than
// weakening it. Step 1b used to null the sidecar's normalized copy; with the
// table gone there is no second copy to null, so the erasure has one surface
// instead of two and this test no longer has to trust that step 1b ran.
func TestGetTrafficEventNormalized_DSARScrubbedBody_EmptyNormalized(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// A spill store IS wired, to prove the scrub (NULL spill ref) — not the
	// absence of a backend — is what prevents resurrection. If the scrub left a
	// ref behind, this store would happily hand back the PII.
	h.spillStore = &testSpillStore{
		data:        []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"SENSITIVE PII ssn 123-45-6789"}]}`),
		contentType: "application/json",
	}

	// Post-erasure payload row: inline bodies NULL, spill refs NULL (step 1).
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-erased").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"openai", "gpt-4o", "/v1/chat/completions",
			nil, "", nil, "",
			"application/json", "application/json", nil, nil, "", "", false, false))
	c, rec := echoCtx(http.MethodGet, "/traffic/evt-erased/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-erased")
	if err := h.GetTrafficEventNormalized(c); err != nil {
		t.Fatalf("GetTrafficEventNormalized: %v", err)
	}
	// The scrubbed row has nothing left to project, so 404 is the post-erasure
	// shape. The load-bearing assertion is the one below it: whatever the status,
	// no erased text appears in the response.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a fully scrubbed row, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SENSITIVE PII") || strings.Contains(rec.Body.String(), "123-45-6789") {
		t.Fatalf("erased PII leaked into view-time response: %s", rec.Body.String())
	}
	// The spill store is wired and holds the PII. Proving it was never consulted
	// is what makes this a test of the scrub rather than of an absent backend.
	if h.spillStore.(*testSpillStore).getCalls != 0 {
		t.Fatalf("the spill store was consulted %d times after erasure; a NULL ref must never reach it",
			h.spillStore.(*testSpillStore).getCalls)
	}
}
