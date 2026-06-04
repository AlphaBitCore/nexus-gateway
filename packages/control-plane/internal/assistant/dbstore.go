package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// pgxPool is the minimal pgx surface the DB-backed assistant stores need. *pgxpool.Pool
// satisfies it; tests inject pgxmock so the store logic + isolation are exercised
// without a live database.
type pgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// dbStore is the web SessionStore. The DB row is the per-user metadata INDEX
// (title / model / counts) plus a SpillRef pointer; the transcript CONTENT lives in
// the shared spillstore (localfs locally, S3 in prod — the same `spill:` backend the
// other services use), keyed by session id. Constructed PER CALLER/turn bound to the
// authenticated userId (E90 invariant I3): every query carries WHERE "userId", so a
// user can never read another's sessions — and since the SpillRef is only reachable
// through their own row, transcript content is isolated transitively.
type dbStore struct {
	pool   pgxPool
	spill  spillstore.SpillStore
	userID string
	ctx    context.Context
}

func newDBStore(ctx context.Context, pool pgxPool, spill spillstore.SpillStore, userID string) *dbStore {
	return &dbStore{pool: pool, spill: spill, userID: userID, ctx: ctx}
}

func (s *dbStore) Save(sess *agent.Session) error {
	data, err := json.Marshal(sess.Messages)
	if err != nil {
		return fmt.Errorf("marshal transcript: %w", err)
	}
	ref, err := s.spill.Put(s.ctx, bytes.NewReader(data), int64(len(data)), spillstore.PutOptions{
		EventID:     sess.ID,
		Direction:   "transcript",
		ContentType: "application/json",
	})
	if err != nil {
		return fmt.Errorf("spill transcript: %w", err)
	}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("marshal spill ref: %w", err)
	}
	_, err = s.pool.Exec(s.ctx, `
		INSERT INTO "AssistantSession" (id, "userId", title, model, "msgCount", "lastSeq", "spillRef", "updatedAt")
		VALUES ($1, $2, $3, '', $4, $5, $6, now())
		ON CONFLICT (id) DO UPDATE
		   SET title = EXCLUDED.title, "msgCount" = EXCLUDED."msgCount",
		       "lastSeq" = EXCLUDED."lastSeq", "spillRef" = EXCLUDED."spillRef", "updatedAt" = now()
		 WHERE "AssistantSession"."userId" = $2`,
		sess.ID, s.userID, dbSessionTitle(sess), len(sess.Messages), len(sess.Messages), refJSON)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

func (s *dbStore) Load(id string) (*agent.Session, error) {
	var refJSON []byte
	err := s.pool.QueryRow(s.ctx,
		`SELECT "spillRef" FROM "AssistantSession" WHERE id = $1 AND "userId" = $2`,
		id, s.userID).Scan(&refJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("session %s not found", id) // cross-user reads are indistinguishable from not-found
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	var msgs []agent.Message
	if len(refJSON) > 0 && string(refJSON) != "null" {
		var ref audit.SpillRef
		if err := json.Unmarshal(refJSON, &ref); err != nil {
			return nil, fmt.Errorf("decode spill ref: %w", err)
		}
		rc, err := s.spill.Get(s.ctx, ref)
		if errors.Is(err, spillstore.ErrNotFound) {
			// Content expired under the shared spill retention while the metadata row
			// survives — degrade to an empty transcript rather than a hard failure.
			return &agent.Session{ID: id, Env: "web"}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fetch transcript: %w", err)
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("read transcript: %w", err)
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &msgs); err != nil {
				return nil, fmt.Errorf("decode transcript: %w", err)
			}
		}
	}
	return &agent.Session{ID: id, Env: "web", Messages: msgs}, nil
}

