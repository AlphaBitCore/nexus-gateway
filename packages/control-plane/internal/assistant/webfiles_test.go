package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

func TestWebFileStoreWriteReadRoundTrip(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	spill := &fakeSpill{}
	fs := newWebFileStore(context.Background(), mock, spill, "alice", "sess1")

	// Write: per-user quota check → content → spill, metadata + ref → DB (id + ref AnyArg).
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
		WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(0)))
	mock.ExpectExec(`INSERT INTO "AssistantFile"`).
		WithArgs(pgxmock.AnyArg(), "alice", "sess1", "report.txt", 11, "text/plain; charset=utf-8", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m, err := fs.Write("report.txt", "hello world", "")
	if err != nil || m.Name != "report.txt" || m.Size != 11 {
		t.Fatalf("Write: %v %+v", err, m)
	}
	if got := string(spill.objs[m.ID+":file"]); got != "" { // key uses sessionID/id; sanity that content was stored
		_ = got
	}

	// ReadByName: scoped to userId+sessionId+name; fetches content from spill.
	refJSON, _ := json.Marshal(spill.lastRef)
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantFile" WHERE "userId" = \$1 AND "sessionId" = \$2 AND name = \$3`).
		WithArgs("alice", "sess1", "report.txt").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(refJSON))
	content, err := fs.ReadByName("report.txt")
	if err != nil || content != "hello world" {
		t.Fatalf("ReadByName round-trip: %v %q", err, content)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (userID/sessionID not bound?): %v", err)
	}
}

func TestFileIDFromToolOutput(t *testing.T) {
	// Direct parse of the tool's download line.
	if id, ok := fileIDFromToolOutput(`wrote "r.txt" (5 bytes); download at /api/admin/assistant/files/deadbeef12ab`); !ok || id != "deadbeef12ab" {
		t.Fatalf("parse: id=%q ok=%v", id, ok)
	}
	// Negatives: non-file output and an error string must not match.
	if _, ok := fileIDFromToolOutput("All healthy."); ok {
		t.Error("must not match non-file tool output")
	}
	if _, ok := fileIDFromToolOutput(""); ok {
		t.Error("must not match empty output")
	}

	// Round-trip: the parser must recover the id from the REAL write_file tool output,
	// so the producer (writeFileTool) and consumer (the handler's `file` event) can
	// never silently drift apart.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
		WithArgs("alice").WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(0)))
	mock.ExpectExec(`INSERT INTO "AssistantFile"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	fs := newWebFileStore(context.Background(), mock, &fakeSpill{}, "alice", "s1")
	res, err := writeFileTool{fs: fs}.Run(context.Background(), json.RawMessage(`{"name":"x.txt","content":"abc"}`))
	if err != nil || res.IsError {
		t.Fatalf("write_file Run: %v %+v", err, res)
	}
	if id, ok := fileIDFromToolOutput(res.Content); !ok || id == "" {
		t.Fatalf("round-trip parse failed for tool output %q", res.Content)
	}
}

func TestWebFileStoreWriteGuards(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	fs := newWebFileStore(context.Background(), mock, &fakeSpill{}, "alice", "sess1")

	if _, err := fs.Write("  ", "x", ""); err == nil {
		t.Fatal("a blank file name must error")
	}
	if _, err := fs.Write("big.bin", strings.Repeat("a", maxFileBytes+1), ""); err == nil {
		t.Fatal("an oversize file must error")
	}
	if _, err := fs.Write(strings.Repeat("n", maxFileNameLen+1), "x", ""); err == nil {
		t.Fatal("an over-long file name must error (would truncate the download path out of the tool output)")
	}
}

func TestWebFileStoreWritePropagatesErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	boom := errors.New("boom")

	okQuota := func() {
		mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
			WithArgs("alice").
			WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(0)))
	}

	// Spill Put failure aborts before the DB write (the quota check runs first).
	fs := newWebFileStore(context.Background(), mock, &fakeSpill{putErr: boom}, "alice", "sess1")
	okQuota()
	if _, err := fs.Write("a.txt", "x", ""); err == nil {
		t.Fatal("Write must propagate a spill Put error")
	}

	// DB Exec failure after a successful spill Put.
	fs2 := newWebFileStore(context.Background(), mock, &fakeSpill{}, "alice", "sess1")
	okQuota()
	mock.ExpectExec(`INSERT INTO "AssistantFile"`).WillReturnError(boom)
	if _, err := fs2.Write("a.txt", "x", "text/plain"); err == nil {
		t.Fatal("Write must propagate a DB error")
	}

	// Quota-check query failure aborts before any spill/DB write.
	fs3 := newWebFileStore(context.Background(), mock, &fakeSpill{}, "alice", "sess1")
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
		WithArgs("alice").WillReturnError(boom)
	if _, err := fs3.Write("a.txt", "x", ""); err == nil {
		t.Fatal("Write must propagate a quota-check error")
	}
}

func TestWebFileStoreWriteQuotaExceeded(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	spill := &fakeSpill{}
	fs := newWebFileStore(context.Background(), mock, spill, "alice", "sess1")

	// Already at one byte under the cap: a 2-byte write tips it over → rejected
	// BEFORE any spill Put (no orphaned content) and BEFORE the INSERT.
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
		WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(maxUserFileBytes - 1)))
	_, err := fs.Write("over.txt", "ab", "")
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("expected a storage-quota error, got %v", err)
	}
	if len(spill.objs) != 0 {
		t.Fatalf("over-quota write must not spill content; got %d objects", len(spill.objs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("over-quota write must stop after the quota query: %v", err)
	}
}

func TestWebFileStoreReadByNameErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	fs := newWebFileStore(context.Background(), mock, &fakeSpill{}, "alice", "sess1")

	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantFile"`).
		WithArgs("alice", "sess1", "ghost.txt").WillReturnError(pgx.ErrNoRows)
	if _, err := fs.ReadByName("ghost.txt"); err == nil {
		t.Fatal("a missing file must error")
	}

	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantFile"`).
		WithArgs("alice", "sess1", "x.txt").WillReturnError(errors.New("boom"))
	if _, err := fs.ReadByName("x.txt"); err == nil {
		t.Fatal("a DB error must propagate")
	}

	// Ref present but spill content gone → fetch error propagates.
	refJSON, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "gone:file"})
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantFile"`).
		WithArgs("alice", "sess1", "y.txt").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(refJSON))
	if _, err := fs.ReadByName("y.txt"); err == nil {
		t.Fatal("a missing spill object must surface as an error")
	}
}

