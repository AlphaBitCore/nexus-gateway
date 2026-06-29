package consumer

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TrafficEventWriterConfig holds configuration for the traffic event writer.
type TrafficEventWriterConfig struct {
	BatchSize     int           `yaml:"batchSize"`
	FlushInterval time.Duration `yaml:"flushInterval"`

	// DrainDutyCycle controls how the audit drain yields CPU to a co-located
	// AI-gateway's core request path. After each batch flush the drain sleeps in
	// proportion to how long the flush took, so a duty cycle d means "drain at most
	// d of wall-clock, idle the rest"; the NATS file store (local disk, effectively
	// unbounded) absorbs the backlog while idle and the Hub catches up when the
	// gateway is quiet. Audit is delay-tolerant; no-loss is preserved by NATS
	// retention, not by racing the gateway.
	//
	// Three modes by value (the shipped config default is 0.3 — see the Hub
	// config defaults / yaml `consumers.trafficDrainDutyCycle`):
	//   - in (0,1) → FIXED throttle at that duty cycle. 0.3 is the single-box
	//     default: it reliably yields the box's memory bandwidth / loopback /
	//     Postgres to the gateway core path (the CPU probe below cannot see
	//     memory-bandwidth contention on a core-rich box).
	//   - 0 → ADAPTIVE: a real-time CPU-pressure probe (timer overshoot) sets the
	//     duty dynamically — full-speed when the box has spare CPU, throttling only
	//     under measured core contention. Best on a small/CPU-bound box.
	//   - >= 1 → OFF: always full-speed, no probe, no throttle (dedicated Hub box).
	//
	// Measured on the single-box perf rig (gateway saturating the box): the drain
	// throttling lifted gateway 200-VU RPS ~5150 -> ~6290 with no loss. yaml
	// drainDutyCycle / env NEXUS_HUB_AUDIT_DRAIN_DUTY_CYCLE.
	DrainDutyCycle float64 `yaml:"drainDutyCycle"`
}

// maxDrainPaceSleep caps a single post-flush pacing sleep so a pathologically
// slow flush (e.g. a Postgres stall) cannot park the drain for an unbounded time
// and let the NATS backlog grow without bound.
const maxDrainPaceSleep = 250 * time.Millisecond

// backlogDutyOverride is the NATS-stream fill fraction (used bytes / MaxBytes) at
// or above which the drain abandons its CPU-yield throttle and runs FULL-SPEED,
// regardless of the configured/adaptive duty cycle. The duty throttle assumes the
// gateway has quiet windows for the drain to catch up; under sustained saturation
// there are none, so a fixed/adaptive throttle lets the backlog grow until the
// stream hits MaxBytes and DiscardNew starts rejecting publishes (the gateway then
// spills to disk). Overriding to full-speed once the stream is half-full turns that
// "wedge then shed" failure mode into bounded catch-up: the drain prioritises
// emptying the backlog over yielding CPU exactly when the backlog is dangerous.
const backlogDutyOverride = 0.5

// backlogSampleInterval / backlogSampleTimeout bound the live stream-fill probe:
// a low-frequency refresh (the override only needs to react within a couple
// seconds of the backlog climbing) with a short per-query timeout so a stalled
// broker never blocks the sampler goroutine.
const (
	backlogSampleInterval = 2 * time.Second
	backlogSampleTimeout  = 1 * time.Second
)

type pendingTrafficMessage struct {
	event TrafficEventMessage
	msg   *mq.Message // shared NATS message — metadata only (Subject, NumDelivered)
	raw   []byte      // THIS record's own bytes (DLQ payload); == msg.Data for a legacy single-record message
	frame *frameAck   // resolves the shared msg once every record in the frame is durably handled
}

// payloadBytes returns the bytes to persist for THIS record in a DLQ sink: the
// per-record frame line when set, falling back to the whole message data for a
// directly-constructed (unframed) record.
func (pm pendingTrafficMessage) payloadBytes() []byte {
	if pm.raw != nil {
		return pm.raw
	}
	return pm.msg.Data
}

// PgxPool is the minimum pgx pool surface the writers in this package need
// — only flush() touches the pool directly (Begin tx; the rest of the
// insert path operates on the resulting pgx.Tx, which is already an
// interface in pgx and needs no seam of its own). The concrete
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface
// satisfies it in tests, letting flush()'s Begin→SendBatch→Commit chain
// be exercised without a live Postgres. Mirrors the PgxPool convention
// from packages/nexus-hub/internal/observability/siem/bridge.go,
// packages/nexus-hub/internal/alerts/engine/store.go, and
// packages/ai-gateway/internal/cache/layer/layer.go.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	// Exec runs a single statement outside any caller-held transaction.
	// Used by the DLQ insert path which must succeed even when the
	// flush tx itself has rolled back.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// TrafficEventWriter consumes traffic events from 3 MQ queues and batch-inserts
