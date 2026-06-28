package assistant

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hashchain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// chataudit.go — the chat trusted-audit chain. Every persisted transcript
// revision (a web turn save, a deletion tombstone) appends one hash-chained
// AssistantChatEvent row using the same
// recipe as workflow run events: canonical envelope → SHA-256 chain over the
// previous hash, with the envelope bytes stored verbatim so verification never
// depends on the spilled (sweepable) transcript content. The chain covers
// these append-only rows — never the mutable, LWW-synced AssistantSession row,
// whose only chain role is the cloud-maintained head anchor (chainSeq /
// chainHead) that makes a truncated chain tail detectable.

// Chat integrity statuses, surfaced by the session read path. Named states,
// never silent: a session whose audit chain fails verification is served WITH
// the break named (and durably stamped), so the conversation surface keeps
// working while the audit consumer sees exactly what failed.
const (
	chatIntegrityVerified        = "verified"
	chatIntegrityUnchained       = "unchained"
	chatIntegrityChainBroken     = "chain_broken"
	chatIntegrityContentMismatch = "content_mismatch"
)

// chatChainKinds: a 'revision' pins persisted transcript content; a
// 'tombstone' records a deletion (content digest empty by definition).
const (
	chatKindRevision  = "revision"
	chatKindTombstone = "tombstone"
)

// chatOriginWeb marks revisions persisted by the web surface (a web turn, a
// web session delete) — the only writer today. The envelope keeps the origin
// field so a future second writer extends the chain without a recipe change.
const chatOriginWeb = "web"

// chatChainEnvelope is the canonical hash-input shape for one chat chain
// entry. ContentDigest pins the SHA-256 of the exact transcript bytes written
// to spill storage (empty on a tombstone).
type chatChainEnvelope struct {
	Seq           int    `json:"seq"`
	Kind          string `json:"kind"`
	Origin        string `json:"origin"`
	MsgCount      int    `json:"msgCount"`
	ContentDigest string `json:"contentDigest"`
}

// chatIntegrity is the verification verdict the session read path surfaces.
type chatIntegrity struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// chatChainAppendAttempts bounds the optimistic head-CAS retry: two writers of
// one session are already serialized by the bus (web) or per-key LWW (sync),
// so a (sessionId, seq) collision is a rare race that converges immediately.
const chatChainAppendAttempts = 3

