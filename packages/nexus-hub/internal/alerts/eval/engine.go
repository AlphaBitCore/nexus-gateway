package alerteval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

const (
	// EngineJobID is the stable scheduler.Job identifier.
	EngineJobID          = "alerteval-engine"
	engineJobName        = "Alert Evaluator (Streaming)"
	engineJobDescription = "Subscribes to MQ traffic + audit events under consumer group hub-alerting, maintains in-memory ring buffers per registered Aggregator, and evaluates threshold rules every tick (default 5s)."

	// ConsumerGroup is the JetStream consumer group the Engine subscribes
	// to MQ under. Independent from hub-db-writer (TrafficEventWriter,
	// AdminAuditWriter); fan-out is the MQ driver's job.
	ConsumerGroup = "hub-alerting"
)

// trafficSubjects maps EventSource → MQ subject string.
var trafficSubjects = map[EventSource]string{
	SourceAITraffic:  "nexus.event.ai-traffic",
	SourceCompliance: "nexus.event.compliance",
	SourceAgent:      "nexus.event.agent",
	SourceAdminAudit: "nexus.event.admin-audit",
}

// Config holds Engine wiring knobs.
type Config struct {
	// TickSec is the evaluation cadence in seconds. Defaults to 5.
	TickSec int
	// StartTime overrides the cold-start gate baseline (tests inject a past
	// time to bypass the warmup gate). Defaults to time.Now().UTC().
	StartTime time.Time
}

// alertSink is the subset of *alerting.Raiser the Engine consumes.
// Declared here as an interface so unit tests can drive Run / handleDecision
// against a fake without spinning up the real Raiser (which requires a
// pgxpool.Pool, a Store, a Dispatcher, and an mq.Producer). *alerting.Raiser
// satisfies this interface naturally.
type alertSink interface {
	Raise(ctx context.Context, in alerting.RaiseInput) error
	Resolve(ctx context.Context, ruleID, targetKey, reason string) error
}

// ruleLister is the subset of *alerting.Store the Engine consumes — only
// ListRules. Same rationale as alertSink: unit tests inject a fake instead
// of constructing a Store-backed-by-pgxpool. *alerting.Store satisfies this
// interface naturally.
type ruleLister interface {
	ListRules(ctx context.Context, p alerting.ListRulesParams) ([]alerting.AlertRule, int, error)
}

// Engine is the streaming alert evaluator. Implements scheduler.Job so it
// shows up in the standard Hub job registry as alerteval-engine.
type Engine struct {
	cfg    Config
	pool   *pgxpool.Pool
	mqc    mq.Consumer
	raiser alertSink
	store  ruleLister
	logger *slog.Logger

	// consumeCtx is the long-lived context for the MQ consume goroutines
	// spawned in startMQOnce. **MUST NOT** be the per-tick ctx the
	// scheduler hands to Run(): the scheduler cancels that ctx the
	// moment Run returns, which would kill our consume goroutines
	// after the first tick (hub-alerting wedge: consumer sequence frozen
	// for 15 hours when goroutines were bound to the per-tick ctx).
	// consumeCancel is fired by Stop() for graceful shutdown.
	consumeCtx    context.Context
	consumeCancel context.CancelFunc

	mu          sync.RWMutex
	aggregators map[string]Aggregator
	runtimes    map[string]*Runtime

	mqStarted bool
}

// NewEngine constructs an Engine. Aggregators are added via Register before
// the first scheduler tick. Start subscribes to MQ once at boot.
func NewEngine(cfg Config, pool *pgxpool.Pool, mqc mq.Consumer, raiser *alerting.Raiser, store *alerting.Store, logger *slog.Logger) *Engine {
	if cfg.TickSec <= 0 {
		cfg.TickSec = 5
	}
	if cfg.StartTime.IsZero() {
		cfg.StartTime = time.Now().UTC()
	}
	if logger == nil {
		logger = slog.Default()
	}
	// consumeCtx is decoupled from any per-tick ctx the scheduler
	// passes in to Run. It lives for the lifetime of the Engine
	// (cancellable via Stop) so the consume goroutines spawned in
	// startMQOnce survive across scheduler ticks.
	consumeCtx, consumeCancel := context.WithCancel(context.Background())
	return &Engine{
		cfg:           cfg,
		pool:          pool,
		mqc:           mqc,
		raiser:        raiser,
		store:         store,
		logger:        logger.With("component", "alerteval-engine"),
		aggregators:   make(map[string]Aggregator),
		runtimes:      make(map[string]*Runtime),
		consumeCtx:    consumeCtx,
		consumeCancel: consumeCancel,
	}
}

