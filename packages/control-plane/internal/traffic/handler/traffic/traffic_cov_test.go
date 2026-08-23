// Package traffic — additional white-box coverage for the traffic detail +
// normalize read paths and the spill-body integrity gate. These exercise the
// success branches of GetTrafficEvent / GetTrafficEventNormalized (inline
// recompute and sidecar fallback), the spill-ref resolution wired into the
// detail handler, renderBody's JSON-vs-wrap split, the SSE stream branch of
// computeNormalized, and the sha256 tamper-detection refusal in resolveSpillBody.
package traffic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// trafficEventCovRow builds the column set + value row matching GetTrafficEvent's
// 95 base scan destinations plus the 10 payload-JOIN columns
// (inline_request_body, inline_response_body, request_spill_ref,
// response_spill_ref, inline_request_encoding, inline_response_encoding,
// request_truncated, response_truncated, request_size_bytes,
// response_size_bytes). Base columns 0/1/2/72 carry the non-nullable values
// (ID, Source, Timestamp, CreatedAt); every other base column is SQL NULL. The
// caller fills the first 6 payload columns to drive the body/spill branches;
// the truncation pair defaults to false/NULL (an untruncated body) and is
// overridden by trafficEventCovRowTruncated.
func trafficEventCovRow(id string, reqBody, respBody, reqSpill, respSpill []byte, reqEnc, respEnc string) *pgxmock.Rows {
	return trafficEventCovRowTruncated(id, reqBody, respBody, reqSpill, respSpill, reqEnc, respEnc, false, false, nil, nil)
}

// trafficEventCovRowTruncated is trafficEventCovRow with the truncation columns
// under the caller's control, so a handler test can assert what the detail JSON
// says about a body that was stored as a PREFIX.
func trafficEventCovRowTruncated(id string, reqBody, respBody, reqSpill, respSpill []byte, reqEnc, respEnc string,
	reqTrunc, respTrunc bool, reqSize, respSize *int64) *pgxmock.Rows {
	const n = 96 // base SELECT cols: 91 + end_user_id/session_id + artifact_refs/compliance_coverage/endpoint_type
	const extra = 10
	cols := make([]string, n+extra)
	vals := make([]any, n+extra)
	for i := range cols {
		cols[i] = "c" + itoa(i)
		vals[i] = nil
	}
	vals[0] = id
	vals[1] = "ai-gateway"
	vals[2] = tNowCov
	vals[73] = tNowCov // CreatedAt — SELECT index 73 after the +5 columns
	vals[96] = nullableBytes(reqBody)
	vals[97] = nullableBytes(respBody)
	vals[98] = nullableBytes(reqSpill)
	vals[99] = nullableBytes(respSpill)
	vals[100] = reqEnc
	vals[101] = respEnc
	vals[102] = reqTrunc
	vals[103] = respTrunc
	vals[104] = reqSize
	vals[105] = respSize
	return pgxmock.NewRows(cols).AddRow(vals...)
}

// nullableBytes maps an empty slice to SQL NULL so the *json.RawMessage /
// *[]byte scan targets receive nil (mirrors a NULL payload column).
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [4]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

var tNowCov = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func errNoRowsStub() error { return pgx.ErrNoRows }

// ── GetTrafficEvent success paths ────────────────────────────────────────────

