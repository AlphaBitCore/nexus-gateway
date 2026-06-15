package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// AttestationOutcome is the closed enum of verification results. The
// label set is what `nexus_attestation_verify_total{outcome=...}` carries
// — operators alert on a sustained non-valid, non-missing outcome rate
// (a steady invalid/replayed share indicates a bad agent rollout or a
// forgery attempt; "missing" is just non-agent traffic and stays out of
// the alert).
type AttestationOutcome string

const (
	AttestationOutcomeValid        AttestationOutcome = "valid"
	AttestationOutcomeMissing      AttestationOutcome = "missing"
	AttestationOutcomeInvalidSig   AttestationOutcome = "invalid_sig"
	AttestationOutcomeExpired      AttestationOutcome = "expired"
	AttestationOutcomeReplayed     AttestationOutcome = "replayed"
	AttestationOutcomeUnknownAgent AttestationOutcome = "unknown_agent"
	AttestationOutcomeDisabled     AttestationOutcome = "disabled"
)

// attestationDefaultWindow is the ±5-minute slack the verifier allows
// between agent and CP wall-clock per architecture § 2 (Time skew row).
// Tests substitute a different value via NewAttestationVerifierWith.
const attestationDefaultWindow = 5 * time.Minute

// AttestationVerifier wraps the cache + replay LRU and produces the
// outcome label for a single CONNECT header. One verifier is shared
// across the whole compliance-proxy process; it is safe for concurrent
// use (the inner cache + LRU each carry their own mutex).
//
// The verifier owns NO traffic state — every Verify call is independent.
// Callers translate the returned outcome to "transparent tunnel" (valid)
// vs "fall through to MITM" (everything else) per the fail-open
// architecture contract.
type AttestationVerifier struct {
	keys    *tlsbump.AttestationKeyCache
	replay  *tlsbump.AttestationReplayCache
	window  time.Duration
	enabled atomic.Bool
	logger  *slog.Logger
	now     func() time.Time // injected for deterministic tests
}

// NewAttestationVerifier wires a verifier with production defaults:
// 5-minute ts window, the supplied key cache + replay cache. Enabled
// state mirrors the ComplianceConfig.AttestationEnabled toggle so
// runtime config changes flip it without a process restart.
func NewAttestationVerifier(
	keys *tlsbump.AttestationKeyCache,
	replay *tlsbump.AttestationReplayCache,
	enabled bool,
	logger *slog.Logger,
) *AttestationVerifier {
	return NewAttestationVerifierWith(keys, replay, attestationDefaultWindow, enabled, logger)
}

// NewAttestationVerifierWith is the test seam letting unit tests pin
// the ts window without sleeping for 5 minutes.
func NewAttestationVerifierWith(
	keys *tlsbump.AttestationKeyCache,
	replay *tlsbump.AttestationReplayCache,
	window time.Duration,
	enabled bool,
	logger *slog.Logger,
) *AttestationVerifier {
	v := &AttestationVerifier{
		keys:   keys,
		replay: replay,
		window: window,
		logger: logger,
		now:    time.Now,
	}
	v.enabled.Store(enabled)
	return v
}

// SetEnabled flips the runtime toggle. Wired to the
// ComplianceConfig.AttestationEnabled value at startup and re-applied
// on every config reload tick.
func (v *AttestationVerifier) SetEnabled(b bool) {
	if v == nil {
		return
	}
	v.enabled.Store(b)
}

// Enabled reports the current toggle state. Exposed for diagnostics
// and to gate the ServeHTTP fast path (skip the header peek entirely
// when the feature is off).
func (v *AttestationVerifier) Enabled() bool {
	if v == nil {
		return false
	}
	return v.enabled.Load()
}

// AttestationResult is the verifier's typed return value. agentID is
// only populated when Outcome == Valid; callers MUST treat it as
// unset on any other outcome (defence in depth — the architecture
// contract is "valid → tunnel, anything else → MITM", but stamping
// agentID on an invalid row would corrupt the audit trail).
type AttestationResult struct {
	Outcome AttestationOutcome
	AgentID string
	// Reason is a short human-readable string suitable for structured
	// log enrichment. Stable enough to be filtered on but free-form;
	// the wire/metric label is Outcome.
	Reason string
}