// Stop cancels the engine's long-lived consume context, signalling all
// MQ consume goroutines to exit. Safe to call multiple times. Idempotent.
// Optional — process shutdown also kills the goroutines, so production
// callers can rely on that path. Mainly for clean test teardown.
func (e *Engine) Stop() {
	if e.consumeCancel != nil {
		e.consumeCancel()
	}
}

// Register adds an Aggregator. Must be called before Start.
func (e *Engine) Register(agg Aggregator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aggregators[agg.RuleID()] = agg
	e.runtimes[agg.RuleID()] = NewRuntime(agg.RuleID(), e.cfg.StartTime)
}

// scheduler.Job implementation — see packages/nexus-hub/internal/jobs/scheduler/scheduler.go

// ID returns the stable scheduler slug.
func (e *Engine) ID() string { return EngineJobID }

// Name returns the human-readable job name.
func (e *Engine) Name() string { return engineJobName }

// Description returns the job description shown in the admin UI.
func (e *Engine) Description() string { return engineJobDescription }

// Interval returns the tick cadence.
func (e *Engine) Interval() time.Duration {
	return time.Duration(e.cfg.TickSec) * time.Second
}

// RunOnStart returns true so the first eval doesn't wait for the first
// ticker tick. Cold-start gate handles the actual no-fire-during-warmup
// behaviour.
func (e *Engine) RunOnStart() bool { return true }

// Run is invoked every tick by the scheduler. Loads current rule.params
// from DB (so admin edits propagate within one tick), walks each registered
// Aggregator, and turns Decisions into Raise / Resolve calls.
func (e *Engine) Run(ctx context.Context) error {
	// Lazy MQ subscribe: scheduler invokes Run synchronously; we need MQ
	// consumption running in background goroutines. First Run starts them.
	e.startMQOnce(ctx)

	now := time.Now().UTC()

	rules, err := e.loadRules(ctx)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	for _, agg := range e.snapshotAggregators() {
		rule, ok := rules[agg.RuleID()]
		if !ok || !rule.Enabled {
			continue
		}
		rt := e.runtimes[agg.RuleID()]
		if rt.WarmupRemaining(agg.MinWarmupSec(rule.Params), now) > 0 {
			continue
		}
		decisions := agg.Tick(rt, rule.Params, now)
		for _, d := range decisions {
			e.handleDecision(ctx, rule, rt, d, now)
		}
	}
	return nil
}

func (e *Engine) startMQOnce(_ context.Context) {
	// Per-tick ctx is INTENTIONALLY ignored. See Engine.consumeCtx
	// doc — using the scheduler's per-tick ctx here caused all MQ
	// consume goroutines to die after the first tick (the consumer
	// sequence got stuck at 469 for 15 h, 41k events backed up in
	// NATS, eventually wedged the whole box). The fix routes consume
	// through the Engine's long-lived consumeCtx.
	e.mu.Lock()
	if e.mqStarted {
		e.mu.Unlock()
		return
	}
	e.mqStarted = true
	subjects := e.collectSubjectsLocked()
	e.mu.Unlock()

	for sub := range subjects {
		s := sub
		go e.runConsumeLoop(s)
	}
	e.logger.Info("alerteval engine subscribed",
		"subjects", len(subjects),
		"aggregators", len(e.aggregators),
		"tickSec", e.cfg.TickSec)
}

// runConsumeLoop owns one MQ consume goroutine for `subject`. If
// e.mqc.Consume returns (transient driver error, lost connection,
// JetStream consumer recreated, …) the loop logs and retries after a
// short backoff so the alerting engine recovers automatically. The
// pre-fix behavior was a SINGLE Consume call — any return killed
// consumption for that subject for the rest of the Hub's lifetime
// (no log when ctx.Err() != nil, so the failure was invisible).
//
// Exits only when consumeCtx is cancelled (Stop / process shutdown).
func (e *Engine) runConsumeLoop(subject string) {
	const backoff = 5 * time.Second
	for {
		err := e.mqc.Consume(e.consumeCtx, subject, ConsumerGroup, func(_ context.Context, msg *mq.Message) error {
			return e.handleMQMessage(subject, msg)
		})
		if e.consumeCtx.Err() != nil {
			// Engine.Stop or process shutdown — exit cleanly.
			return
		}
		// Consume returned without consumeCtx being cancelled: that's
		// always a regression (driver error, lost JetStream consumer,
		// panic-recover swallow, …). Log loudly so the symptom never
		// hides again, then resubscribe.
		e.logger.Error("alerteval MQ consume exited unexpectedly; resubscribing",
			"subject", subject, "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-e.consumeCtx.Done():
			return
		}
	}
}

