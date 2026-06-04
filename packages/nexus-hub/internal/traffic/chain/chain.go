// Package chain provides the tamper-evident hash chain helper used by every
// AdminAuditLog writer in the platform. There are two writer paths today:
//
//  1. consumer/admin_audit.go — handles MQ-published audit events emitted by
//     the Control Plane (admin API mutations).
//  2. fleet/manager/override.go — direct in-tx writer for per-Thing
//     override mutations performed Hub-side.
//
// Both call NextHash inside their tx before INSERT to:
//   - acquire the advisory lock (transaction-scoped),
//   - read the prior chain head's integrityHash,
//   - compute the new previousHash + integrityHash from the canonical
//     payload.
//
// Centralising the chain in one helper avoids the dual-writer drift that
// motivated F3: the previous design had the CP compute hashes locally and
// the Hub direct writer leave them NULL, producing a chain that broke at
// every override mutation.
//
// # Reserved Postgres advisory lock keys
//
// Do not reuse the following advisory-lock key without coordination — a
// collision would serialize unrelated writes against each other or, worse,
// admit interleaved chain inserts:
//
//	0x4E455841554348 — audit chain head (this package; chainAdvisoryLockKey)
package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// chainAdvisoryLockKey is the constant Postgres advisory-lock key that
// serializes audit-chain inserts. The exact value does not matter as long
// as it is stable across replicas — pg_advisory_xact_lock takes any int64
// and only callers using the same key contend with each other. We pick a
// human-readable byte sequence ("NEXAUCH" packed into hex) so the value
// shows up recognisably in pg_locks. See package doc for the registry of
// reserved keys.
const chainAdvisoryLockKey int64 = 0x4E4558_4155_4348 // "NEXAUCH"

// Construction errors for NewHashPayload. Both indicate caller bugs (the
// chain is meaningless without an actor + action), so callers should
// surface them as 500 / panic-in-test rather than 4xx user errors.
var (
	// ErrEmptyAction — payload.Action is empty. Every chain row is keyed by
	// the action verb; an empty Action would collide across mutation types
	// and make VerifyChain ambiguous.
	ErrEmptyAction = errors.New("audit hash payload: action cannot be empty")
	// ErrEmptyActorID — payload.ActorID is empty. Audit policy requires
	// every mutation to attribute to an actor (system jobs use a fixed
	// "system:<job>" id, never empty).
	ErrEmptyActorID = errors.New("audit hash payload: actorId cannot be empty")
)

// NewHashPayload constructs a HashPayload with the four required fields
// validated up-front. EntityType / EntityID may be empty — the
// AdminAuditLog schema permits NULL entityId for action types that don't
// target an entity (e.g. login events). Callers fill in BeforeState /
// AfterState / optional request-id fields on the returned value before
// passing to NextHash.
//
// Centralising the required-field check here avoids two writer paths
// silently producing chain rows with missing fields that VerifyChain
// would later flag as tampered.
func NewHashPayload(action, actorID, entityType, entityID string) (HashPayload, error) {
	if action == "" {
		return HashPayload{}, ErrEmptyAction
	}
	if actorID == "" {
		return HashPayload{}, ErrEmptyActorID
	}
	return HashPayload{
		Action:     action,
		ActorID:    actorID,
		EntityType: entityType,
		EntityID:   entityID,
	}, nil
}

