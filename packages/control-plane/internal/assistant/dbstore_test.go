package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// fakeSpill is an in-memory SpillStore for store tests: Put stashes the bytes under
// a deterministic key and returns a ref; Get returns them back. putErr/getErr force
// the failure paths.
type fakeSpill struct {
	objs    map[string][]byte
	lastRef audit.SpillRef
	putErr  error
	getErr  error
}

func (f *fakeSpill) Put(_ context.Context, r io.Reader, size int64, opts spillstore.PutOptions) (audit.SpillRef, error) {
	if f.putErr != nil {
		return audit.SpillRef{}, f.putErr
	}
	b, _ := io.ReadAll(r)
	if f.objs == nil {
		f.objs = map[string][]byte{}
	}
	key := opts.EventID + ":" + opts.Direction
	f.objs[key] = b
	f.lastRef = audit.SpillRef{Backend: "fake", Key: key, Size: size, ContentType: opts.ContentType}
	return f.lastRef, nil
}

func (f *fakeSpill) Get(_ context.Context, ref audit.SpillRef) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.objs[ref.Key]
	if !ok {
		return nil, spillstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeSpill) Delete(_ context.Context, ref audit.SpillRef) error {
	delete(f.objs, ref.Key)
	return nil
}
func (f *fakeSpill) Sweep(context.Context, time.Time) (int, error)  { return 0, nil }
func (f *fakeSpill) Stat(context.Context) (spillstore.Stats, error) { return spillstore.Stats{}, nil }
func (f *fakeSpill) Backend() string                                { return "fake" }

var _ spillstore.SpillStore = (*fakeSpill)(nil)

// Every dbStore/dbMemory query is asserted to carry the bound userID — that is the
// per-user isolation guarantee (I3): a store bound to "alice" can only ever read or
// write alice's rows, and the transcript content is reachable only through that row.

func TestDBStoreSaveAndLoadRoundTrip(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	spill := &fakeSpill{}
	s := newDBStore(context.Background(), mock, spill, "alice")
	sess := &agent.Session{ID: "s1", Messages: []agent.Message{agent.TextMessage(agent.RoleUser, "is it healthy?")}}

	// Save: content → spill, metadata + ref → DB (the ref is AnyArg JSON).
	mock.ExpectExec(`INSERT INTO "AssistantSession"`).
		WithArgs("s1", "alice", "is it healthy?", 1, 1, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := s.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := string(spill.objs["s1:transcript"]); !strings.Contains(got, "is it healthy?") {
		t.Fatalf("transcript content not written to spill: %q", got)
	}

	// Load: read the ref from DB (scoped by userID), then fetch content from spill.
	refJSON, _ := json.Marshal(spill.lastRef)
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(refJSON))
	loaded, err := s.Load("s1")
	if err != nil || loaded.ID != "s1" || len(loaded.Messages) != 1 {
		t.Fatalf("Load round-trip: %v %+v", err, loaded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (userID not bound into the query?): %v", err)
	}
}

func TestDBStoreLoadNotFoundAndNullRef(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s := newDBStore(context.Background(), mock, &fakeSpill{}, "alice")

	// A cross-user / missing id surfaces as not-found (no leak).
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantSession"`).
		WithArgs("s2", "alice").
		WillReturnError(pgx.ErrNoRows)
	if _, err := s.Load("s2"); err == nil {
		t.Fatal("loading another user's / missing session must error (not-found)")
	}

	// A row that exists but has no ref yet → empty session, no spill fetch.
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantSession"`).
		WithArgs("s3", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow([]byte("null")))
	sess, err := s.Load("s3")
	if err != nil || len(sess.Messages) != 0 {
		t.Fatalf("null-ref session must load empty: %v %+v", err, sess)
	}

	// Content expired/reaped under shared spill retention (Get → ErrNotFound) while
	// the metadata row survives → degrade to an empty session, not a hard error.
	refGone, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "gone:transcript"})
	mock.ExpectQuery(`SELECT "spillRef" FROM "AssistantSession"`).
		WithArgs("s4", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(refGone))
	sess4, err := s.Load("s4")
	if err != nil || len(sess4.Messages) != 0 {
		t.Fatalf("expired content must load as empty: %v %+v", err, sess4)
	}
}

func TestDBStoreSavePropagatesSpillAndDBErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	boom := errors.New("boom")

	// Spill Put failure aborts Save before touching the DB.
	s := newDBStore(context.Background(), mock, &fakeSpill{putErr: boom}, "alice")
	if err := s.Save(&agent.Session{ID: "s1"}); err == nil {
		t.Fatal("Save must propagate a spill Put error")
	}

	// DB Exec failure after a successful spill Put.
	s2 := newDBStore(context.Background(), mock, &fakeSpill{}, "alice")
	mock.ExpectExec(`INSERT INTO "AssistantSession"`).WillReturnError(boom)
	if err := s2.Save(&agent.Session{ID: "s1"}); err == nil {
		t.Fatal("Save must propagate a DB error")
	}
}

func TestDBStoreLoadPropagatesErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	boom := errors.New("boom")

	// Non-not-found DB error.
	s := newDBStore(context.Background(), mock, &fakeSpill{}, "alice")
	mock.ExpectQuery(`SELECT "spillRef"`).WithArgs("s1", "alice").WillReturnError(boom)
	if _, err := s.Load("s1"); err == nil {
		t.Fatal("Load must propagate a non-not-found DB error")
	}

	// DB returns a ref but spill Get fails.
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	s2 := newDBStore(context.Background(), mock, &fakeSpill{getErr: boom}, "alice")
	mock.ExpectQuery(`SELECT "spillRef"`).WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(ref))
	if _, err := s2.Load("s1"); err == nil {
		t.Fatal("Load must propagate a spill Get error")
	}
}

func TestDBStoreList(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	s := newDBStore(context.Background(), mock, &fakeSpill{}, "alice")
	mock.ExpectQuery(`SELECT id, title, "updatedAt" FROM "AssistantSession" WHERE "userId" = \$1`).
		WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "updatedAt"}).AddRow("s1", "hi", time.Now()))
	metas, err := s.List()
	if err != nil || len(metas) != 1 || metas[0].ID != "s1" {
		t.Fatalf("List: %v %+v", err, metas)
	}

	mock.ExpectQuery(`SELECT id, title`).WithArgs("alice").WillReturnError(errors.New("boom"))
	if _, err := s.List(); err == nil {
		t.Fatal("List must propagate a DB error")
	}
}

func TestDBStoreDelete(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": []byte("[]"), "s1/file1": []byte("artifact")}}
	s := newDBStore(context.Background(), mock, spill, "alice")

	// Happy path: delete the row (scoped to alice) + its spilled transcript, then the
	// session's sandbox files (rows + their spill content) so the quota is reclaimed.
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	fileRef, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1/file1"})
	mock.ExpectQuery(`DELETE FROM "AssistantSession" WHERE id = \$1 AND "userId" = \$2 RETURNING "spillRef"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(ref))
	mock.ExpectQuery(`DELETE FROM "AssistantFile" WHERE "sessionId" = \$1 AND "userId" = \$2 RETURNING "spillRef"`).
		WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(fileRef))
	if err := s.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := spill.objs["s1/file1"]; ok {
		t.Error("the session's file content must be reclaimed on delete")
	}

	// A missing / non-owned id is not-found.
	mock.ExpectQuery(`DELETE FROM "AssistantSession"`).WithArgs("s2", "alice").WillReturnError(pgx.ErrNoRows)
	if err := s.Delete("s2"); err == nil {
		t.Fatal("deleting a missing / non-owned session must error")
	}

	// A non-not-found DB error propagates.
	mock.ExpectQuery(`DELETE FROM "AssistantSession"`).WithArgs("s3", "alice").WillReturnError(errors.New("boom"))
	if err := s.Delete("s3"); err == nil {
		t.Fatal("Delete must propagate a DB error")
	}

	// A null ref deletes the row without a transcript spill call; the file-reclaim
	// query still runs (here it returns no files).
	mock.ExpectQuery(`DELETE FROM "AssistantSession"`).WithArgs("s4", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow([]byte("null")))
	mock.ExpectQuery(`DELETE FROM "AssistantFile" WHERE "sessionId" = \$1 AND "userId" = \$2 RETURNING "spillRef"`).
		WithArgs("s4", "alice").WillReturnRows(pgxmock.NewRows([]string{"spillRef"}))
	if err := s.Delete("s4"); err != nil {
		t.Fatalf("Delete null-ref: %v", err)
	}
}

func TestDBMemoryRecallScopesByUser(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	m := newDBMemory(context.Background(), mock, "bob")
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 AND name = \$2`).
		WithArgs("bob", "region").
		WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body"}).AddRow("region", "entity", "us-east"))
	f, ok, err := m.Recall("region")
	if err != nil || !ok || f.Body != "us-east" {
		t.Fatalf("Recall: %+v ok=%v err=%v", f, ok, err)
	}

	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory"`).
		WithArgs("bob", "ghost").
		WillReturnError(pgx.ErrNoRows)
	if _, ok, _ := m.Recall("ghost"); ok {
		t.Fatal("a missing fact must report ok=false")
	}
}