// The detail JSON is what the drawer actually reads, so the truncation facts have
// to survive all the way to the wire — not just into the store struct. This is the
// exact shape of the ABC stg row that started this: a streamed response whose
// stored copy stops at the 100 KiB inline cutoff, in the middle of an SSE frame,
// while the provider actually sent 254432 bytes and finished normally. Rendering
// that prefix without these two fields is indistinguishable from a model that
// stopped thinking part-way.
func TestGetTrafficEvent_TruncatedBody_ReportsFlagAndTrueSizeOnTheWire(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	reqSize, respSize := int64(561168), int64(254432)
	truncatedSSE := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"x\"}}]}\n\ndata: {\"object\":\"chat.completion.chu")
	mock.ExpectQuery(`LEFT JOIN traffic_event_payload p`).
		WithArgs("evt-truncated").
		WillReturnRows(trafficEventCovRowTruncated(
			"evt-truncated",
			[]byte(`{"prompt":"hi"}`), truncatedSSE,
			nil, nil, "text", "text",
			true, true, &reqSize, &respSize))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-truncated")
	c.SetParamNames("id")
	c.SetParamValues("evt-truncated")
	if err := h.GetTrafficEvent(c); err != nil {
		t.Fatalf("GetTrafficEvent: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		RequestBodyTruncated  bool   `json:"requestBodyTruncated"`
		ResponseBodyTruncated bool   `json:"responseBodyTruncated"`
		RequestBodySizeBytes  *int64 `json:"requestBodySizeBytes"`
		ResponseBodySizeBytes *int64 `json:"responseBodySizeBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !got.RequestBodyTruncated || !got.ResponseBodyTruncated {
		t.Errorf("truncation flags missing from the detail JSON: request=%v response=%v",
			got.RequestBodyTruncated, got.ResponseBodyTruncated)
	}
	if got.ResponseBodySizeBytes == nil || *got.ResponseBodySizeBytes != respSize {
		t.Errorf("response size on the wire: want %d, got %v", respSize, got.ResponseBodySizeBytes)
	}
	if got.RequestBodySizeBytes == nil || *got.RequestBodySizeBytes != reqSize {
		t.Errorf("request size on the wire: want %d, got %v", reqSize, got.RequestBodySizeBytes)
	}
}

// The truncation flags are non-omitempty booleans, so an untruncated row must
// emit them as false rather than dropping them — a missing key and "false" read
// the same to a JS client only by accident, and the drawer branches on the key.
func TestGetTrafficEvent_WholeBody_EmitsTruncationFlagsAsFalse(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	mock.ExpectQuery(`LEFT JOIN traffic_event_payload p`).
		WithArgs("evt-whole").
		WillReturnRows(trafficEventCovRow(
			"evt-whole", []byte(`{"prompt":"hi"}`), []byte(`{"ok":true}`), nil, nil, "text", "text"))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-whole")
	c.SetParamNames("id")
	c.SetParamValues("evt-whole")
	if err := h.GetTrafficEvent(c); err != nil {
		t.Fatalf("GetTrafficEvent: %v", err)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, `"requestBodyTruncated":false`) || !strings.Contains(raw, `"responseBodyTruncated":false`) {
		t.Errorf("untruncated row must still carry both flags as false; got %s", raw)
	}
}

func TestGetTrafficEvent_InlineBodies_RendersJSONAndWrapsNonJSON(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// request body is valid JSON → passes through; response body is SSE text →
	// renderBody wraps it as a JSON string so the drawer always gets a printable
	// value.
	mock.ExpectQuery(`LEFT JOIN traffic_event_payload p`).
		WithArgs("evt-inline").
		WillReturnRows(trafficEventCovRow(
			"evt-inline",
			[]byte(`{"prompt":"hi"}`), []byte("event: done\ndata: {}\n\n"),
			nil, nil, "text", "text"))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-inline")
	c.SetParamNames("id")
	c.SetParamValues("evt-inline")
	if err := h.GetTrafficEvent(c); err != nil {
		t.Fatalf("GetTrafficEvent: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		ID           string          `json:"id"`
		RequestBody  json.RawMessage `json:"requestBody"`
		ResponseBody json.RawMessage `json:"responseBody"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got.ID != "evt-inline" {
		t.Errorf("id = %q, want evt-inline", got.ID)
	}
	// request body is valid JSON → object preserved
	var reqObj map[string]any
	if err := json.Unmarshal(got.RequestBody, &reqObj); err != nil {
		t.Fatalf("requestBody not JSON object: %s", got.RequestBody)
	}
	if reqObj["prompt"] != "hi" {
		t.Errorf("requestBody.prompt = %v, want hi", reqObj["prompt"])
	}
	// response body is non-JSON SSE → wrapped as a JSON string
	var respStr string
	if err := json.Unmarshal(got.ResponseBody, &respStr); err != nil {
		t.Fatalf("responseBody should be a JSON string, got %s", got.ResponseBody)
	}
	if !strings.Contains(respStr, "event: done") {
		t.Errorf("responseBody string lost SSE content: %q", respStr)
	}
}

func TestGetTrafficEvent_SpillResolved_FillsBodyFromStore(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	spillPayload := []byte(`{"spilled":true}`)
	sum := sha256.Sum256(spillPayload)
	ref := sharedaudit.SpillRef{Key: "k/req", ContentType: "application/json", SHA256: hex.EncodeToString(sum[:])}
	refJSON, _ := json.Marshal(ref)
	h.spillStore = &testSpillStore{data: spillPayload, contentType: "application/json"}

	// inline request body NULL + non-empty request_spill_ref → resolveSpillBody.
	mock.ExpectQuery(`LEFT JOIN traffic_event_payload p`).
		WithArgs("evt-spill").
		WillReturnRows(trafficEventCovRow("evt-spill", nil, nil, refJSON, nil, "", ""))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-spill")
	c.SetParamNames("id")
	c.SetParamValues("evt-spill")
	if err := h.GetTrafficEvent(c); err != nil {
		t.Fatalf("GetTrafficEvent: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		RequestBody json.RawMessage `json:"requestBody"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(got.RequestBody, &obj); err != nil {
		t.Fatalf("requestBody not JSON from spill: %s", got.RequestBody)
	}
	if obj["spilled"] != true {
		t.Errorf("requestBody = %s, want spilled body from store", got.RequestBody)
	}
}

func TestGetTrafficEvent_SpillResolveFails_LeavesBodyNil(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// Get error → resolveSpillBody fails → handler logs warn and leaves the
	// inline (NULL) body as-is; the response still succeeds with 200.
	h.spillStore = &testSpillStore{getErr: errStub("spill offline")}
	ref := sharedaudit.SpillRef{Key: "k/resp", ContentType: "application/json"}
	refJSON, _ := json.Marshal(ref)

	mock.ExpectQuery(`LEFT JOIN traffic_event_payload p`).
		WithArgs("evt-spill-fail").
		WillReturnRows(trafficEventCovRow("evt-spill-fail", nil, nil, nil, refJSON, "", ""))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-spill-fail")
	c.SetParamNames("id")
	c.SetParamValues("evt-spill-fail")
	if err := h.GetTrafficEvent(c); err != nil {
		t.Fatalf("GetTrafficEvent: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 even on spill failure, got %d", rec.Code)
	}
	var got struct {
		ResponseBody json.RawMessage `json:"responseBody"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ResponseBody != nil {
		t.Errorf("responseBody should stay nil when spill resolve fails, got %s", got.ResponseBody)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

// ── resolveSpillBody integrity gate ──────────────────────────────────────────

func TestResolveSpillBody_SHA256Mismatch_RefusesTamperedBlob(t *testing.T) {
	h := newHandlerNilPool()
	h.spillStore = &testSpillStore{data: []byte(`{"ok":1}`), contentType: "application/json"}
	// Recorded sha256 does not match the fetched bytes → integrity gate fires.
	ref := sharedaudit.SpillRef{Key: "k", ContentType: "application/json", SHA256: "deadbeef"}
	refJSON, _ := json.Marshal(ref)
	_, err := h.resolveSpillBody(context.Background(), refJSON)
	if err == nil {
		t.Fatal("expected integrity error on sha256 mismatch")
	}
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("error = %q, want sha256 integrity message", err.Error())
	}
}

func TestResolveSpillBody_SHA256Match_ReturnsBody(t *testing.T) {
	h := newHandlerNilPool()
	payload := []byte(`{"ok":1}`)
	sum := sha256.Sum256(payload)
	h.spillStore = &testSpillStore{data: payload, contentType: "application/json"}
	ref := sharedaudit.SpillRef{Key: "k", ContentType: "application/json", SHA256: strings.ToUpper(hex.EncodeToString(sum[:]))}
	refJSON, _ := json.Marshal(ref)
	got, err := h.resolveSpillBody(context.Background(), refJSON)
	if err != nil {
		t.Fatalf("unexpected error on matching sha256: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got %s, want %s", got, payload)
	}
}

// ── GetTrafficEventNormalized: inline recompute + sidecar fallback ────────────

// normalizeInputCols mirrors GetTrafficEventForNormalize's SELECT order:
// ingress_format, model, path, req_body, req_enc, resp_body, resp_enc,
// req_content_type, resp_content_type, req_spill_ref, resp_spill_ref,
// endpoint_type, artifact_refs, request_truncated, response_truncated.
var normalizeInputCols = []string{
	"ingress_format", "model", "path",
	"req_body", "req_enc", "resp_body", "resp_enc",
	"req_ct", "resp_ct", "req_spill", "resp_spill",
	"endpoint_type", "artifact_refs",
	"req_truncated", "resp_truncated",
}

func TestGetTrafficEventNormalized_InlineRecompute_ReturnsComputed(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// A captured Anthropic request + SSE response → computeNormalized runs the
	// shared chain. The SSE response content type drives the stream branch.
	reqBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	respBody := []byte("event: message_stop\ndata: {}\n\n")
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-norm").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"anthropic", "claude-opus-4-7", "/v1/messages",
			reqBody, "", respBody, "",
			"application/json", "text/event-stream", nil, nil, "", "", false, false))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-norm/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-norm")
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
	if got.TrafficEventID != "evt-norm" {
		t.Errorf("trafficEventId = %q, want evt-norm", got.TrafficEventID)
	}
	if got.NormalizeVersion == "" {
		t.Error("expected a normalize version stamp on the computed payload")
	}
	// Both directions had bodies → both must carry a recompute status.
	if got.RequestStatus == nil {
		t.Error("expected request status from recompute")
	}
	if got.ResponseStatus == nil {
		t.Error("expected response status from recompute (SSE stream branch)")
	}
}

// A row the parent lookup finds but which carries no recoverable body — capture
// was off, or the bodies have gone to retention — is unavailable, not an error.
//
// This test used to assert a 200 carrying the stored traffic_event_normalized
// sidecar. That tier is gone with the table, so the assertion inverts: exactly
// ONE query is issued (the parent lookup) and the answer is 404. The
// single-query expectation is the part that matters — it is what would fail if
// the sidecar SELECT were ever reintroduced.
func TestGetTrafficEventNormalized_NoRecoverableBody_Returns404(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-empty").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"anthropic", "m", "/v1/messages", nil, "", nil, "", "", "", nil, nil, "", "", false, false))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-empty/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-empty")
	_ = h.GetTrafficEventNormalized(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no body is recoverable, got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the parent lookup must be the only query issued: %v", err)
	}
}