// handleMQMessage processes one MQ event. Returning nil signals the
// shared/mq consumer to ack the message; returning ErrDeferAck would
// hand ack ownership back to us. Pre-fix the handler called
// `defer msg.Ack()` which double-acked every message — the natsmq
// consumer still autoacks on nil, then JetStream rejected our second
// ack with "nats: message was already acknowledged" and that warning
// flooded the Hub log under load. Now we simply return nil and let
// the consumer ack once.
func (e *Engine) handleMQMessage(subject string, msg *mq.Message) error {
	source, ok := subjectToSource(subject)
	if !ok {
		return nil
	}

	evt, err := decodeEvent(source, msg.Data)
	if err != nil {
		e.logger.Warn("decode failed; dropping message", "subject", subject, "error", err)
		return nil
	}

	for _, agg := range e.snapshotAggregators() {
		if !aggMatchesSource(agg, source) {
			continue
		}
		rt := e.runtimes[agg.RuleID()]
		agg.OnEvent(rt, evt)
	}
	return nil
}

func (e *Engine) handleDecision(ctx context.Context, rule alerting.AlertRule, rt *Runtime, d Decision, now time.Time) {
	switch d.Action {
	case Fire:
		if rt.IsCooldown(d.TargetKey, now) {
			return
		}
		severity := rule.DefaultSeverity
		if d.Severity != "" {
			// Decision.Severity is a free-form string produced by rule
			// evaluators (e.g. quota threshold tiers). Funnel through
			// the typed ParseLoose so a stale "WARN" or empty string
			// falls back to the rule's DefaultSeverity rather than
			// flowing through as an invalid AlertSeverity enum write.
			if parsed, err := alerting.ParseLoose(d.Severity); err == nil {
				severity = parsed
			} else {
				e.logger.Warn("invalid decision severity; using rule default",
					"ruleId", rule.ID, "value", d.Severity, "default", rule.DefaultSeverity)
			}
		}
		err := e.raiser.Raise(ctx, alerting.RaiseInput{
			RuleID:    rule.ID,
			TargetKey: d.TargetKey,
			Severity:  severity,
			Message:   d.Message,
			Details:   d.Details,
			FiredAt:   now,
		})
		if err != nil {
			e.logger.Error("raise failed",
				"ruleId", rule.ID, "targetKey", d.TargetKey, "error", err)
			return
		}
		rt.SetCooldown(d.TargetKey, now.Add(time.Duration(rule.CooldownSec)*time.Second))

	case Resolve:
		if err := e.raiser.Resolve(ctx, rule.ID, d.TargetKey, "auto"); err != nil {
			e.logger.Warn("resolve failed",
				"ruleId", rule.ID, "targetKey", d.TargetKey, "error", err)
		}
	}
}

func (e *Engine) snapshotAggregators() []Aggregator {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Aggregator, 0, len(e.aggregators))
	for _, a := range e.aggregators {
		out = append(out, a)
	}
	return out
}

// collectSubjectsLocked computes the set of MQ subjects across all
// registered Aggregators. Caller must hold e.mu.
func (e *Engine) collectSubjectsLocked() map[string]struct{} {
	out := make(map[string]struct{})
	for _, agg := range e.aggregators {
		for _, src := range agg.Sources() {
			if subj, ok := trafficSubjects[src]; ok {
				out[subj] = struct{}{}
			}
		}
	}
	return out
}

// loadRules returns the AlertRule rows for the registered aggregators.
// Re-reads on every tick — admin edits to rule.params are picked up within
// one tick (per spec A8).
func (e *Engine) loadRules(ctx context.Context) (map[string]alerting.AlertRule, error) {
	all, _, err := e.store.ListRules(ctx, alerting.ListRulesParams{Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := make(map[string]alerting.AlertRule, len(e.aggregators))
	for _, r := range all {
		if _, want := e.aggregators[r.ID]; want {
			out[r.ID] = r
		}
	}
	return out, nil
}

func decodeEvent(source EventSource, data []byte) (*Event, error) {
	if source == SourceAdminAudit {
		var msg mq.AdminAuditMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, err
		}
		return &Event{Kind: EventAudit, Source: source, Timestamp: msg.Timestamp, Audit: &msg}, nil
	}
	var msg consumer.TrafficEventMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &Event{Kind: EventTraffic, Source: source, Timestamp: msg.Timestamp, Traffic: &msg}, nil
}

func subjectToSource(subject string) (EventSource, bool) {
	for src, subj := range trafficSubjects {
		if subj == subject {
			return src, true
		}
	}
	return "", false
}

func aggMatchesSource(agg Aggregator, src EventSource) bool {
	for _, s := range agg.Sources() {
		if s == src {
			return true
		}
	}
	return false
}