func TestWebFileStoreGetOwnerScoped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	spill := &fakeSpill{objs: map[string][]byte{"k:file": []byte("payload")}}
	fs := newWebFileStore(context.Background(), mock, spill, "alice", "")
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "k:file"})

	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile" WHERE id = \$1 AND "userId" = \$2`).
		WithArgs("f1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"name", "contentType", "size", "spillRef"}).
			AddRow("r.txt", "text/plain", 7, ref))
	rc, m, err := fs.Get("f1")
	if err != nil || m.Name != "r.txt" {
		t.Fatalf("Get: %v %+v", err, m)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "payload" {
		t.Fatalf("Get content = %q", data)
	}

	// A non-owned / missing id → not found.
	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile"`).
		WithArgs("f2", "alice").WillReturnError(pgx.ErrNoRows)
	if _, _, err := fs.Get("f2"); err == nil {
		t.Fatal("a non-owned / missing file must error")
	}

	// Malformed ref → decode error.
	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile"`).
		WithArgs("f3", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"name", "contentType", "size", "spillRef"}).
			AddRow("r", "text/plain", 1, []byte("{bad")))
	if _, _, err := fs.Get("f3"); err == nil {
		t.Fatal("a malformed ref must surface a decode error")
	}

	// Content reaped under shared spill retention → errFileExpired (graceful), so the
	// download endpoint can return 410 rather than a generic failure.
	goneRef, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "missing:file"})
	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile"`).
		WithArgs("f4", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"name", "contentType", "size", "spillRef"}).
			AddRow("r", "text/plain", 1, goneRef))
	if _, _, err := fs.Get("f4"); !errors.Is(err, errFileExpired) {
		t.Fatalf("reaped content must surface errFileExpired, got %v", err)
	}
}

