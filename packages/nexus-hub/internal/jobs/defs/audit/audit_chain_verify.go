package audit

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/chain"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const auditChainVerifyJobID = "audit-chain-verify"

// ackedOrphansKey is the system_metadata key holding the operator-acknowledged
// audit-chain orphans (benign, investigated non-tamper breaks). Job-internal
// state, mirroring the semantic-cache job's last-reindexed key — not part of
// the 4-layer admin config, so it carries no configkey registration.
const ackedOrphansKey = "audit_chain.acked_orphans"

// ackedOrphanEntry is one acknowledged orphan record. Seq is the only field
// the verifier consumes; the rest are human-facing provenance retained so the
// row is a self-describing incident record.
type ackedOrphanEntry struct {
	Seq     int64  `json:"seq"`
	Reason  string `json:"reason,omitempty"`
	AckedBy string `json:"ackedBy,omitempty"`
	AckedAt string `json:"ackedAt,omitempty"`
}

// auditChainPool is the surface AuditChainVerify needs: the chain.Queryer
// (Query) used by VerifyChainAcked plus a single-row read for the acked-orphans
// key. *pgxpool.Pool satisfies it in production; pgxmock in tests.
type auditChainPool interface {
	chain.Queryer
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AuditChainVerify walks the AdminAuditLog hash chain end-to-end at every
// tick and emits a counter on detected breaks. The chain itself is written
// by chain.NextHash from two paths (CP→MQ→consumer and Hub direct in-tx);
// this job is the only thing that catches a row whose integrityHash no
// longer matches the recomputed value, which is the whole point of having
// a chain in the first place.
//
// One run reads every row in sequenceNumber order. Cost is O(N) on N audit
// rows, no JOINs. With audit log retention bounded by the existing retention
// job, even multi-million row tables finish in seconds. The default tick is
// 1 hour to balance detection latency against scan cost; ops can tune via
// cfg.Scheduler.AuditChainVerifyInterval.
type AuditChainVerify struct {
	// pool is typed against the narrow chain.Queryer surface (just Query)
	// so the test suite can inject pgxmock without sharing the real
	// Postgres AdminAuditLog table. VerifyChain walks the entire chain in
	// sequenceNumber order, so a real-DB test cannot be scoped to
	// test-owned rows without TRUNCATE-ing the table — which is exactly
	// what NEXUS_DESTRUCTIVE_TESTS=1 was added to guard.
	pool     auditChainPool
	interval time.Duration
	logger   *slog.Logger
	verified *opsmetrics.Counter
	breaks   *opsmetrics.Counter
}

// NewAuditChainVerify wires the job. Counters are registered against the
// shared opsmetrics.Registry so they show up in the same /metrics surface
// as the rest of the Hub's job metrics. opsReg may be nil in tests.
func NewAuditChainVerify(pool *pgxpool.Pool, interval time.Duration, opsReg *opsmetrics.Registry, logger *slog.Logger) *AuditChainVerify {
	var verified, breaks *opsmetrics.Counter
	if opsReg != nil {
		verified = opsReg.NewCounter("audit_chain.verified_runs_total", []string{})
		breaks = opsReg.NewCounter("audit_chain.break_detected_total", []string{})
	}
	return &AuditChainVerify{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", auditChainVerifyJobID),
		verified: verified,
		breaks:   breaks,
	}
}

func (j *AuditChainVerify) ID() string   { return auditChainVerifyJobID }
func (j *AuditChainVerify) Name() string { return "Audit Chain Verify" }
func (j *AuditChainVerify) Description() string {
	return "Walks the AdminAuditLog hash chain and reports tamper detection."
}
func (j *AuditChainVerify) Interval() time.Duration { return j.interval }
func (j *AuditChainVerify) RunOnStart() bool        { return true }

// Run reports a chain break by logging at error level and incrementing the
// break counter. It does NOT return the break as an error — a tampered chain
// is operational data, not a job failure, and we want the next tick to keep
// running. The error return is reserved for actual scan failures (DB down).
func (j *AuditChainVerify) Run(ctx context.Context) error {
	acked, err := j.loadAckedOrphans(ctx)
	if err != nil {
		// A real read failure (DB down) is a job failure — retry next tick.
		// We do NOT fall through to a full verify here: an unreadable ack set
		// is ambiguous, and erroring is louder than silently re-alerting.
		return err
	}
	badSeq, err := chain.VerifyChainAcked(ctx, j.pool, acked)
	if err != nil {
		return err
	}
	if j.verified != nil {
		j.verified.With().Inc()
	}
	if badSeq != 0 {
		if j.breaks != nil {
			j.breaks.With().Inc()
		}
		j.logger.Error("audit chain break detected",
			slog.String("event", "audit_chain_break"),
			slog.Int64("first_bad_sequence_number", badSeq),
		)
		return nil
	}
	j.logger.Info("audit chain intact",
		slog.String("event", "audit_chain_verified"),
	)
	return nil
}

// loadAckedOrphans reads the operator-acknowledged orphan set from the
// system_metadata key. Semantics:
//   - no row            → nil set (verify the full chain; the common case).
//   - corrupt JSON      → nil set + WARN (fail toward detection: better to keep
//     re-alerting on a real break than to silently skip rows from a bad blob).
//   - read/DB error     → surfaced to the caller (Run treats it as a job error).
func (j *AuditChainVerify) loadAckedOrphans(ctx context.Context) (map[int64]struct{}, error) {
	var raw []byte
	err := j.pool.QueryRow(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`, ackedOrphansKey,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}

	var entries []ackedOrphanEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		j.logger.Warn("audit-chain-verify: corrupt acked-orphans blob; verifying full chain",
			slog.String("key", ackedOrphansKey),
			slog.String("error", err.Error()),
		)
		return nil, nil
	}
	if len(entries) == 0 {
		return nil, nil
	}
	set := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		set[e.Seq] = struct{}{}
	}
	return set, nil
}