// HashPayload is the deterministic input set hashed for each chain link.
// Field order matches the struct field declaration order, which Go's
// encoding/json preserves — so the canonical JSON encoding is byte-stable
// without any extra sorting layer.
//
// TimestampMs is milliseconds since the Unix epoch (UTC). We hash the int64
// rather than a formatted string because Postgres timestamptz round-trips
// can drift by sub-millisecond fractions depending on driver options;
// hashing the integer (and storing it intact via to_timestamp on read)
// avoids that whole class of bugs in VerifyChain.
//
// The (omitempty)-marked fields drop out of the JSON when empty so the
// canonical hash for a row without, say, a clientSessionId is the same
// whether that field is absent or empty in the payload struct.
type HashPayload struct {
	TimestampMs     int64           `json:"timestampMs"`
	Action          string          `json:"action"`
	ActorID         string          `json:"actorId"`
	EntityType      string          `json:"entityType"`
	EntityID        string          `json:"entityId,omitempty"`
	BeforeState     json.RawMessage `json:"beforeState,omitempty"`
	AfterState      json.RawMessage `json:"afterState,omitempty"`
	NexusRequestID  string          `json:"nexusRequestId,omitempty"`
	ClientRequestID string          `json:"clientRequestId,omitempty"`
	ClientUserID    string          `json:"clientUserId,omitempty"`
	ClientSessionID string          `json:"clientSessionId,omitempty"`
	// Via records the channel that initiated the mutation — "assistant" for an
	// AI-initiated admin write performed by the web assistant, empty for a direct
	// human/UI action. It is hashed (omitempty) so the AI-attribution marker is
	// tamper-evident: a row written with via="assistant" cannot have the marker
	// stripped without breaking the chain. Because of omitempty + the sorted-key
	// canonical encoding, every row with an empty Via (all existing rows and all
	// human/system writes) hashes byte-identically to the pre-Via recipe, so adding
	// this field does NOT re-anchor the chain or require a backfill.
	Via string `json:"via,omitempty"`
}

// NextHash acquires the advisory lock for the duration of the surrounding
// transaction, reads the current chain head's integrityHash (or none for
// the genesis row), and computes the new previousHash + integrityHash plus
// the exact canonical bytes that were SHA-256'd.
//
// MUST be called inside a transaction; the advisory lock is released on
// commit/rollback automatically, so callers cannot leak it. The caller is
// responsible for performing the INSERT itself — this helper only computes.
//
// The returned previousHash is the empty string for the genesis row (DB
// column previousHash should then be stored as NULL); for every subsequent
// row it is the prior row's integrityHash. integrityHash is always a 64-char
// lowercase hex SHA-256 digest.
//
// hashInput is the cryptographic source of truth and MUST be persisted in
// AdminAuditLog.hashInput. VerifyChain reads that column verbatim so it can
// recompute the chain without reconstructing the payload from JSONB columns
// — JSONB normalises key order and whitespace on storage/readback, which
// would otherwise break byte equality on every row whose beforeState or
// afterState is non-null.
func NextHash(ctx context.Context, tx pgx.Tx, p HashPayload) (previousHash, integrityHash string, hashInput []byte, err error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, chainAdvisoryLockKey); err != nil {
		return "", "", nil, fmt.Errorf("acquire chain advisory lock: %w", err)
	}

	var prev *string
	row := tx.QueryRow(ctx,
		`SELECT "integrityHash" FROM "AdminAuditLog" ORDER BY "sequenceNumber" DESC LIMIT 1`,
	)
	if err := row.Scan(&prev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, fmt.Errorf("read chain head: %w", err)
	}

	canonical, err := canonicalizePayload(p)
	if err != nil {
		return "", "", nil, fmt.Errorf("marshal payload: %w", err)
	}

	h := sha256.New()
	if prev != nil {
		h.Write([]byte(*prev))
	}
	h.Write(canonical)
	integrityHash = hex.EncodeToString(h.Sum(nil))

	if prev != nil {
		previousHash = *prev
	}
	return previousHash, integrityHash, canonical, nil
}

// Queryer is the minimal interface VerifyChain needs from a pgx pool / tx /
// connection. Declared here so callers can pass either.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// VerifyChain walks the chain in sequenceNumber order and checks that each
// row's stored previousHash links to the running chain head and that
// SHA-256(previousHash || hashInput) equals the stored integrityHash.
//
// Returns the sequenceNumber of the first tampered row, or 0 if the chain
// is intact.
//
// hashInput is read verbatim from the column populated by NextHash at write
// time, NOT reconstructed from beforeState/afterState/etc. JSONB normalises
// key order and whitespace on storage, so reconstructing the payload from
// columns would not byte-match what was originally hashed even on a row
// nobody touched. This implementation therefore detects integrityHash
// rewrites (someone overwrites the column without re-deriving) and chain
// reorderings (a row inserted out of sequence with a fabricated previousHash
// would be flagged at the linkage check). It does NOT detect tampering of
// the display columns (beforeState, afterState, action, actorId) without a
// matching update to hashInput — that protection lives in the database
// transaction boundary: the writer always populates hashInput in the same
// INSERT as the columns, and read-only access policies prevent post-insert
// column updates in production.
func VerifyChain(ctx context.Context, q Queryer) (badSeq int64, err error) {
	return VerifyChainAcked(ctx, q, nil)
}