func (s *dbStore) List() ([]agent.SessionMeta, error) {
	rows, err := s.pool.Query(s.ctx,
		`SELECT id, title, "updatedAt" FROM "AssistantSession" WHERE "userId" = $1 ORDER BY "updatedAt" DESC`,
		s.userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []agent.SessionMeta
	for rows.Next() {
		var m agent.SessionMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.Updated); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		m.Env = "web"
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete removes one of the caller's sessions (row scoped to the owner) and its
// spilled transcript. A missing / non-owned id is reported as not-found, so a
// cross-user delete cannot even confirm the session exists.
func (s *dbStore) Delete(id string) error {
	var refJSON []byte
	err := s.pool.QueryRow(s.ctx,
		`DELETE FROM "AssistantSession" WHERE id = $1 AND "userId" = $2 RETURNING "spillRef"`,
		id, s.userID).Scan(&refJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("session %s not found", id)
	}
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	s.deleteSpillRef(refJSON)
	// Reclaim the session's sandbox files too — rows AND spill content. This is what
	// makes the per-user storage quota recoverable: the quota SUMs AssistantFile.size,
	// so without deleting these rows a session delete would NOT free quota and a user who
	// hit the cap could never write again. Best-effort (the session row is already gone);
	// a stale file row only over-counts the courtesy quota until the next spill sweep.
	s.deleteSessionFiles(id)
	return nil
}

// deleteSpillRef best-effort removes one spilled blob. The row is already gone, so an
// orphaned blob is harmless and reclaimed by spillstore Sweep; ErrNotFound is non-fatal.
func (s *dbStore) deleteSpillRef(refJSON []byte) {
	if s.spill == nil || len(refJSON) == 0 || string(refJSON) == "null" {
		return
	}
	var ref audit.SpillRef
	if json.Unmarshal(refJSON, &ref) == nil {
		_ = s.spill.Delete(s.ctx, ref)
	}
}

// deleteSessionFiles removes every sandbox file belonging to (sessionId, userId) — the
// DB rows (so the quota SUM drops) and their spill content. Scoped by userId (I3). All
// best-effort: a failure here never fails the session delete; leftover rows only inflate
// the soft quota until the spill sweep reclaims them.
func (s *dbStore) deleteSessionFiles(sessionID string) {
	rows, err := s.pool.Query(s.ctx,
		`DELETE FROM "AssistantFile" WHERE "sessionId" = $1 AND "userId" = $2 RETURNING "spillRef"`,
		sessionID, s.userID)
	if err != nil {
		return
	}
	var refs [][]byte
	for rows.Next() {
		var rj []byte
		if rows.Scan(&rj) == nil {
			refs = append(refs, rj)
		}
	}
	rows.Close()
	for _, rj := range refs {
		s.deleteSpillRef(rj)
	}
}

var _ agent.SessionStore = (*dbStore)(nil)

func dbSessionTitle(sess *agent.Session) string {
	for _, m := range sess.Messages {
		if m.Role == agent.RoleUser {
			t := strings.TrimSpace(m.Text())
			if t == "" {
				continue
			}
			if len(t) > 60 {
				return t[:60] + "…"
			}
			return t
		}
	}
	return ""
}

// dbMemory is the web MemoryStore: durable per-user facts in Postgres, isolated by
// userId. Constructed per caller bound to the authenticated userId; every query
// carries WHERE "userId".
type dbMemory struct {
	pool   pgxPool
	userID string
	ctx    context.Context
}

func newDBMemory(ctx context.Context, pool pgxPool, userID string) *dbMemory {
	return &dbMemory{pool: pool, userID: userID, ctx: ctx}
}

func (m *dbMemory) Index() (string, error) {
	rows, err := m.pool.Query(m.ctx,
		`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = $1 ORDER BY name`, m.userID)
	if err != nil {
		return "", fmt.Errorf("memory index: %w", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var name, typ, body string
		if err := rows.Scan(&name, &typ, &body); err != nil {
			return "", fmt.Errorf("scan memory: %w", err)
		}
		fmt.Fprintf(&b, "- %s [%s] — %s\n", name, typ, oneLineDB(body))
	}
	return strings.TrimRight(b.String(), "\n"), rows.Err()
}

func (m *dbMemory) Recall(name string) (agent.MemoryFact, bool, error) {
	var f agent.MemoryFact
	err := m.pool.QueryRow(m.ctx,
		`SELECT name, type, body FROM "AssistantMemory" WHERE "userId" = $1 AND name = $2`,
		m.userID, name).Scan(&f.Name, &f.Type, &f.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.MemoryFact{}, false, nil
	}
	if err != nil {
		return agent.MemoryFact{}, false, fmt.Errorf("recall: %w", err)
	}
	return f, true, nil
}

func (m *dbMemory) Remember(f agent.MemoryFact) error {
	_, err := m.pool.Exec(m.ctx, `
		INSERT INTO "AssistantMemory" ("userId", name, type, body, "updatedAt")
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT ("userId", name) DO UPDATE SET type = EXCLUDED.type, body = EXCLUDED.body, "updatedAt" = now()`,
		m.userID, f.Name, f.Type, f.Body)
	if err != nil {
		return fmt.Errorf("remember: %w", err)
	}
	return nil
}

func (m *dbMemory) Update(name, body string) error {
	tag, err := m.pool.Exec(m.ctx,
		`UPDATE "AssistantMemory" SET body = $3, "updatedAt" = now() WHERE "userId" = $1 AND name = $2`,
		m.userID, name, body)
	if err != nil {
		return fmt.Errorf("update memory: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no memory named %q to update", name)
	}
	return nil
}

func (m *dbMemory) Forget(name string) (bool, error) {
	tag, err := m.pool.Exec(m.ctx,
		`DELETE FROM "AssistantMemory" WHERE "userId" = $1 AND name = $2`, m.userID, name)
	if err != nil {
		return false, fmt.Errorf("forget: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

var _ agent.MemoryStore = (*dbMemory)(nil)

func oneLineDB(s string) string { return strings.Join(strings.Fields(s), " ") }
