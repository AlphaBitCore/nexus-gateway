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
// 89 base scan destinations plus the 6 payload-JOIN columns
// (inline_request_body, inline_response_body, request_spill_ref,
// response_spill_ref, inline_request_encoding, inline_response_encoding). Base
// columns 0/1/2/66 carry the non-nullable values (ID, Source, Timestamp,
// CreatedAt); every other base column is SQL NULL. The caller fills the 6
// payload columns (indices 89..94) to drive the body/spill branches.
func trafficEventCovRow(id string, reqBody, respBody, reqSpill, respSpill []byte, reqEnc, respEnc string) *pgxmock.Rows {
	const n = 89
	const extra = 6
	cols := make([]string, n+extra)
	vals := make([]any, n+extra)
	for i := range cols {
		cols[i] = "c" + itoa(i)
		vals[i] = nil
	}
	vals[0] = id
	vals[1] = "ai-gateway"
	vals[2] = tNowCov
	vals[66] = tNowCov
	vals[89] = nullableBytes(reqBody)
	vals[90] = nullableBytes(respBody)
	vals[91] = nullableBytes(reqSpill)
	vals[92] = nullableBytes(respSpill)
	vals[93] = reqEnc
	vals[94] = respEnc
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
// req_content_type, resp_content_type, req_spill_ref, resp_spill_ref.
var normalizeInputCols = []string{
	"ingress_format", "model", "path",
	"req_body", "req_enc", "resp_body", "resp_enc",
	"req_ct", "resp_ct", "req_spill", "resp_spill",
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
			"application/json", "text/event-stream", nil, nil))

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

func TestGetTrafficEventNormalized_NoInlineBody_FallsBackToSidecar(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	// First query: NormalizeInput with empty bodies → recompute skipped.
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-fb").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"anthropic", "claude-opus-4-7", "/v1/messages",
			nil, "", nil, "", "", "", nil, nil))
	// Second query: stored sidecar row is returned.
	normCols := []string{"traffic_event_id", "request_normalized", "response_normalized",
		"request_status", "response_status", "request_error_reason", "response_error_reason",
		"request_redaction_spans", "response_redaction_spans", "normalize_version", "created_at"}
	mock.ExpectQuery(`FROM traffic_event_normalized`).
		WithArgs("evt-fb").
		WillReturnRows(pgxmock.NewRows(normCols).AddRow(
			"evt-fb", []byte(`{"r":1}`), nil,
			sptr("ok"), nil, nil, nil, nil, nil, "1", tNowCov))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-fb/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-fb")
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
	if got.TrafficEventID != "evt-fb" {
		t.Errorf("trafficEventId = %q, want evt-fb (sidecar fallback)", got.TrafficEventID)
	}
	if string(got.RequestNormalized) != `{"r":1}` {
		t.Errorf("requestNormalized = %s, want stored sidecar value", got.RequestNormalized)
	}
}

func TestGetTrafficEventNormalized_NoInlineNoSidecar_Returns404(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-empty").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"anthropic", "m", "/v1/messages", nil, "", nil, "", "", "", nil, nil))
	mock.ExpectQuery(`FROM traffic_event_normalized`).
		WithArgs("evt-empty").
		WillReturnError(errNoRowsStub())

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-empty/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-empty")
	_ = h.GetTrafficEventNormalized(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no inline body and no sidecar, got %d", rec.Code)
	}
}

func TestGetTrafficEventNormalized_SidecarFallbackDBError_Returns500(t *testing.T) {
	h, mock := newHandlerWithMock(t)
	mock.ExpectQuery(`COALESCE\(a.ingress_format`).
		WithArgs("evt-fb-err").
		WillReturnRows(pgxmock.NewRows(normalizeInputCols).AddRow(
			"anthropic", "m", "/v1/messages", nil, "", nil, "", "", "", nil, nil))
	mock.ExpectQuery(`FROM traffic_event_normalized`).
		WithArgs("evt-fb-err").
		WillReturnError(errStub("db down"))

	c, rec := echoCtx(http.MethodGet, "/traffic/evt-fb-err/normalized")
	c.SetParamNames("id")
	c.SetParamValues("evt-fb-err")
	_ = h.GetTrafficEventNormalized(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on sidecar fallback DB error, got %d", rec.Code)
	}
}

func sptr(s string) *string { return &s }