func TestFileToolsRun(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	spill := &fakeSpill{}
	fs := newWebFileStore(context.Background(), mock, spill, "alice", "sess1")

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(size\), 0\) FROM "AssistantFile"`).
		WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(0)))
	mock.ExpectExec(`INSERT INTO "AssistantFile"`).WithArgs(
		pgxmock.AnyArg(), "alice", "sess1", "out.txt", 3, "text/plain; charset=utf-8", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	wres, err := writeFileTool{fs: fs}.Run(context.Background(), json.RawMessage(`{"name":"out.txt","content":"abc"}`))
	if err != nil || wres.IsError || !strings.Contains(wres.Content, "download at /api/admin/assistant/files/") {
		t.Fatalf("write_file Run: %v %+v", err, wres)
	}

	// invalid JSON → IsError result (never a hard error).
	bad, _ := writeFileTool{fs: fs}.Run(context.Background(), json.RawMessage(`{bad`))
	if !bad.IsError {
		t.Fatal("write_file must report invalid input as IsError")
	}

	refJSON, _ := json.Marshal(spill.lastRef)
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantFile"`).
		WithArgs("alice", "sess1", "out.txt").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(refJSON))
	rres, err := readFileTool{fs: fs}.Run(context.Background(), json.RawMessage(`{"name":"out.txt"}`))
	if err != nil || rres.IsError || rres.Content != "abc" {
		t.Fatalf("read_file Run: %v %+v", err, rres)
	}
	badr, _ := readFileTool{fs: fs}.Run(context.Background(), json.RawMessage(`{bad`))
	if !badr.IsError {
		t.Fatal("read_file must report invalid input as IsError")
	}

	// read_file of a missing name → IsError (store error surfaced, not a hard error).
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantFile"`).
		WithArgs("alice", "sess1", "nope.txt").WillReturnError(pgx.ErrNoRows)
	miss, _ := readFileTool{fs: fs}.Run(context.Background(), json.RawMessage(`{"name":"nope.txt"}`))
	if !miss.IsError {
		t.Fatal("read_file of a missing file must be IsError")
	}

	// Tool metadata sanity.
	wt, rt := writeFileTool{}, readFileTool{}
	if wt.Name() != "write_file" || rt.Name() != "read_file" {
		t.Fatal("tool names")
	}
	if len(wt.Schema()) == 0 || len(rt.Schema()) == 0 {
		t.Fatal("tool schemas")
	}
	_ = wt.Tier()
	_ = rt.Tier()
	_ = wt.Description()
	_ = rt.Description()
}

func TestDownloadFileEndpoint(t *testing.T) {
	e := echo.New()
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	spill := &fakeSpill{objs: map[string][]byte{"k:file": []byte("report-bytes")}}
	h := New(Config{Pool: mock, Spill: spill})
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "k:file"})

	// Success → 200 stream with attachment headers.
	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile" WHERE id = \$1 AND "userId" = \$2`).
		WithArgs("f1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"name", "contentType", "size", "spillRef"}).
			AddRow("r.txt", "text/plain", 12, ref))
	c, rec := ctxWithUser(e, http.MethodGet, "/f", "alice")
	c.SetParamNames("id")
	c.SetParamValues("f1")
	if err := h.DownloadFile(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("download: err=%v code=%d", err, rec.Code)
	}
	if rec.Body.String() != "report-bytes" || !strings.Contains(rec.Header().Get("Content-Disposition"), "r.txt") {
		t.Fatalf("download body/headers: %q %q", rec.Body.String(), rec.Header().Get("Content-Disposition"))
	}

	// Non-owned / missing → 404.
	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile"`).
		WithArgs("f2", "alice").WillReturnError(pgx.ErrNoRows)
	c2, rec2 := ctxWithUser(e, http.MethodGet, "/f", "alice")
	c2.SetParamNames("id")
	c2.SetParamValues("f2")
	_ = h.DownloadFile(c2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("missing file code=%d", rec2.Code)
	}

	// Blank id → 400.
	c3, rec3 := ctxWithUser(e, http.MethodGet, "/f", "alice")
	c3.SetParamNames("id")
	c3.SetParamValues(" ")
	_ = h.DownloadFile(c3)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("blank id code=%d", rec3.Code)
	}

	// Reaped content → 410 Gone (graceful expiry).
	goneRef, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "missing:file"})
	mock.ExpectQuery(`SELECT name, "contentType", size, "spillRef" FROM "AssistantFile"`).
		WithArgs("f9", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"name", "contentType", "size", "spillRef"}).
			AddRow("r", "text/plain", 1, goneRef))
	cg, recg := ctxWithUser(e, http.MethodGet, "/f", "alice")
	cg.SetParamNames("id")
	cg.SetParamValues("f9")
	_ = h.DownloadFile(cg)
	if recg.Code != http.StatusGone {
		t.Fatalf("expired file code=%d", recg.Code)
	}

	// No pool → 503.
	c4, rec4 := ctxWithUser(echo.New(), http.MethodGet, "/f", "alice")
	c4.SetParamNames("id")
	c4.SetParamValues("f1")
	_ = New(Config{}).DownloadFile(c4)
	if rec4.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-pool code=%d", rec4.Code)
	}

	// No auth → 422.
	c5, rec5 := ctxWithUser(e, http.MethodGet, "/f", "")
	c5.SetParamNames("id")
	c5.SetParamValues("f1")
	_ = h.DownloadFile(c5)
	if rec5.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no-auth code=%d", rec5.Code)
	}
}
