package hubapi

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// stubMQProducer is a minimal mq.Producer satisfying the Enqueue
// signature the DLQ retry handler calls. enqueued captures the calls for
// happy-path assertion; enqueueErr lets tests force the failure branch.
type stubMQProducer struct {
	enqueueErr error
	enqueued   []struct {
		Subject string
		Data    []byte
	}
}

func (s *stubMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (s *stubMQProducer) Enqueue(_ context.Context, subject string, data []byte) error {
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	s.enqueued = append(s.enqueued, struct {
		Subject string
		Data    []byte
	}{subject, append([]byte(nil), data...)})
	return nil
}
func (s *stubMQProducer) Close() error { return nil }

// silentDLQLogger discards output so tests don't spam stderr.
func silentDLQLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// echoTestContext builds an echo.Context for an HTTP request the handler
// can run against. Returns the recorder so callers can assert response.
func echoTestContext(t *testing.T, method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	return e.NewContext(r, w), w
}

func TestListDLQ_NilPoolReturns503(t *testing.T) {
	h := &HubAPI{Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet, "/api/hub/dlq", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// countRows builds the single-column COUNT(*) result pgxmock hands back
// to the QueryRow Scan in ListDLQ.
func countRows(total int) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"count"}).AddRow(total)
}

func TestListDLQ_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// ListDLQ runs COUNT first (no subject filter → no args), then the
	// page query with (limit, offset). Default limit 50, offset 0.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event_dlq`).
		WillReturnRows(countRows(1))
	rows := pgxmock.NewRows([]string{
		"id", "msg_id", "subject", "delivery_count", "last_error",
		"first_seen_at", "dlq_inserted_at", "payload_size",
	}).AddRow(
		"11111111-1111-1111-1111-111111111111",
		"msg-1", "nexus.event.gateway", 5, "23505 duplicate key",
		fixedTime(2026, 5, 26, 10, 0), fixedTime(2026, 5, 26, 10, 5), 1234,
	)
	mock.ExpectQuery(`ORDER BY dlq_inserted_at DESC LIMIT \$1 OFFSET \$2`).
		WithArgs(50, 0).
		WillReturnRows(rows)

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet, "/api/hub/dlq", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp dlqListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(resp.Rows))
	}
	if resp.Rows[0].MsgID != "msg-1" {
		t.Errorf("msgId = %q, want msg-1", resp.Rows[0].MsgID)
	}
	if resp.Rows[0].LastError != "23505 duplicate key" {
		t.Errorf("lastError = %q, want known err string", resp.Rows[0].LastError)
	}
	// total drives the offset-pagination footer.
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListDLQ_SubjectFilterAndOffset(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// COUNT carries the subject filter as $1; the page query carries
	// (subject, limit, offset) so the footer total agrees with the slice.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event_dlq`).
		WithArgs("nexus.event.compliance").
		WillReturnRows(countRows(7))
	rows := pgxmock.NewRows([]string{
		"id", "msg_id", "subject", "delivery_count", "last_error",
		"first_seen_at", "dlq_inserted_at", "payload_size",
	}).
		AddRow("id-a", "m-a", "nexus.event.compliance", 5, "err-a",
			fixedTime(2026, 5, 26, 11, 0), fixedTime(2026, 5, 26, 11, 5), 100).
		AddRow("id-b", "m-b", "nexus.event.compliance", 6, "err-b",
			fixedTime(2026, 5, 26, 10, 0), fixedTime(2026, 5, 26, 10, 5), 200)
	mock.ExpectQuery(`subject = \$1.*ORDER BY dlq_inserted_at DESC LIMIT \$2 OFFSET \$3`).
		WithArgs("nexus.event.compliance", 2, 2).
		WillReturnRows(rows)

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet,
		"/api/hub/dlq?subject=nexus.event.compliance&limit=2&offset=2", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp dlqListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(resp.Rows))
	}
	if resp.Total != 7 {
		t.Errorf("total = %d, want 7", resp.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (offset/subject not forwarded as expected): %v", err)
	}
}