// appendChatEvent appends one chain entry for the session: read the current
// head, fold the canonical envelope into the chain, insert. The unique
// (sessionId, seq) index is the CAS — a concurrent appender's collision is
// retried with a fresh head. Returns the appended seq + hash so the caller can
// stamp the session row's head anchor in the same flow.
func appendChatEvent(ctx context.Context, pool pgxPool, userID, sessionID, kind string, msgCount int, contentDigest string) (int, string, error) {
	origin := chatOriginWeb
	for range chatChainAppendAttempts {
		var lastSeq int
		var lastHash *string
		err := pool.QueryRow(ctx, `
			SELECT "seq","hash" FROM "AssistantChatEvent"
			WHERE "sessionId"=$1 AND "userId"=$2 ORDER BY "seq" DESC LIMIT 1`,
			sessionID, userID).Scan(&lastSeq, &lastHash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, "", err
		}
		seq := lastSeq + 1
		envBytes, err := json.Marshal(chatChainEnvelope{
			Seq: seq, Kind: kind, Origin: origin,
			MsgCount: msgCount, ContentDigest: contentDigest,
		})
		if err != nil {
			return 0, "", err
		}
		hashInput, err := hashchain.Canonicalize(envBytes)
		if err != nil {
			return 0, "", err
		}
		hash := hashchain.ChainHash(lastHash, hashInput)
		_, err = pool.Exec(ctx, `
			INSERT INTO "AssistantChatEvent"
				("sessionId","userId","seq","kind","origin","msgCount","contentDigest","prevHash","hash","hashInput")
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			sessionID, userID, seq, kind, origin, msgCount, contentDigest, lastHash, hash, hashInput)
		if err == nil {
			return seq, hash, nil
		}
		if !isUniqueViolation(err) {
			return 0, "", err
		}
		// Lost the head race — re-read and retry.
	}
	return 0, "", fmt.Errorf("chat chain append: exhausted retries (seq contention)")
}

// isUniqueViolation reports a Postgres unique-constraint failure (SQLSTATE
// 23505) — the chain append's CAS-lost signal.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// chatChainRow is one AssistantChatEvent read back for verification.
type chatChainRow struct {
	Seq           int
	Kind          string
	ContentDigest string
	PrevHash      *string
	Hash          string
	HashInput     []byte
}

// verifySessionChain verifies the session's trusted-audit chain and the live
// transcript against it, returning a named verdict:
//
//   - unchained: the session predates the chain (no entries, zero anchor) —
//     it chains from its next write (no-backfill genesis).
//   - chain_broken: the audit rows fail hash-chain verification, or the
//     session row's head anchor points PAST the stored head (a truncated
//     tail), or the anchor hash disagrees with the head row.
//   - content_mismatch: the chain is intact but the transcript the session
//     currently serves is not the content the chain head attests (an
//     out-of-band edit, a rollback to an older blob, or a half-completed save).
//   - verified: chain intact, anchors agree, and the served transcript's
//     digest equals the head's pinned digest. A transcript whose spill content
//     expired under retention stays verified (the chain pins the digest, not
//     the bytes) with the expiry named in detail — the workflow-export
//     "unavailable(swept)" semantics.
func (s *dbStore) verifySessionChain(id string) chatIntegrity {
	var refJSON []byte
	var chainSeq int
	var chainHead string
	err := s.pool.QueryRow(s.ctx, `
		SELECT "spillRef","chainSeq","chainHead" FROM "AssistantSession"
		WHERE id = $1 AND "userId" = $2`, id, s.userID).Scan(&refJSON, &chainSeq, &chainHead)
	if err != nil {
		return chatIntegrity{Status: chatIntegrityChainBroken, Detail: "session anchor could not be read"}
	}

	rows, err := s.pool.Query(s.ctx, `
		SELECT "seq","kind","contentDigest","prevHash","hash","hashInput"
		FROM "AssistantChatEvent"
		WHERE "sessionId" = $1 AND "userId" = $2 ORDER BY "seq"`, id, s.userID)
	if err != nil {
		return chatIntegrity{Status: chatIntegrityChainBroken, Detail: "audit chain could not be read"}
	}
	defer rows.Close()
	var entries []chatChainRow
	for rows.Next() {
		var r chatChainRow
		if err := rows.Scan(&r.Seq, &r.Kind, &r.ContentDigest, &r.PrevHash, &r.Hash, &r.HashInput); err != nil {
			return chatIntegrity{Status: chatIntegrityChainBroken, Detail: "audit chain could not be read"}
		}
		entries = append(entries, r)
	}
	if rows.Err() != nil {
		return chatIntegrity{Status: chatIntegrityChainBroken, Detail: "audit chain could not be read"}
	}

	if len(entries) == 0 {
		if chainSeq == 0 {
			return chatIntegrity{Status: chatIntegrityUnchained, Detail: "this session predates the audit chain; it chains from its next message"}
		}
		return chatIntegrity{Status: chatIntegrityChainBroken, Detail: fmt.Sprintf("the session anchor records %d chained revisions but none exist", chainSeq)}
	}

	links := make([]hashchain.Link, len(entries))
	for i, e := range entries {
		links[i] = hashchain.Link{Seq: e.Seq, PrevHash: e.PrevHash, Hash: e.Hash, HashInput: e.HashInput}
	}
	if err := hashchain.VerifyLinks(links); err != nil {
		return chatIntegrity{Status: chatIntegrityChainBroken, Detail: err.Error()}
	}
	head := entries[len(entries)-1]
	if chainSeq > head.Seq {
		return chatIntegrity{Status: chatIntegrityChainBroken,
			Detail: fmt.Sprintf("the session anchor records revision %d but the chain ends at %d (tail truncated)", chainSeq, head.Seq)}
	}
	// The anchor must pin its OWN entry whether or not the chain has grown past
	// it (append and row-stamp are separate statements, so head.Seq can run
	// ahead of the anchor legitimately). Checking only the equal-seq case would
	// let a full self-consistent chain rewrite hide behind ONE appended junk
	// entry — the lag state must not disable the rewrite check. VerifyLinks has
	// proven seqs are gapless from 1, so the anchored entry is at index
	// chainSeq-1.
	if chainSeq >= 1 {
		anchored := entries[chainSeq-1]
		if chainHead != anchored.Hash {
			return chatIntegrity{Status: chatIntegrityChainBroken,
				Detail: fmt.Sprintf("the session anchor disagrees with the chain at revision %d", chainSeq)}
		}
	}
	// The revision the session row actually serves is the ANCHORED one (the
	// anchor stamps in the same save as the row content); when the chain has
	// run ahead of the anchor (a later append whose row-stamp did not land),
	// checking content against the HEAD would convict an infra blip as
	// tampering. Verify against the entry the row acknowledges, naming the lag.
	attest := head
	lag := ""
	if chainSeq >= 1 && chainSeq < head.Seq {
		attest = entries[chainSeq-1]
		lag = fmt.Sprintf("; %d chained revision(s) past the anchor await the session row's acknowledgment", head.Seq-chainSeq)
	}
	// The kind and digest used for the content check come from the HASH-COVERED
	// envelope, never the convenience columns — a column edited alongside a
	// tampered transcript would otherwise pass. A column that drifts from its
	// own envelope is itself evidence.
	var attEnv chatChainEnvelope
	if err := json.Unmarshal(attest.HashInput, &attEnv); err != nil {
		return chatIntegrity{Status: chatIntegrityChainBroken,
			Detail: fmt.Sprintf("the chain entry's envelope is undecodable (revision %d)", attest.Seq)}
	}
	if attEnv.Kind != attest.Kind || attEnv.ContentDigest != attest.ContentDigest {
		return chatIntegrity{Status: chatIntegrityChainBroken,
			Detail: fmt.Sprintf("the chain entry's columns disagree with its hash-covered envelope (revision %d)", attest.Seq)}
	}
	if attEnv.Kind == chatKindTombstone {
		return chatIntegrity{Status: chatIntegrityContentMismatch,
			Detail: "the chain records a deletion but the session still serves content"}
	}

	// Content check: the digest of the transcript bytes the session currently
	// serves must equal the digest its acknowledged chain entry pinned.
	digest, state := s.transcriptDigest(id, refJSON)
	switch state {
	case transcriptSwept:
		return chatIntegrity{Status: chatIntegrityVerified, Detail: "chain verified; transcript content expired under retention" + lag}
	case transcriptUnreadable:
		return chatIntegrity{Status: chatIntegrityContentMismatch, Detail: "the stored transcript could not be read for verification"}
	}
	if digest != attEnv.ContentDigest {
		return chatIntegrity{Status: chatIntegrityContentMismatch,
			Detail: fmt.Sprintf("the stored transcript does not match its chained attestation (revision %d)", attest.Seq)}
	}
	if lag != "" {
		return chatIntegrity{Status: chatIntegrityVerified, Detail: "chain verified" + lag}
	}
	return chatIntegrity{Status: chatIntegrityVerified}
}

// transcript content states for the verify path.
const (
	transcriptPresent = iota
	transcriptSwept
	transcriptUnreadable
)

// transcriptDigest returns the SHA-256 of the session's spilled transcript.
// When the same store instance just loaded those exact bytes (the GET-session
// path: Load then verify on one request), the memoized digest is reused so the
// open path pays a single spill read. An absent blob (retention sweep) is the
// named swept state, not an error; an unreadable backend is unreadable (never
// silently verified).
func (s *dbStore) transcriptDigest(id string, refJSON []byte) (string, int) {
	if len(refJSON) == 0 || string(refJSON) == "null" || s.spill == nil {
		return "", transcriptSwept
	}
	if d, ok := s.loadedDigest[id]; ok {
		return d, transcriptPresent
	}
	var ref audit.SpillRef
	if err := json.Unmarshal(refJSON, &ref); err != nil {
		return "", transcriptUnreadable
	}
	rc, err := s.spill.Get(s.ctx, ref)
	if errors.Is(err, spillstore.ErrNotFound) {
		return "", transcriptSwept
	}
	if err != nil {
		return "", transcriptUnreadable
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", transcriptUnreadable
	}
	return audit.SHA256Hex(data), transcriptPresent
}

// markChatChainBroken durably stamps the session's tamper flag (the workflow
// MarkChainBroken mirror) so a verification failure stays visible even if the
// chain is later "repaired". Best-effort: the verdict is already surfaced.
func markChatChainBroken(ctx context.Context, pool pgxPool, userID, sessionID string) {
	_, _ = pool.Exec(ctx, `
		UPDATE "AssistantSession" SET "chainBrokenAt" = COALESCE("chainBrokenAt", now())
		WHERE id = $1 AND "userId" = $2`, sessionID, userID)
}
