package jwtverifier

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bits-and-blooms/bloom/v3"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// minEventShape is the subset of a nexus.auth.revocation payload consumed by
// the checker. Kept local so shared/jwtverifier never has to import the
// control-plane revocation package (which already depends on shared, and the
// reverse dependency would create a module cycle).
type minEventShape struct {
	EventID         string    `json:"event_id"`
	RevokedAt       time.Time `json:"revoked_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Scope           string    `json:"scope"`
	TargetJTI       string    `json:"target_jti,omitempty"`
	TargetUserID    string    `json:"target_user_id,omitempty"`
	TargetDeviceID  string    `json:"target_device_id,omitempty"`
	TargetSessionID string    `json:"target_session_id,omitempty"`
}

// Canonical scope values matching the publisher and RFC 7009 + CP spec.
const (
	scopeJTI     = "jti"
	scopeUser    = "user"
	scopeDevice  = "device"
	scopeSession = "session"
)

// MQCheckerConfig wires an MQRevocationChecker.
//
// IntrospectURL and ReplayURL are optional: when unset the checker behaves as
// a pure in-memory accumulator (useful for dev and tests that only exercise
// HandleMessage). Production deployments set both.
type MQCheckerConfig struct {
	// IntrospectURL is the POST endpoint of the auth server's RFC 7662
	// introspection endpoint. Hit on bloom false positives and in strict mode.
	IntrospectURL string

	// ReplayURL is the GET endpoint that serves paged missed events
	// (?since=<lastID>&limit=N). Used by RunCatchup after a fresh start or a
	// consumer reconnect.
	ReplayURL string

	// ReplayAuthHeader is sent verbatim as the Authorization header on both
	// introspect and replay requests. Typically "Bearer <rs-access-token>".
	ReplayAuthHeader string

	// HTTPClient is used for introspect and replay requests. Defaults to a
	// client with a 5-second timeout when nil.
	HTTPClient *http.Client

	// DisconnectTimeout is how long the checker waits without seeing a fresh
	// MQ event before flipping into strict mode. Defaults to 30s.
	DisconnectTimeout time.Duration

	// ExpectedJTIs sizes the bloom filter. Defaults to 100000.
	ExpectedJTIs uint

	// FalsePositiveRate tunes the bloom filter. Defaults to 0.001.
	FalsePositiveRate float64

	// Logger defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// MQRevocationChecker implements RevocationChecker backed by an in-memory
// bloom filter of revoked JTIs plus per-subject, per-device, and per-session
// cutoff maps. Updates arrive over shared/mq on the nexus.auth.revocation
// topic; a disconnect longer than DisconnectTimeout flips the checker into
// strict mode, where every IsRevoked call round-trips to /oauth/introspect.
//
// Safe for concurrent use.
type MQRevocationChecker struct {
	cfg MQCheckerConfig

	mu        sync.RWMutex
	filter    *bloom.BloomFilter
	byUser    map[string]time.Time
	byDevice  map[string]time.Time
	bySession map[string]time.Time
	// byJTI holds the exact set of JTI-scoped revocations backing the bloom
	// filter; used to disambiguate bloom false positives without paying an
	// introspect round-trip on every hit.
	byJTI map[string]time.Time

	// lastID tracks the most recent replay checkpoint. Only mutated inside
	// RunCatchup from the auth server's replay response; MQ messages carry
	// opaque string event ids so HandleMessage leaves lastID untouched.
	lastID atomic.Int64

	// strict flips to true when DisconnectTimeout elapses without a fresh
	// event and back to false the next time HandleMessage applies one.
	strict atomic.Bool

	// lastEvent is the unix-second timestamp of the most recent applied event
	// or, at startup, the constructor call. The disconnect ticker uses it to
	// detect MQ silence.
	lastEvent atomic.Int64
}

// NewMQRevocationChecker builds a checker with empty state. Call StartConsumer
// to subscribe to the MQ topic; callers that only need to RunCatchup
// periodically may skip StartConsumer entirely.
func NewMQRevocationChecker(cfg MQCheckerConfig) *MQRevocationChecker {
	if cfg.DisconnectTimeout == 0 {
		cfg.DisconnectTimeout = 30 * time.Second
	}
	if cfg.ExpectedJTIs == 0 {
		cfg.ExpectedJTIs = 100_000
	}
	if cfg.FalsePositiveRate == 0 {
		cfg.FalsePositiveRate = 0.001
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = nexushttp.New(nexushttp.Config{
			Timeout:        5 * time.Second,
			Caller:         "cp-jwt-mqrevocation",
			PropagateReqID: true,
		})
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	c := &MQRevocationChecker{
		cfg:       cfg,
		filter:    bloom.NewWithEstimates(cfg.ExpectedJTIs, cfg.FalsePositiveRate),
		byUser:    make(map[string]time.Time),
		byDevice:  make(map[string]time.Time),
		bySession: make(map[string]time.Time),
		byJTI:     make(map[string]time.Time),
	}
	c.lastEvent.Store(time.Now().Unix())
	return c
}

// IsRevoked implements RevocationChecker. In strict mode every call
// round-trips to the introspect endpoint. In normal mode the call is served
// from in-memory state unless the bloom reports a possible hit that the exact
// byJTI set cannot confirm.
//
// The read lock is released BEFORE any HTTP call to introspect so that
// concurrent HandleMessage and RunCatchup writers are never blocked by a
// network round-trip.
func (c *MQRevocationChecker) IsRevoked(ctx context.Context, claims *Claims) (bool, error) {
	if c.strict.Load() {
		return c.introspect(ctx, claims)
	}

	c.mu.RLock()

	// Check JTI bloom first. If the bloom reports a definite miss, we can
	// answer false immediately while still holding the read lock.
	jtiBloomHit := claims.JTI != "" && c.filter.TestString(claims.JTI)
	jtiExact := false
	if jtiBloomHit {
		_, jtiExact = c.byJTI[claims.JTI]
	}

	// Read cutoff state into locals so we can release the lock before any
	// potential HTTP call.
	iat := time.Unix(claims.IssuedAt, 0)
	userRevoked := false
	if cutoff, ok := c.byUser[claims.Subject]; ok && cutoff.After(iat) {
		userRevoked = true
	}
	deviceRevoked := false
	if claims.DeviceID != "" {
		if cutoff, ok := c.byDevice[claims.DeviceID]; ok && cutoff.After(iat) {
			deviceRevoked = true
		}
	}
	sessionRevoked := false
	if claims.SessionID != "" {
		if cutoff, ok := c.bySession[claims.SessionID]; ok && cutoff.After(iat) {
			sessionRevoked = true
		}
	}

	c.mu.RUnlock()

	// JTI exact match - confirmed revoked.
	if jtiBloomHit && jtiExact {
		return true, nil
	}

	// Bloom false positive - ask auth server (lock already released).
	if jtiBloomHit && !jtiExact {
		return c.introspect(ctx, claims)
	}

	if userRevoked || deviceRevoked || sessionRevoked {
		return true, nil
	}
	return false, nil
}

// HandleMessage applies a raw MQ payload to the checker state. Safe for
// concurrent use. A successful apply clears strict mode.
func (c *MQRevocationChecker) HandleMessage(_ context.Context, payload []byte) error {
	var ev minEventShape
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("mqrevocation: decode: %w", err)
	}
	c.mu.Lock()
	c.applyLocked(ev)
	c.mu.Unlock()

	c.lastEvent.Store(time.Now().Unix())
	if c.strict.CompareAndSwap(true, false) {
		c.cfg.Logger.Info("mqrevocation: leaving strict mode")
	}
	return nil
}

// applyLocked merges one event into the in-memory state. Caller must hold
// c.mu for writing. Events with an empty target ID or an unrecognized scope
// are dropped (to honor the MQ delivery contract) but a Warn is emitted so
// operators can spot malformed publishers.
func (c *MQRevocationChecker) applyLocked(ev minEventShape) {
	switch ev.Scope {
	case scopeJTI:
		if ev.TargetJTI == "" {
			if c.cfg.Logger != nil {
				c.cfg.Logger.Warn("mqrevocation: dropping event with empty target_jti",
					slog.String("event_id", ev.EventID),
					slog.String("scope", ev.Scope),
				)
			}
			return
		}
		c.filter.AddString(ev.TargetJTI)
		c.byJTI[ev.TargetJTI] = ev.ExpiresAt
	case scopeUser:
		if ev.TargetUserID == "" {
			if c.cfg.Logger != nil {
				c.cfg.Logger.Warn("mqrevocation: dropping event with empty target_user_id",
					slog.String("event_id", ev.EventID),
					slog.String("scope", ev.Scope),
				)
			}
			return
		}
		if t, ok := c.byUser[ev.TargetUserID]; !ok || ev.RevokedAt.After(t) {
			c.byUser[ev.TargetUserID] = ev.RevokedAt
		}
	case scopeDevice:
		if ev.TargetDeviceID == "" {
			if c.cfg.Logger != nil {
				c.cfg.Logger.Warn("mqrevocation: dropping event with empty target_device_id",
					slog.String("event_id", ev.EventID),
					slog.String("scope", ev.Scope),
				)
			}
			return
		}
		if t, ok := c.byDevice[ev.TargetDeviceID]; !ok || ev.RevokedAt.After(t) {
			c.byDevice[ev.TargetDeviceID] = ev.RevokedAt
		}
	case scopeSession:
		if ev.TargetSessionID == "" {
			if c.cfg.Logger != nil {
				c.cfg.Logger.Warn("mqrevocation: dropping event with empty target_session_id",
					slog.String("event_id", ev.EventID),
					slog.String("scope", ev.Scope),
				)
			}
			return
		}
		if t, ok := c.bySession[ev.TargetSessionID]; !ok || ev.RevokedAt.After(t) {
			c.bySession[ev.TargetSessionID] = ev.RevokedAt
		}
	default:
		if c.cfg.Logger != nil {
			c.cfg.Logger.Warn("mqrevocation: dropping event with unrecognized scope",
				slog.String("event_id", ev.EventID),
				slog.String("scope", ev.Scope),
				slog.String("target_jti", ev.TargetJTI),
				slog.String("target_user_id", ev.TargetUserID),
				slog.String("target_device_id", ev.TargetDeviceID),
				slog.String("target_session_id", ev.TargetSessionID),
			)
		}
	}
}

// introspect performs a RFC 7662 token-introspection call to disambiguate a
// bloom false positive or satisfy a strict-mode check.
//
// Failure policy: when the introspect endpoint is unreachable or returns a
// non-2xx status, introspect returns (false, err). The wrapping Verifier.Verify
// propagates that error, failing the whole token validation. That forces the
// client to retry (fail-closed at the verifier layer) instead of letting an
// introspect outage either deny every token (operational meltdown) or leak
// revoked tokens (security gap). IntrospectURL == "" short-circuits to
// (false, nil) for dev deployments that have not wired it yet.
func (c *MQRevocationChecker) introspect(ctx context.Context, claims *Claims) (bool, error) {
	if c.cfg.IntrospectURL == "" {
		return false, nil
	}
	if claims.Raw == "" {
		c.cfg.Logger.Warn("mqrevocation: raw JWT not populated on Claims; cannot introspect")
		return false, nil
	}

	form := url.Values{}
	form.Set("token", claims.Raw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.IntrospectURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, fmt.Errorf("mqrevocation: introspect build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.cfg.ReplayAuthHeader != "" {
		req.Header.Set("Authorization", c.cfg.ReplayAuthHeader)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("mqrevocation: introspect request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("mqrevocation: introspect status %d", resp.StatusCode)
	}

	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("mqrevocation: introspect decode: %w", err)
	}
	// active=true means NOT revoked; active=false means revoked.
	return !body.Active, nil
}

// StartConsumer subscribes to the MQ topic via the caller-supplied adapter
// and runs the disconnect timer until ctx is canceled. Returns the error from
// subscribe.
//
// Context is the sole shutdown signal: no internal stop channel exists.
// StartConsumer honors ctx in two ways:
//
//  1. The strict-flip ticker goroutine exits when ctx is canceled.
//  2. The subscribe closure receives the same ctx; callers must wire the
//     underlying MQ subscription to that ctx so that message delivery stops
//     when ctx is canceled. StartConsumer waits for subscribe to return before
//     returning itself.
//
// The subscribe closure is the caller's bridge to shared/mq.Consumer.Subscribe
// and is expected to block until its context is canceled.
func (c *MQRevocationChecker) StartConsumer(
	ctx context.Context,
	subscribe func(ctx context.Context, handler func(ctx context.Context, payload []byte) error) error,
) error {
	c.lastEvent.Store(time.Now().Unix())

	tickerCtx, stopTicker := context.WithCancel(ctx)
	defer stopTicker()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := c.cfg.DisconnectTimeout / 3
		if interval <= 0 {
			interval = c.cfg.DisconnectTimeout
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-tickerCtx.Done():
				return
			case <-ticker.C:
				last := time.Unix(c.lastEvent.Load(), 0)
				if time.Since(last) > c.cfg.DisconnectTimeout && !c.strict.Load() {
					if c.strict.CompareAndSwap(false, true) {
						c.cfg.Logger.Warn("mqrevocation: entering strict mode",
							slog.String("reason", "no messages received"),
							slog.Duration("silence", time.Since(last)),
						)
					}
				}
			}
		}
	}()

	err := subscribe(ctx, c.HandleMessage)
	stopTicker()
	wg.Wait()
	return err
}

// replayResponse matches the /api/admin/revocations reply shape.
// The CP handler emits the cursor as "lastId" (camelCase) to match the rest of
// the admin API style.
type replayResponse struct {
	Events []minEventShape `json:"events"`
	LastID int64           `json:"lastId"`
}

// catchupLimit is the page size used by RunCatchup and the cap on pagination
// iterations. A single page of 1000 events covers normal operation; the loop
// cap prevents pathological infinite loops when the endpoint keeps returning
// full pages.
const (
	catchupPageLimit = 1000
	catchupMaxPages  = 50
)

// RunCatchup fetches missed events since the most recent checkpoint via
// ReplayURL and applies them in order. It loops until the response returns
// fewer than catchupPageLimit events (i.e. fully caught up) or catchupMaxPages
// iterations, whichever comes first. A missing ReplayURL short-circuits to nil
// for dev deployments.
//
// Decode happens outside the write lock; the lock is taken only around the
// apply loop so writers are not blocked by HTTP round-trips.
func (c *MQRevocationChecker) RunCatchup(ctx context.Context) error {
	if c.cfg.ReplayURL == "" {
		return nil
	}

	baseURL, err := url.Parse(c.cfg.ReplayURL)
	if err != nil {
		return fmt.Errorf("mqrevocation: parse replay url: %w", err)
	}

	for range catchupMaxPages {
		u := *baseURL
		q := u.Query()
		q.Set("since", strconv.FormatInt(c.lastID.Load(), 10))
		q.Set("limit", strconv.Itoa(catchupPageLimit))
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return fmt.Errorf("mqrevocation: catchup build request: %w", err)
		}
		if c.cfg.ReplayAuthHeader != "" {
			req.Header.Set("Authorization", c.cfg.ReplayAuthHeader)
		}

		resp, err := c.cfg.HTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("mqrevocation: catchup request: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return fmt.Errorf("mqrevocation: catchup status %d", resp.StatusCode)
		}

		// Decode outside the lock so HTTP latency does not block writers.
		var body replayResponse
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if decErr != nil {
			return fmt.Errorf("mqrevocation: catchup decode: %w", decErr)
		}

		// Apply batch under the write lock.
		c.mu.Lock()
		for _, ev := range body.Events {
			c.applyLocked(ev)
		}
		// Advance lastID only forward to prevent a stale response from a racing
		// second call from regressing the checkpoint.
		if body.LastID > c.lastID.Load() {
			c.lastID.Store(body.LastID)
		}
		c.mu.Unlock()

		c.lastEvent.Store(time.Now().Unix())
		if len(body.Events) > 0 {
			if c.strict.CompareAndSwap(true, false) {
				c.cfg.Logger.Info("mqrevocation: leaving strict mode",
					slog.String("reason", "catchup applied events"),
				)
			}
		}

		// Fewer events than the page size means we have reached the tail.
		if len(body.Events) < catchupPageLimit {
			return nil
		}
	}

	// Hit the iteration cap - log a warning and let the next scheduled catchup
	// tick pick up the remainder.
	if c.cfg.Logger != nil {
		c.cfg.Logger.Warn("mqrevocation: catchup hit page cap",
			slog.Int("cap", catchupMaxPages),
			slog.String("hint", "remainder will be fetched on next catchup tick"),
		)
	}
	return nil
}