// them into the traffic_event table. Consumer group: "hub-db-writer".
type TrafficEventWriter struct {
	pool   PgxPool // interface seam — *pgxpool.Pool in prod, pgxmock in tests
	mqc    mq.Consumer
	cfg    TrafficEventWriterConfig
	logger *slog.Logger

	// consumed_total / flush_total / traffic_errors_total align with
	// mq.processed_total{stream, status} but the existing label scheme
	// (queue, result, error_type) is more diagnostic for the writer path,
	// so kept verbatim under mq.* dotted names. The error counter is
	// traffic_errors_total (not errors_total) so it doesn't collide with the
	// shared MQ transport layer's unlabeled nexus_mq_errors_total — same
	// per-consumer namespacing the exemption/admin/siem writers use.
	consumedTotal    *opsmetrics.Counter
	flushTotal       *opsmetrics.Counter
	batchSizeHist    *opsmetrics.Histogram
	errorsTotal      *opsmetrics.Counter
	dlqInsertedTotal *opsmetrics.Counter
	diskDLQTotal     *opsmetrics.Counter

	// diskDLQ is the DB-independent, on-disk dead-letter sink used only when
	// the DB-backed insertDLQ itself fails (DB unreachable). Never nil after
	// construction.
	diskDLQ *diskDLQ

	// pacer drives the adaptive drain duty cycle from a real-time CPU-pressure
	// probe; non-nil only in adaptive mode (cfg.DrainDutyCycle == 0). nil for the
	// fixed-throttle and off modes.
	pacer *adaptiveDrainPacer

	// backlogProbe, when set, reports the NEXUS_EVENTS stream fill fraction
	// (used bytes / MaxBytes, 0..1) and whether the reading is valid. drainDuty
	// uses it to override the throttle to full-speed once the backlog is dangerous
	// (>= backlogDutyOverride), turning the sustained-saturation wedge into bounded
	// catch-up. nil → no override (the pre-existing throttle behaviour). Set
	// directly via WithBacklogProbe (tests), or built from a sampler by Start.
	backlogProbe func() (float64, bool)

	// backlogSampleFn, when set, is a LIVE (network) query of the stream fill
	// fraction. Start wraps it in a low-frequency cached sampler so drainDuty
	// (called per flush) reads a cheap atomic, not a broker round-trip. Set via
	// WithBacklogSampler. Ignored if backlogProbe is already set.
	backlogSampleFn func(context.Context) (float64, bool)
}

// WithBacklogProbe wires a cheap, already-cached fill-fraction reader directly
// (used in tests). Production uses WithBacklogSampler. Call before Start.
func (w *TrafficEventWriter) WithBacklogProbe(p func() (float64, bool)) *TrafficEventWriter {
	w.backlogProbe = p
	return w
}

// WithBacklogSampler wires a LIVE stream fill-fraction query; Start caches it
// behind a low-frequency sampler so the per-flush drainDuty stays cheap. Call
// before Start. Returns the receiver for chaining.
func (w *TrafficEventWriter) WithBacklogSampler(fn func(context.Context) (float64, bool)) *TrafficEventWriter {
	w.backlogSampleFn = fn
	return w
}

// startBacklogSampler launches the goroutine that periodically refreshes the
// cached backlog fill fraction from the live sampler, and points backlogProbe at
// the cache. A nil sampler (or an already-set probe) is a no-op.
func (w *TrafficEventWriter) startBacklogSampler(ctx context.Context) {
	if w.backlogSampleFn == nil || w.backlogProbe != nil {
		return
	}
	var fracBits atomic.Uint64
	var valid atomic.Bool
	w.backlogProbe = func() (float64, bool) {
		return math.Float64frombits(fracBits.Load()), valid.Load()
	}
	sample := func() {
		cctx, cancel := context.WithTimeout(ctx, backlogSampleTimeout)
		defer cancel()
		if f, ok := w.backlogSampleFn(cctx); ok {
			fracBits.Store(math.Float64bits(f))
			valid.Store(true)
		}
	}
	go func() {
		t := time.NewTicker(backlogSampleInterval)
		defer t.Stop()
		sample() // prime immediately so the override can engage on the first flush
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sample()
			}
		}
	}()
}

// TrafficQueues lists the 3 MQ queues this consumer reads from.
var TrafficQueues = []string{
	"nexus.event.ai-traffic",
	"nexus.event.compliance",
	"nexus.event.agent",
}

const dbWriterGroup = "hub-db-writer"

// NewTrafficEventWriter creates a new writer. Call Start(ctx) to begin consuming.
// reg powers both /metrics and the per-tick metrics_sample push; pass nil
// only in test harnesses that do not exercise the metrics path.
func NewTrafficEventWriter(
	pool *pgxpool.Pool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	return newTrafficEventWriter(pool, mqc, cfg, logger, reg)
}