func TestDBMemoryWriteOps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	m := newDBMemory(context.Background(), mock, "bob")

	mock.ExpectExec(`INSERT INTO "AssistantMemory"`).
		WithArgs("bob", "region", "entity", "us-east").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := m.Remember(agent.MemoryFact{Name: "region", Type: "entity", Body: "us-east"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	mock.ExpectExec(`UPDATE "AssistantMemory" SET body`).
		WithArgs("bob", "region", "us-west").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := m.Update("region", "us-west"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Update of a missing fact (0 rows) must error.
	mock.ExpectExec(`UPDATE "AssistantMemory" SET body`).
		WithArgs("bob", "ghost", "x").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	if err := m.Update("ghost", "x"); err == nil {
		t.Fatal("updating a missing fact must error")
	}

	mock.ExpectExec(`DELETE FROM "AssistantMemory"`).
		WithArgs("bob", "region").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if removed, err := m.Forget("region"); err != nil || !removed {
		t.Fatalf("Forget: removed=%v err=%v", removed, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDBMemoryIndexAndErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	m := newDBMemory(context.Background(), mock, "bob")
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 ORDER BY name`).
		WithArgs("bob").
		WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body"}).
			AddRow("region", "entity", "us-east").
			AddRow("terse", "preference", "short replies"))
	idx, err := m.Index()
	if err != nil || !strings.Contains(idx, "region") {
		t.Fatalf("Index: %q %v", idx, err)
	}

	boom := errors.New("boom")
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 ORDER BY`).WithArgs("bob").WillReturnError(boom)
	if _, err := m.Index(); err == nil {
		t.Fatal("Index must propagate")
	}
	mock.ExpectExec(`INSERT INTO "AssistantMemory"`).WillReturnError(boom)
	if err := m.Remember(agent.MemoryFact{Name: "x", Type: "entity", Body: "y"}); err == nil {
		t.Fatal("Remember must propagate")
	}
	mock.ExpectExec(`DELETE FROM "AssistantMemory"`).WithArgs("bob", "x").WillReturnError(boom)
	if _, err := m.Forget("x"); err == nil {
		t.Fatal("Forget must propagate")
	}
}

func TestDBSessionTitle(t *testing.T) {
	if got := dbSessionTitle(&agent.Session{}); got != "" {
		t.Fatalf("empty session → empty title, got %q", got)
	}
	long := strings.Repeat("x", 80)
	got := dbSessionTitle(&agent.Session{Messages: []agent.Message{agent.TextMessage(agent.RoleUser, long)}})
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != 61 {
		t.Fatalf("a long title must truncate to 60 chars + …, got %d runes", len([]rune(got)))
	}
	// A blank first user message is skipped in favour of the next real one.
	got = dbSessionTitle(&agent.Session{Messages: []agent.Message{
		agent.TextMessage(agent.RoleUser, "   "),
		agent.TextMessage(agent.RoleUser, "real title"),
	}})
	if got != "real title" {
		t.Fatalf("blank first message must be skipped, got %q", got)
	}
}

func TestDBStoreMemoryScanAndDecodeErrors(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	boom := errors.New("boom")

	store := newDBStore(context.Background(), mock, &fakeSpill{}, "alice")

	// List: a non-time value in the updatedAt column fails Scan into time.Time.
	mock.ExpectQuery(`SELECT id, title`).WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"id", "title", "updatedAt"}).AddRow("s1", "hi", "not-a-time"))
	if _, err := store.List(); err == nil {
		t.Fatal("List must surface a row Scan error")
	}

	// Load: the DB ref column is malformed JSON.
	mock.ExpectQuery(`SELECT "spillRef"`).WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow([]byte("{bad")))
	if _, err := store.Load("s1"); err == nil {
		t.Fatal("Load must surface a malformed-ref decode error")
	}

	// Load: ref is valid but the spilled content is not valid transcript JSON.
	spill := &fakeSpill{objs: map[string][]byte{"s1:transcript": []byte("{not-array}")}}
	store2 := newDBStore(context.Background(), mock, spill, "alice")
	ref, _ := json.Marshal(audit.SpillRef{Backend: "fake", Key: "s1:transcript"})
	mock.ExpectQuery(`SELECT "spillRef"`).WithArgs("s1", "alice").
		WillReturnRows(pgxmock.NewRows([]string{"spillRef"}).AddRow(ref))
	if _, err := store2.Load("s1"); err == nil {
		t.Fatal("Load must surface a malformed-transcript decode error")
	}

	mem := newDBMemory(context.Background(), mock, "bob")

	// Index: a time value in the name column fails Scan into string.
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 ORDER BY name`).
		WithArgs("bob").
		WillReturnRows(pgxmock.NewRows([]string{"name", "type", "body"}).AddRow(time.Now(), "t", "b"))
	if _, err := mem.Index(); err == nil {
		t.Fatal("Index must surface a row Scan error")
	}

	// Recall: a non-not-found DB error.
	mock.ExpectQuery(`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = \$1 AND name`).
		WithArgs("bob", "region").WillReturnError(boom)
	if _, _, err := mem.Recall("region"); err == nil {
		t.Fatal("Recall must propagate a non-not-found error")
	}

	// Update: a DB Exec error (distinct from the 0-rows case).
	mock.ExpectExec(`UPDATE "AssistantMemory" SET body`).WithArgs("bob", "region", "v").WillReturnError(boom)
	if err := mem.Update("region", "v"); err == nil {
		t.Fatal("Update must propagate a DB Exec error")
	}
}
