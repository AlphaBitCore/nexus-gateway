package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/chain"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const auditChainVerifyJobID = "audit-chain-verify"

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
	pool     chain.Queryer
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
	badSeq, err := chain.VerifyChain(ctx, j.pool)
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