// NewTrafficEventWriterWithPgxPool is the test-only constructor accepting any
// PgxPool — production code goes through NewTrafficEventWriter. Lets the
// flush()'s Begin→SendBatch→Commit chain be driven through pgxmock without a
// live Postgres so the error branches (begin failure, insert failure with
// 22021 poison-pill ack vs nakAll, payload failure, normalized warn-and-
// continue, commit failure, ackAll success) are exercised in unit tests.
func NewTrafficEventWriterWithPgxPool(
	pool PgxPool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	return newTrafficEventWriter(pool, mqc, cfg, logger, reg)
}

func newTrafficEventWriter(
	pool PgxPool,
	mqc mq.Consumer,
	cfg TrafficEventWriterConfig,
	logger *slog.Logger,
	reg *opsmetrics.Registry,
) *TrafficEventWriter {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	// Env override for the drain duty cycle (co-located perf knob); yaml is the
	// declarative default, env wins for a redeploy-free flip on the perf rig.
	if v := os.Getenv("NEXUS_HUB_AUDIT_DRAIN_DUTY_CYCLE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.DrainDutyCycle = f
		}
	}

	w := &TrafficEventWriter{
		pool:    pool,
		mqc:     mqc,
		cfg:     cfg,
		logger:  logger.With("component", "traffic-event-writer"),
		diskDLQ: newDiskDLQ(""),
	}
	// Adaptive mode (duty cycle 0): a CPU-pressure probe sets the duty dynamically.
	if cfg.DrainDutyCycle == 0 {
		w.pacer = newAdaptiveDrainPacer()
	}
	if reg != nil {
		w.consumedTotal = reg.NewCounter("mq.processed_total", []string{"queue"})
		w.flushTotal = reg.NewCounter("mq.batch_flush_total", []string{"result"})
		w.batchSizeHist = reg.NewHistogram("mq.batch_size", nil)
		w.errorsTotal = reg.NewCounter("mq.traffic_errors_total", []string{"error_type"})
		w.dlqInsertedTotal = reg.NewCounter("mq.dlq_inserted_total", []string{"subject"})
		w.diskDLQTotal = reg.NewCounter("mq.disk_dlq_inserted_total", []string{"subject"})
	}
	return w
}

// Start begins consuming from all 3 event queues in parallel goroutines.
// Blocks until ctx is cancelled.
func (w *TrafficEventWriter) Start(ctx context.Context) error {
	// In adaptive mode, run the CPU-pressure probe that drives the drain duty cycle.
	if w.pacer != nil {
		go w.pacer.run(ctx)
	}
	// Cache the NATS backlog fill fraction so drainDuty can override the throttle
	// to full-speed when the stream nears MaxBytes (the sustained-saturation guard).
	w.startBacklogSampler(ctx)
	workers := drainWorkersPerQueue()
	for _, queue := range TrafficQueues {
		q := queue
		batch := NewBatchAccumulator[pendingTrafficMessage](w.cfg.BatchSize, w.cfg.FlushInterval, func(items []pendingTrafficMessage) error {
			start := time.Now()
			err := w.flush(ctx, items)
			// Yield CPU to the gateway's core path in proportion to the flush cost
			// (no-op when the duty cycle is disabled). The sleep blocks this consume
			// goroutine, which paces the next Fetch — the NATS store holds the
			// backlog meanwhile.
			w.paceDrain(ctx, time.Since(start))
			return err
		})

		// N parallel drain workers per queue share ONE BatchAccumulator. The drain
		// was single-goroutine-per-queue, so the synchronous PG flush stalled the
		// NATS fetch loop on every batch — the hub sat at ~5% CPU, NATS backed up,
		// and the audit pipeline overflowed (lossy under load) even though the box
		// had ample disk + CPU. With multiple workers, while one is flushing to
		// PostgreSQL the others keep fetching and filling the next batch, so fetch
		// pipelines against PG-write. BatchAccumulator.flushLocked releases its lock
		// across flushFn precisely so concurrent fill-to-maxSize Adds each enter
		// flushFn on their own goroutine + tx; w.flush opens its own tx per call, so
		// concurrent flushes are independent. batch.Stop() runs once, after all this
		// queue's workers exit.
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := w.mqc.Consume(ctx, q, dbWriterGroup, func(_ context.Context, msg *mq.Message) error {
					return w.handleMessage(q, batch, msg)
				})
				if err != nil && ctx.Err() == nil {
					w.logger.Error("consumer exited with error", "queue", q, "error", err)
				}
			}()
		}
		go func() { wg.Wait(); _ = batch.Stop() }()
	}

	<-ctx.Done()
	return nil
}