// Verify peels the parsed header, checks the ts window, looks the
// agent up in the key cache, runs the Ed25519 verify, and records the
// nonce in the replay LRU. The four "soft" failure paths
// (invalid_sig / expired / replayed / unknown_agent) all return without
// an error — callers branch on Outcome. Missing/empty header returns
// Outcome=missing with no metric noise so the operator's "attestation
// coverage" dashboard (valid / (valid+missing)) is correct.
//
// metricsObserve is invoked once per call with the outcome label so
// the Prometheus counter increments exactly once per CONNECT.
func (v *AttestationVerifier) Verify(ctx context.Context, header string) AttestationResult {
	if v == nil || !v.Enabled() {
		v.observe(AttestationOutcomeDisabled)
		return AttestationResult{Outcome: AttestationOutcomeDisabled}
	}
	if header == "" {
		v.observe(AttestationOutcomeMissing)
		return AttestationResult{Outcome: AttestationOutcomeMissing}
	}

	fields, err := tlsbump.ParseAttestationHeader(header)
	if err != nil {
		v.warn("attestation header malformed", "error", err)
		v.observe(AttestationOutcomeInvalidSig)
		return AttestationResult{Outcome: AttestationOutcomeInvalidSig, Reason: err.Error()}
	}

	// 1. Replay-window timestamp check.
	now := v.now()
	delta := now.Sub(time.Unix(fields.TS, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > v.window {
		v.warn("attestation ts outside replay window",
			"agent_id", fields.AgentID, "ts", fields.TS, "delta", delta.String())
		v.observe(AttestationOutcomeExpired)
		return AttestationResult{Outcome: AttestationOutcomeExpired, AgentID: fields.AgentID}
	}

	// 2. Agent identity lookup (positive + negative cache).
	ak, err := v.keys.Get(ctx, fields.AgentID)
	if err != nil {
		if errors.Is(err, tlsbump.ErrUnknownAgent) {
			v.warn("attestation agent_id unknown to Hub",
				"agent_id", fields.AgentID)
			v.observe(AttestationOutcomeUnknownAgent)
			return AttestationResult{Outcome: AttestationOutcomeUnknownAgent, AgentID: fields.AgentID}
		}
		// Loader transient error — treat as unknown_agent so the
		// fail-open path engages. We still log the underlying error
		// for ops, distinguished by the "loader error" reason field.
		v.warn("attestation key load failed",
			"agent_id", fields.AgentID, "error", err)
		v.observe(AttestationOutcomeUnknownAgent)
		return AttestationResult{
			Outcome: AttestationOutcomeUnknownAgent,
			AgentID: fields.AgentID,
			Reason:  "loader error: " + err.Error(),
		}
	}
	pub := ak.Key
	if len(pub) != ed25519.PublicKeySize {
		v.warn("attestation key size invalid",
			"agent_id", fields.AgentID, "got_bytes", len(pub))
		v.observe(AttestationOutcomeInvalidSig)
		return AttestationResult{Outcome: AttestationOutcomeInvalidSig, AgentID: fields.AgentID}
	}

	// 2b. Attestation cert expiry. A key whose 90-day
	// attestation cert has lapsed must stop being trusted — otherwise a
	// compromised or exfiltrated key would bypass compliance inspection forever.
	// expired maps to MITM fallback at the caller, exactly like unknown_agent.
	// A zero CertExpiresAt (legacy stamp with no expiry on record) is treated as
	// non-expiring to stay fail-open.
	if !ak.CertExpiresAt.IsZero() && now.After(ak.CertExpiresAt) {
		v.warn("attestation cert expired",
			"agent_id", fields.AgentID, "cert_expires_at", ak.CertExpiresAt.Format(time.RFC3339))
		v.observe(AttestationOutcomeExpired)
		return AttestationResult{Outcome: AttestationOutcomeExpired, AgentID: fields.AgentID, Reason: "attestation cert expired"}
	}

	// 3. Ed25519 signature verify over the canonical pre-image.
	sigBytes, err := base64.RawURLEncoding.DecodeString(fields.Signature)
	if err != nil {
		v.observe(AttestationOutcomeInvalidSig)
		return AttestationResult{Outcome: AttestationOutcomeInvalidSig, AgentID: fields.AgentID,
			Reason: "sig base64: " + err.Error()}
	}
	if !ed25519.Verify(pub, fields.SignatureInput(), sigBytes) {
		v.warn("attestation signature failed verify", "agent_id", fields.AgentID)
		v.observe(AttestationOutcomeInvalidSig)
		return AttestationResult{Outcome: AttestationOutcomeInvalidSig, AgentID: fields.AgentID}
	}

	// 4. Replay-LRU check — runs AFTER signature verify so an attacker
	// can't pollute the LRU with forged (ts, nonce) pairs.
	if v.replay.Seen(fields.TS, fields.Nonce) {
		v.warn("attestation replay detected",
			"agent_id", fields.AgentID, "ts", fields.TS, "nonce", fields.Nonce)
		v.observe(AttestationOutcomeReplayed)
		return AttestationResult{Outcome: AttestationOutcomeReplayed, AgentID: fields.AgentID}
	}

	v.observe(AttestationOutcomeValid)
	return AttestationResult{Outcome: AttestationOutcomeValid, AgentID: fields.AgentID}
}

// observe is the metric-stamp wrapper. Threaded through every
// outcome path so a future "include latency" extension has one
// place to do it.
func (v *AttestationVerifier) observe(outcome AttestationOutcome) {
	if metrics.AttestationVerifyTotal != nil {
		metrics.AttestationVerifyTotal.With(string(outcome)).Inc()
	}
}

func (v *AttestationVerifier) warn(msg string, kv ...any) {
	if v == nil || v.logger == nil {
		return
	}
	v.logger.Warn(msg, kv...)
}
