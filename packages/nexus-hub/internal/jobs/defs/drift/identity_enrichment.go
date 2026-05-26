package drift

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const (
	identityJobID          = "user-identity-enrichment"
	identityJobName        = "User Identity Enrichment"
	identityJobDescription = "Backfills user identity fields into recent traffic_event rows using IAM lookups."
	identityBatch          = 500
	identityLookback       = 24 * time.Hour
)

// IdentityEnricher enriches traffic events with user identity.
type IdentityEnricher struct {
	store    *store.Store
	interval time.Duration
	logger   *slog.Logger

	pendingTotal   *opsmetrics.Gauge
	matchedTotal   *opsmetrics.Counter
	unmatchedTotal *opsmetrics.Counter
	ambiguousTotal *opsmetrics.Counter
	matchByMethod  *opsmetrics.Counter
	durationMs     *opsmetrics.Histogram
	errorsTotal    *opsmetrics.Counter
}

// NewIdentityEnricher creates an identity enrichment job. None of these
// counters are in the spec §6.3 Hub catalog — they are job-internal and
// kept under the `identity.*` prefix.
func NewIdentityEnricher(
	st *store.Store,
	interval time.Duration,
	reg *opsmetrics.Registry,
	logger *slog.Logger,
) *IdentityEnricher {
	e := &IdentityEnricher{
		store:    st,
		interval: interval,
		logger:   logger.With("job", identityJobID),
	}
	if reg != nil {
		e.pendingTotal = reg.NewGauge("identity.pending_total", nil)
		e.matchedTotal = reg.NewCounter("identity.matched_total", nil)
		e.unmatchedTotal = reg.NewCounter("identity.unmatched_total", nil)
		e.ambiguousTotal = reg.NewCounter("identity.ambiguous_total", nil)
		e.matchByMethod = reg.NewCounter("identity.match_by_method_total", []string{"method"})
		e.durationMs = reg.NewHistogram("identity.enrichment_duration_ms", nil)
		e.errorsTotal = reg.NewCounter("identity.enrichment_errors_total", nil)
	}
	return e
}

func (e *IdentityEnricher) ID() string              { return identityJobID }
func (e *IdentityEnricher) Name() string            { return identityJobName }
func (e *IdentityEnricher) Description() string     { return identityJobDescription }
func (e *IdentityEnricher) Interval() time.Duration { return e.interval }

// Run processes pending identity events in batches of 500 until the
// pending set is exhausted.
//
// Pagination is offset=0 every iteration, NOT offset += batch. After
// each batch, enrichEvent calls UpdateEventIdentity which flips
// status from "pending" to matched/unmatched/ambiguous; those rows
// then fall out of FindPendingIdentityEvents's `status='pending'`
// SELECT result entirely. Using a moving OFFSET on a shrinking set
// double-counts the gap — OFFSET 500 after processing the first 500
// skips ANOTHER 500 rows that should be handled. The bug only
// surfaces with large backlogs (real-world cron pending stays under
// 500 so batch 1 sweeps everything); it was caught by a 10K backfill
// on prod-20260515-identity-fix. Loop exits when SELECT returns
// fewer rows than batch size (terminal short read = nothing left to
// page through).
//
// pendingTotal Gauge tracks the FIRST-batch size at the start of each
// Run, so operators see "how big was the queue when we picked it up"
// rather than the trailing zero from the last empty SELECT.
func (e *IdentityEnricher) Run(ctx context.Context) error {
	start := time.Now()
	defer func() {
		if e.durationMs != nil {
			e.durationMs.With().Observe(float64(time.Since(start).Milliseconds()))
		}
	}()

	firstBatch := true
	for {
		events, err := e.store.TrafficStore().FindPendingIdentityEvents(ctx, identityLookback, identityBatch)
		if err != nil {
			return fmt.Errorf("find pending events: %w", err)
		}

		if firstBatch && e.pendingTotal != nil {
			e.pendingTotal.With().Set(float64(len(events)))
			firstBatch = false
		}

		for _, evt := range events {
			if err := e.enrichEvent(ctx, evt); err != nil {
				if e.errorsTotal != nil {
					e.errorsTotal.With().Inc()
				}
				e.logger.Warn("enrich failed", "event_id", evt.ID, "error", err)
			}
		}

		if len(events) < identityBatch {
			break
		}
	}
	return nil
}