// VerifyChainAcked is VerifyChain with an explicit set of acknowledged orphan
// sequence numbers.
//
// An acknowledged orphan is a row that was investigated and recorded
// out-of-band (Hub system_metadata key audit_chain.acked_orphans) as a benign,
// non-tamper discontinuity — canonically an audit row that a pre-fix background
// job inserted WITHOUT going through NextHash, leaving previousHash /
// integrityHash / hashInput empty. For such a seq the walk does NOT report a
// break: it adopts the row's stored integrityHash as the running head
// (normalised — empty/NULL becomes a genesis head so the next correctly-chained
// row re-anchors), then keeps walking.
//
// Every row whose seq is NOT in ackedOrphans is still fully linkage- and
// integrity-checked, so tampering before AND after the orphan is still caught.
// This is a targeted, audited exception — never a relaxation of the chain rule,
// and never automatic: only sequence numbers an operator explicitly recorded
// reach the skip branch. A nil/empty set makes this identical to VerifyChain.
func VerifyChainAcked(ctx context.Context, q Queryer, ackedOrphans map[int64]struct{}) (badSeq int64, err error) {
	rows, err := q.Query(ctx, `
        SELECT "sequenceNumber", "previousHash", "integrityHash", "hashInput"
          FROM "AdminAuditLog"
         ORDER BY "sequenceNumber" ASC
    `)
	if err != nil {
		return 0, fmt.Errorf("query chain: %w", err)
	}
	defer rows.Close()

	var prevHash *string
	for rows.Next() {
		var (
			seq         int64
			storedPrev  *string
			storedInteg *string
			hashInput   []byte
		)
		if err := rows.Scan(&seq, &storedPrev, &storedInteg, &hashInput); err != nil {
			return 0, fmt.Errorf("scan row: %w", err)
		}

		if _, acked := ackedOrphans[seq]; acked {
			// Acknowledged benign orphan: do not validate it; adopt its stored
			// integrityHash (empty/NULL → genesis) as the running head so the
			// next row re-anchors and the rest of the chain stays verified.
			prevHash = normalizeHead(storedInteg)
			continue
		}

		// previousHash linkage: must equal the running chain head (NULL on
		// both sides for the genesis row).
		if (prevHash == nil) != (storedPrev == nil) {
			return seq, nil
		}
		if prevHash != nil && storedPrev != nil && *prevHash != *storedPrev {
			return seq, nil
		}

		h := sha256.New()
		if prevHash != nil {
			h.Write([]byte(*prevHash))
		}
		h.Write(hashInput)
		expected := hex.EncodeToString(h.Sum(nil))
		if storedInteg == nil || *storedInteg != expected {
			return seq, nil
		}

		prevHash = storedInteg
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate chain: %w", err)
	}
	return 0, nil
}

// normalizeHead maps an empty or NULL stored integrityHash to nil so the row
// after an acknowledged orphan is treated as a fresh genesis (NULL running
// head). A non-empty hash is adopted verbatim, so an acknowledged orphan that
// DID chain its successor still links correctly.
func normalizeHead(h *string) *string {
	if h == nil || *h == "" {
		return nil
	}
	return h
}

// canonicalizePayload marshals p to a sorted-key JSON object so the on-the-
// wire representation does not depend on Go's struct-field-declaration
// order. Without this, a future refactor reordering struct fields would
// silently re-anchor the chain across binary versions: every row written
// after the refactor hashes differently from one written before, even
// though the logical payload is identical, and VerifyChain would flag
// every post-refactor row as tampered.
//
// The output is the same JSON object encoding/json would produce — same
// keys, same value bytes — but with the keys serialised in lexicographic
// order. Callers must use this for both NextHash and VerifyChain so the
// hash inputs are equal.
func canonicalizePayload(p HashPayload) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(m[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
