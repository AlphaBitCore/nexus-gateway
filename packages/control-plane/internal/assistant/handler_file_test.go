package assistant

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"
)

// TestChatStreamEmitsFileEvent drives a turn where the model calls write_file; the
// handler must surface a structured `file` SSE event (id + download path) sourced
// from the tool's own output — so the browser's download button no longer depends on
// the model echoing the URL into its prose (e90-s7 follow-up b).
func TestChatStreamEmitsFileEvent(t *testing.T) {
	var round int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			if atomic.AddInt32(&round, 1) == 1 {
				// Round 1: the model calls write_file.
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"name\\\":\\\"report.txt\\\",\\\"content\\\":\\\"hello\\\"}\"}}]}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			// Round 2: a plain reply that does NOT echo the URL — proving the button
			// signal comes from the structured event, not the model's prose.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Saved your report.\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer gw.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.MatchExpectationsInOrder(false)
	spill := &fakeSpill{}

	h := New(Config{AIGatewayURL: gw.URL, CPBaseURL: gw.URL, SystemVK: "nvk_test", Model: "m", Pool: mock, Spill: spill})

	// DB ops for the turn: memory index (system prompt), fresh-session load (no rows →
	// start fresh), write_file quota check + insert, and the post-turn session save.
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory"`).
		WithArgs("alice").WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body"}))
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantSession"`).WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
		WithArgs("alice").WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(0)))
	mock.ExpectExec(`INSERT INTO "AssistantFile"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "AssistantSession"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	_, out := driveTurn(t, h, "alice", `{"message":"make me a report"}`)
	if !strings.Contains(out, "event: file") {
		t.Fatalf("expected a structured file event, got:\n%s", out)
	}
	if !strings.Contains(out, `"downloadPath":"/api/admin/assistant/files/`) {
		t.Fatalf("the file event must carry the download path, got:\n%s", out)
	}
}