func (e *IdentityEnricher) enrichEvent(ctx context.Context, evt store.PendingIdentityEvent) error {
	// Method 1: trace_id match
	if match, err := e.tryTraceIDMatch(ctx, evt); err == nil {
		return e.applyMatch(ctx, evt, match)
	}

	// Method 2: IP + agent match
	match, err := e.tryIPAgentMatch(ctx, evt)
	if err == nil {
		return e.applyMatch(ctx, evt, match)
	}
	// 2+ DA rows share this source_ip (NAT-shared egress: office /
	// VPN / coffee shop / shared dev VM). Stamping the first match
	// arbitrarily would misattribute traffic to the wrong user, which
	// is worse than no attribution. Mark ambiguous so operators can
	// see contention and pick a resolution strategy (e.g. require SSO
	// session cookies for those subnets).
	if errors.Is(err, store.ErrAmbiguous) {
		return e.markAmbiguous(ctx, evt)
	}

	// No match found
	return e.markUnmatched(ctx, evt)
}

type identityMatch struct {
	Method     string
	EntityID   string
	EntityName string
	Identity   map[string]any
}

func (e *IdentityEnricher) tryTraceIDMatch(ctx context.Context, evt store.PendingIdentityEvent) (*identityMatch, error) {
	matched, err := e.store.TrafficStore().FindMatchedEventByTraceID(ctx, evt.TraceID)
	if err != nil {
		return nil, err
	}
	return &identityMatch{
		Method:     "trace_id",
		EntityID:   matched.EntityID,
		EntityName: matched.EntityName,
		Identity:   matched.Identity,
	}, nil
}

func (e *IdentityEnricher) tryIPAgentMatch(ctx context.Context, evt store.PendingIdentityEvent) (*identityMatch, error) {
	// Resolve identity via DeviceAssignment ip_address + time window.
	assignment, err := e.store.TrafficStore().FindActiveAssignmentByIPAndTime(ctx, evt.SourceIP, evt.CreatedAt)
	if err != nil {
		return nil, err
	}

	identity := map[string]any{
		"status": "matched",
		"method": "ip_agent",
		"user": map[string]any{
			"id":    assignment.UserID,
			"name":  assignment.DisplayName,
			"email": assignment.Email,
		},
		"device": map[string]any{
			"id": assignment.DeviceID,
		},
	}
	return &identityMatch{
		Method:     "ip_agent",
		EntityID:   assignment.UserID,
		EntityName: assignment.DisplayName,
		Identity:   identity,
	}, nil
}

func (e *IdentityEnricher) applyMatch(ctx context.Context, evt store.PendingIdentityEvent, match *identityMatch) error {
	// Ensure the identity has 'status: matched'
	if match.Identity == nil {
		match.Identity = map[string]any{}
	}
	match.Identity["status"] = "matched"
	match.Identity["method"] = match.Method

	err := e.store.TrafficStore().UpdateEventIdentity(ctx, store.UpdateEventIdentityParams{
		EventID:    evt.ID,
		EntityID:   match.EntityID,
		EntityName: match.EntityName,
		Identity:   match.Identity,
	})
	if err != nil {
		return err
	}

	if e.matchedTotal != nil {
		e.matchedTotal.With().Inc()
	}
	if e.matchByMethod != nil {
		e.matchByMethod.With(match.Method).Inc()
	}
	return nil
}

func (e *IdentityEnricher) markUnmatched(ctx context.Context, evt store.PendingIdentityEvent) error {
	identity := map[string]any{
		"status": "unmatched",
		"detail": "no trace_id or ip_agent match found",
	}
	err := e.store.TrafficStore().UpdateEventIdentity(ctx, store.UpdateEventIdentityParams{
		EventID:  evt.ID,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	if e.unmatchedTotal != nil {
		e.unmatchedTotal.With().Inc()
	}
	return nil
}

// markAmbiguous records that 2+ active DeviceAssignment rows share the
// same source_ip and the lookup cannot pick a winner. Deliberately
// leaves entity_id / entity_name blank — we refuse to commit to a user
// rather than guess. ambiguous rows do not later auto-resolve; if the
// operator wants attribution they need a richer signal (session
// cookie, X-User header, …) or to retire the duplicate DA rows.
func (e *IdentityEnricher) markAmbiguous(ctx context.Context, evt store.PendingIdentityEvent) error {
	identity := map[string]any{
		"status": "ambiguous",
		"method": "ip_agent",
		"detail": "multiple devices share this source_ip (shared NAT egress)",
	}
	err := e.store.TrafficStore().UpdateEventIdentity(ctx, store.UpdateEventIdentityParams{
		EventID:  evt.ID,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	if e.ambiguousTotal != nil {
		e.ambiguousTotal.With().Inc()
	}
	return nil
}
