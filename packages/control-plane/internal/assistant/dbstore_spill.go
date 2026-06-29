// dbStore spill handling: oversized session payloads live in the spill store
// with only a reference in the row — loads re-inflate them; deletes release
// the spilled object and the session-scoped files.
package assistant

import (
	"bytes"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// decodeSpilledSession fills sess from the spilled bytes, accepting both spill
// formats: the full-session object (current) and the bare message array
// (pre-format rows). The full object wins for env/timestamps when populated —
// it is the device-authored truth the sync round-trip must preserve; the row
// values already in sess remain as fallback.
func decodeSpilledSession(data []byte, sess *agent.Session) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '[' {
		return json.Unmarshal(trimmed, &sess.Messages)
	}
	var full agent.Session
	if err := json.Unmarshal(trimmed, &full); err != nil {
		return err
	}
	sess.Messages = full.Messages
	sess.NavTrail = full.NavTrail
	if full.Env != "" {
		sess.Env = full.Env
	}
	if !full.Created.IsZero() {
		sess.Created = full.Created
	}
	if !full.Updated.IsZero() {
		sess.Updated = full.Updated
	}
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
// DB rows (so the quota SUM drops) and their spill content. Scoped by userId. All
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
