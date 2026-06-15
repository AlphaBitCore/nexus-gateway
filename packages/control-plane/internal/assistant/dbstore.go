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
	"time"
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
// (title / counts) plus a SpillRef pointer; the transcript CONTENT lives in
// the shared spillstore (localfs locally, S3 in prod — the same `spill:` backend the
// other services use), keyed by session id. Constructed PER CALLER/turn bound to the
// authenticated userId: every query carries WHERE "userId", so a
// user can never read another's sessions — and since the SpillRef is only reachable
// through their own row, transcript content is isolated transitively.
type dbStore struct {
	pool   pgxPool
	spill  spillstore.SpillStore
	userID string
	ctx    context.Context
	// loadedDigest memoizes, per session id, the SHA-256 of the exact spill
	// bytes the last Load read — so the chain verification that follows a load
	// on the same request reuses it instead of re-fetching and re-hashing the
	// whole transcript (the session-open hot path pays ONE spill read).
	loadedDigest map[string]string
}

func newDBStore(ctx context.Context, pool pgxPool, spill spillstore.SpillStore, userID string) *dbStore {
	return &dbStore{pool: pool, spill: spill, userID: userID, ctx: ctx}
}

// errSessionNotFound distinguishes "absent or not yours" from an
// infrastructure failure, so the delete handler can 404 the former without
// masking the latter as not-found.
var errSessionNotFound = errors.New("session not found")

func (s *dbStore) Save(sess *agent.Session) error {
	now := time.Now().UTC()
	if sess.Created.IsZero() {
		sess.Created = now
	}
	sess.Updated = now
	// The spill holds the FULL session (env + timestamps + messages), not just
	// the transcript, so a reload restores identity without consulting the row.
	data, err := json.Marshal(sess)
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
	// Trusted audit: chain this revision BEFORE the row upsert. If the append
	// fails the row keeps pointing at the previous (attested) transcript and
	// the save fails loudly; the just-spilled blob is an orphan the retention
	// sweep reclaims. The digest is computed over the exact bytes spilled.
	seq, head, err := appendChatEvent(s.ctx, s.pool, s.userID, sess.ID,
		chatKindRevision, len(sess.Messages), audit.SHA256Hex(data))
	if err != nil {
		return fmt.Errorf("chain transcript revision: %w", err)
	}
	tag, err := s.pool.Exec(s.ctx, `
		INSERT INTO "AssistantSession" (id, "userId", title, "msgCount", "lastSeq", "spillRef", "chainSeq", "chainHead", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (id) DO UPDATE
		   SET title = EXCLUDED.title, "msgCount" = EXCLUDED."msgCount",
		       "lastSeq" = EXCLUDED."lastSeq", "spillRef" = EXCLUDED."spillRef",
		       "chainSeq" = EXCLUDED."chainSeq", "chainHead" = EXCLUDED."chainHead", "updatedAt" = now()
		 WHERE "AssistantSession"."userId" = $2`,
		sess.ID, s.userID, dbSessionTitle(sess), len(sess.Messages), len(sess.Messages), refJSON, seq, head)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	// The conflict-update's userId guard makes a cross-user id collision a
	// silent no-op at the SQL layer; surface it as a named error instead — the
	// caller's transcript was NOT saved and pretending otherwise would lose it.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("session %s: %w (id owned by another user)", sess.ID, errSessionNotFound)
	}
	return nil
}

func (s *dbStore) Load(id string) (*agent.Session, error) {
	var refJSON []byte
	var createdAt, updatedAt time.Time
	err := s.pool.QueryRow(s.ctx,
		`SELECT "spillRef", "createdAt", "updatedAt" FROM "AssistantSession" WHERE id = $1 AND "userId" = $2`,
		id, s.userID).Scan(&refJSON, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("session %s not found", id) // cross-user reads are indistinguishable from not-found
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	// The row is the fallback identity when the spill content expired:
	// timestamps from the row, env always web.
	sess := &agent.Session{ID: id, Env: "web", Created: createdAt, Updated: updatedAt}
	if len(refJSON) > 0 && string(refJSON) != "null" {
		var ref audit.SpillRef
		if err := json.Unmarshal(refJSON, &ref); err != nil {
			return nil, fmt.Errorf("decode spill ref: %w", err)
		}
		rc, err := s.spill.Get(s.ctx, ref)
		if errors.Is(err, spillstore.ErrNotFound) {
			// Content expired under the shared spill retention while the metadata row
			// survives — degrade to an empty transcript rather than a hard failure.
			return sess, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fetch transcript: %w", err)
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("read transcript: %w", err)
		}
		if s.loadedDigest == nil {
			s.loadedDigest = map[string]string{}
		}
		s.loadedDigest[id] = audit.SHA256Hex(data)
		if err := decodeSpilledSession(data, sess); err != nil {
			return nil, fmt.Errorf("decode transcript: %w", err)
		}
	}
	return sess, nil
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
// spilled transcript. A missing / non-owned id is reported as errSessionNotFound,
// so a cross-user delete cannot even confirm the session exists. The deletion is
// recorded as a tombstone entry on the session's trusted-audit chain — the chain
// rows deliberately survive the row (deletion is itself audited evidence); only
// digests and counts remain, never transcript content.
func (s *dbStore) Delete(id string) error {
	var refJSON []byte
	err := s.pool.QueryRow(s.ctx,
		`DELETE FROM "AssistantSession" WHERE id = $1 AND "userId" = $2 RETURNING "spillRef"`,
		id, s.userID).Scan(&refJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("session %s: %w", id, errSessionNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if _, _, cerr := appendChatEvent(s.ctx, s.pool, s.userID, id, chatKindTombstone, 0, ""); cerr != nil {
		// The row is already gone — surface the audit gap rather than swallow
		// it; the caller decides how loudly (the session itself was deleted).
		return fmt.Errorf("session deleted but the audit tombstone failed: %w", cerr)
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

var _ agent.SessionStore = (*dbStore)(nil)

func dbSessionTitle(sess *agent.Session) string {
	for _, m := range sess.Messages {
		if m.Role == agent.RoleUser {
			t := strings.TrimSpace(m.Text())
			if t == "" {
				continue
			}
			if len(t) > 60 {
				return agent.CutText(t, 60) + "…"
			}
			return t
		}
	}
	return ""
}

func oneLineDB(s string) string { return strings.Join(strings.Fields(s), " ") }