func TestListDLQ_InvalidOffsetDefaultsToZero(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// A non-numeric offset is ignored → the page query runs with offset 0.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event_dlq`).
		WillReturnRows(countRows(0))
	mock.ExpectQuery(`ORDER BY dlq_inserted_at DESC LIMIT \$1 OFFSET \$2`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "msg_id", "subject", "delivery_count", "last_error",
			"first_seen_at", "dlq_inserted_at", "payload_size",
		}))

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet, "/api/hub/dlq?offset=not-a-number", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListDLQ_LimitCapped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event_dlq`).
		WillReturnRows(countRows(0))
	// limit=99999 from caller → capped at 200.
	mock.ExpectQuery(`ORDER BY dlq_inserted_at DESC LIMIT \$1 OFFSET \$2`).
		WithArgs(200, 0).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "msg_id", "subject", "delivery_count", "last_error",
			"first_seen_at", "dlq_inserted_at", "payload_size",
		}))

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet, "/api/hub/dlq?limit=99999", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListDLQ_CountQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// The COUNT runs first; its failure short-circuits to 500.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event_dlq`).
		WillReturnError(errors.New("conn refused"))

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet, "/api/hub/dlq", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestListDLQ_PageQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// COUNT succeeds, the page query fails → 500.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event_dlq`).
		WillReturnRows(countRows(3))
	mock.ExpectQuery(`ORDER BY dlq_inserted_at DESC LIMIT \$1 OFFSET \$2`).
		WillReturnError(errors.New("conn refused"))

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodGet, "/api/hub/dlq", "")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRetryDLQ_NilProducerReturns503(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	h := &HubAPI{DLQPool: mock, Logger: silentDLQLogger()}
	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq/abc/retry", "")
	c.SetParamNames("id")
	c.SetParamValues("abc")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestRetryDLQ_HappyPath_RepublishAndDelete(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const id = "22222222-2222-2222-2222-222222222222"
	mock.ExpectQuery(`SELECT subject, payload FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"subject", "payload"}).
			AddRow("nexus.event.gateway", []byte(`{"id":"orig-msg"}`)))
	mock.ExpectExec(`DELETE FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))

	prod := &stubMQProducer{}
	h := &HubAPI{DLQPool: mock, DLQProducer: prod, Logger: silentDLQLogger()}

	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq/"+id+"/retry", "")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(prod.enqueued) != 1 || prod.enqueued[0].Subject != "nexus.event.gateway" {
		t.Errorf("Enqueue calls = %+v, want 1 to nexus.event.gateway", prod.enqueued)
	}
}

func TestRetryDLQ_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const id = "33333333-3333-3333-3333-333333333333"
	mock.ExpectQuery(`SELECT subject, payload FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnError(pgx.ErrNoRows)

	prod := &stubMQProducer{}
	h := &HubAPI{DLQPool: mock, DLQProducer: prod, Logger: silentDLQLogger()}

	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq/"+id+"/retry", "")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if len(prod.enqueued) != 0 {
		t.Error("Enqueue must not fire on not-found")
	}
}

func TestRetryDLQ_PublishFailureLeavesRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const id = "44444444-4444-4444-4444-444444444444"
	mock.ExpectQuery(`SELECT subject, payload FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"subject", "payload"}).
			AddRow("nexus.event.compliance", []byte(`{}`)))
	// NO DELETE expected — republish fails, row must stay.

	prod := &stubMQProducer{enqueueErr: errors.New("nats: connection closed")}
	h := &HubAPI{DLQPool: mock, DLQProducer: prod, Logger: silentDLQLogger()}

	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq/"+id+"/retry", "")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRetryDLQ_DeleteFailureReturnsWarn(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const id = "55555555-5555-5555-5555-555555555555"
	mock.ExpectQuery(`SELECT subject, payload FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"subject", "payload"}).
			AddRow("nexus.event.agent", []byte(`{}`)))
	mock.ExpectExec(`DELETE FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnError(errors.New("conn dead"))

	prod := &stubMQProducer{}
	h := &HubAPI{DLQPool: mock, DLQProducer: prod, Logger: silentDLQLogger()}

	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq/"+id+"/retry", "")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	// Publish succeeded → 200 OK with deleteWarn flag (the broker already
	// has the message; row staying around is logged for ops follow-up).
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["deleteWarn"] != true {
		t.Errorf("deleteWarn = %v, want true", body["deleteWarn"])
	}
	if len(prod.enqueued) != 1 {
		t.Errorf("Enqueue calls = %d, want 1 (publish must have succeeded)", len(prod.enqueued))
	}
}

// TestRetryDLQ_QueryError covers the generic SELECT-failed branch
// (anything that's not pgx.ErrNoRows). Returns 500.
func TestRetryDLQ_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	const id = "66666666-6666-6666-6666-666666666666"
	mock.ExpectQuery(`SELECT subject, payload FROM traffic_event_dlq WHERE id`).
		WithArgs(id).
		WillReturnError(errors.New("conn dead"))

	prod := &stubMQProducer{}
	h := &HubAPI{DLQPool: mock, DLQProducer: prod, Logger: silentDLQLogger()}

	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq/"+id+"/retry", "")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRetryDLQ_EmptyIDReturns400(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	prod := &stubMQProducer{}
	h := &HubAPI{DLQPool: mock, DLQProducer: prod, Logger: silentDLQLogger()}

	c, w := echoTestContext(t, http.MethodPost, "/api/hub/dlq//retry", "")
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// fixedTime returns a deterministic time.Time for row scaffolding. pgxmock's
// AddRow accepts any concrete value; the destination time.Time receives it
// via the standard pgx Scan path.
func fixedTime(y, mo, d, h, mi int) time.Time {
	return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC)
}
