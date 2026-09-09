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
// 500 so batch 1 sweeps everything); it was caught by a 10K backfill.
// Loop exits when SELECT returns
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

		// One statement for the whole page instead of one per event. The page
		// is up to identityBatch rows and the loop has no upper bound, so the
		// per-event form scaled with the backlog it was there to drain.
		assignments, prefetchErr := e.prefetchIPAssignments(ctx, events)

		for _, evt := range events {
			if err := e.enrichEvent(ctx, evt, assignments, prefetchErr); err != nil {
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

func (e *IdentityEnricher) enrichEvent(
	ctx context.Context, evt store.PendingIdentityEvent,
	assignments map[string][]store.DeviceAssignmentWindow, prefetchErr error,
) error {
	// Method 1: the device this row came from, resolved deterministically.
	//
	// It runs first because it is the only leg whose input is authenticated:
	// thing_id is stamped by the Hub from the mTLS device token, so "which user
	// held this device then" has one answer. The two legs below infer from
	// values the uploading node supplied (a request id) or from an address many
	// devices can share (an IP), and a deterministic answer must not lose to a
	// heuristic that happens to run earlier.
	if match, err := e.tryThingIDMatch(ctx, evt); err == nil {
		return e.applyMatch(ctx, evt, match)
	}

	// Method 2: same-request match
	if match, err := e.tryRequestIDMatch(ctx, evt); err == nil {
		return e.applyMatch(ctx, evt, match)
	}

	// A FAILED prefetch is not "no match". Falling through to markUnmatched
	// would stamp a terminal verdict on rows the job never actually looked at,
	// and an unmatched row is outside both the Art.17 erase scope and the
	// Art.15 export scope. Returning the error leaves them pending for the next
	// run, which is the recoverable outcome.
	if prefetchErr != nil {
		return fmt.Errorf("ip assignment prefetch: %w", prefetchErr)
	}

	// Method 3: IP + agent match
	match, err := e.matchIPAgent(evt, assignments[evt.SourceIP])
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

// requestIDMatchWindow bounds how far apart two rows may be and still count as
// one request. A request id is self-reported by the uploading node, and the
// commonest ones are a framework's auto-incrementing counter — "1", "2" — so
// without a window the same value from two different machines a year apart
// would look like the same call. Ten minutes is far beyond any single request's
// life (the longest realtime session guard is 65 minutes but writes its rows as
// it goes) and far short of a counter's wrap.
const requestIDMatchWindow = 10 * time.Minute

// tryThingIDMatch resolves the user from the device the row came from.
//
// Only agent rows carry a thing_id, and only the Hub can put one there — it
// comes from the authenticated mTLS device token, never from the uploaded
// payload (see the ingest path's anti-forgery blanking). That is what makes
// this leg deterministic where the others are inferential.
func (e *IdentityEnricher) tryThingIDMatch(ctx context.Context, evt store.PendingIdentityEvent) (*identityMatch, error) {
	matched, err := e.store.TrafficStore().FindAssignmentByThingAndTime(ctx, evt.ThingID, evt.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &identityMatch{
		Method:     "thing_id",
		EntityID:   matched.UserID,
		EntityName: matched.DisplayName,
		Identity: map[string]any{
			"status": "matched",
			"method": "thing_id",
			"user": map[string]any{
				"id":    matched.UserID,
				"name":  matched.DisplayName,
				"email": matched.Email,
			},
			"device": map[string]any{"id": matched.DeviceID},
		},
	}, nil
}

// tryRequestIDMatch inherits a resolved identity from the GATEWAY's row for the
// same request — it resolved the caller from the virtual key — standing in for
// the agent / compliance-proxy row that could not.
//
// The narrowing lives in the store query (source, source_ip, time window) and
// is load-bearing: the request id on evt is self-reported by the node that
// uploaded the row, so an unnarrowed join would let an enrolled agent attribute
// its traffic to a victim by guessing a plausible id. See
// FindMatchedEventByRequestID.
func (e *IdentityEnricher) tryRequestIDMatch(ctx context.Context, evt store.PendingIdentityEvent) (*identityMatch, error) {
	matched, err := e.store.TrafficStore().FindMatchedEventByRequestID(
		ctx, evt.ExternalRequestID, evt.SourceIP, evt.CreatedAt, requestIDMatchWindow)
	if err != nil {
		return nil, err
	}
	return &identityMatch{
		Method:     "request_id",
		EntityID:   matched.EntityID,
		EntityName: matched.EntityName,
		Identity:   matched.Identity,
	}, nil
}

// prefetchIPAssignments reads, in one statement per IP chunk, every assignment
// whose window overlaps the timestamp span of this page, keyed by IP.
//
// The result is a SUPERSET: every event has its own timestamp, so matchIPAgent
// narrows with Covers. What it replaces is one query per event.
func (e *IdentityEnricher) prefetchIPAssignments(
	ctx context.Context, events []store.PendingIdentityEvent,
) (map[string][]store.DeviceAssignmentWindow, error) {
	ips := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))
	var from, to time.Time
	for _, evt := range events {
		if evt.SourceIP != "" && !seen[evt.SourceIP] {
			seen[evt.SourceIP] = true
			ips = append(ips, evt.SourceIP)
		}
		if from.IsZero() || evt.CreatedAt.Before(from) {
			from = evt.CreatedAt
		}
		if to.IsZero() || evt.CreatedAt.After(to) {
			to = evt.CreatedAt
		}
	}
	if len(ips) == 0 {
		return nil, nil
	}

	out := make(map[string][]store.DeviceAssignmentWindow, len(ips))
	// Chunked so one page cannot build an unbounded IN list; the chunk size is
	// well under any driver parameter limit and keeps the plan stable.
	for start := 0; start < len(ips); start += ipPrefetchChunk {
		end := min(start+ipPrefetchChunk, len(ips))
		rows, err := e.store.TrafficStore().FindAssignmentsByIPsOverlapping(ctx, ips[start:end], from, to)
		if err != nil {
			return nil, err
		}
		for _, w := range rows {
			out[w.IP] = append(out[w.IP], w)
		}
	}
	return out, nil
}

// ipPrefetchChunk bounds how many IPs go into one statement.
const ipPrefetchChunk = 200

// matchIPAgent picks the assignment covering this event's timestamp.
//
// The verdict vocabulary is unchanged, and it is the part that matters: zero
// covering windows is not-found, one is a match, and two or more REFUSES to name
// anybody. A shared NAT egress — office, VPN, a dev VM — is the ordinary case
// for two, and stamping the first would be a confidently wrong attribution
// rather than a missing one.
func (e *IdentityEnricher) matchIPAgent(
	evt store.PendingIdentityEvent, windows []store.DeviceAssignmentWindow,
) (*identityMatch, error) {
	if evt.SourceIP == "" {
		return nil, store.ErrNotFound
	}
	var found *store.DeviceAssignmentWindow
	for i := range windows {
		if !windows[i].Covers(evt.CreatedAt) {
			continue
		}
		if found != nil {
			// Counting past two buys nothing: the verdict is already "refuse".
			return nil, store.ErrAmbiguous
		}
		found = &windows[i]
	}
	if found == nil {
		return nil, store.ErrNotFound
	}
	assignment := found.DeviceAssignmentMatch

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
		"detail": "no thing_id, request_id or ip_agent match found",
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
