// Package responseprehook builds the canonical SSE pre-hook callback
// used by every ingress service (agent / compliance-proxy / ai-gateway)
// to stamp a Registry-normalized payload onto the hook executor's
// HookInput BEFORE the compliance pipeline sees it.
//
// Pre-#90 each SSE-pipeline branch built its compliance HookInput with
// `core.PayloadFromTextSegments` — a flat-text fallback whose
// Normalized.Kind defaulted to "text" and dropped every adapter-specific
// signal (model name, tool_calls, reasoning segments). That broke any
// hook whose match rules referenced rich Normalized fields.
//
// #93 unifies the three ingress services on a single callback shape so
// the contract is identical everywhere. Service-specific concerns (e.g.
// stamping auditInfo.ResponseNormalized alongside ci.Normalized) ride
// through the OnPayload option, never inlined into the helper itself.
package responseprehook

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Options configures a PreHookCallback built by Build.
//
// The zero value of Options is NOT usable — Registry must be set,
// otherwise Build returns nil. All other fields are best-effort
// defaults (Direction → DirectionResponse, ContentType derived as
// text/event-stream by the SSE caller).
type Options struct {
	// Ctx is closed over by the returned callback so the underlying
	// Registry.Normalize call honours request cancellation. SSE
	// pipelines pass the per-request ctx; never use a long-lived
	// background context here.
	Ctx context.Context

	// Registry is the Tier 1+2+3 normalize chain. nil → Build returns
	// nil callback (caller keeps flat-text fallback).
	Registry *normcore.Registry

	// AdapterID is the upstream-resolved adapter identifier (e.g.
	// "anthropic-messages"). Lower-cased internally to match the
	// Registry's lower-cased Tier 1 keys.
	AdapterID string

	// EndpointPath is the request path (e.g. "/v1/messages"); fed
	// through to Tier 1 specs that route by path prefix.
	EndpointPath string

	// ContentType is the response Content-Type header value. Parameters
	// (charset, boundary, …) are stripped via StripContentTypeParams
	// before lookup — Registry routes by bare media type only.
	ContentType string

	// Direction is the Tier 1/2 routing direction. Defaulted to
	// DirectionResponse since this helper is response-only (SSE
	// pipelines only ever stream responses).
	Direction normcore.Direction

	// OnPayload, when non-nil, runs after ci.Normalized has been
	// stamped with the successful Registry payload. Service-specific
	// side-effects ride here — tlsbump uses it to stamp
	// auditInfo.ResponseNormalized; ai-gateway leaves it nil.
	//
	// payload is the same pointer that was assigned to ci.Normalized
	// — mutating it after the fact affects the hook executor's view.
	// rawBody is the cumulative SSE wire bytes passed in to the
	// callback.
	OnPayload func(payload *normcore.NormalizedPayload, rawBody []byte)

	// Logger receives the single WARN line emitted when the inner
	// Registry.Normalize call or the OnPayload callback panics. Build
	// wraps both in a recover() so the SSE pipeline never crashes on a
	// pre-hook bug (#97). nil → slog.Default() — never silent.
	Logger *slog.Logger
}

// Build returns a hookcore.PreHookCallback wired against opts. The
// callback is safe to install on either BufferPipeline.WithPreHook
// (fires once between Phase 1 and Phase 2) or LivePipeline.WithPreHook
// (fires at every checkpoint with cumulative bytes).
//
// Returns nil when opts.Registry is nil — callers should treat that as
// "no normalize layer wired; compliance pipeline keeps the flat-text
// PayloadFromTextSegments fallback".
//
// Best-effort contract: nil/empty rawBody / nil ci / Normalize hard
// error are silently dropped — never abort hook execution because the
// pre-hook stumbled (Registry already debug-logs per-tier traces via
// #87).
//
// Panic-safety (#97): both the inner Registry.Normalize call and the
// OnPayload callback are wrapped in recover(). A panicking
// Tier 1/2/3 normalizer or audit-stamp closure must NEVER take down
// the SSE pipeline — losing one stream's normalized payload is
// recoverable; losing the entire connection is not.
func Build(opts Options) hookcore.PreHookCallback {
	if opts.Registry == nil {
		return nil
	}
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	adapterID := strings.ToLower(opts.AdapterID)
	bareCT := normcore.StripContentTypeParams(opts.ContentType)
	if bareCT == "" {
		bareCT = "text/event-stream"
	}
	direction := opts.Direction
	if direction == "" {
		direction = normcore.DirectionResponse
	}
	stream := direction == normcore.DirectionResponse && strings.HasPrefix(bareCT, "text/event-stream")
	endpointPath := opts.EndpointPath
	reg := opts.Registry
	onPayload := opts.OnPayload
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(rawBody []byte, ci *hookcore.HookInput) {
		if len(rawBody) == 0 || ci == nil {
			return
		}
		payload, err := normalizeWithRecover(ctx, reg, rawBody, normcore.Meta{
			AdapterType:  adapterID,
			ContentType:  bareCT,
			Direction:    direction,
			EndpointPath: endpointPath,
			Stream:       stream,
		}, logger)
		if err != nil {
			// #115/S5 — non-panic normalize failures (ErrUnsupported,
			// tier hard errors) used to drop silently here; the hook
			// executor then operated on the flat-text fallback that
			// buildCheckpointInput stamps, so admin-configured Modify
			// hooks ran on degraded input with no signal.
			//
			// Disjoint counter semantics: panic recovery already
			// records nexus_normalize_panic_total inside
			// normalizeWithRecover and returns errPanicked as the
			// sentinel error. We skip the drop counter on that branch
			// so panic ≠ drop ≠ unsupported — admins can sum them for
			// total failures without double-counting.
			if !errors.Is(err, errPanicked) {
				recordNormalizeDrop(adapterID)
				logger.Warn("responseprehook: Registry.Normalize returned error — pre-hook drop, hook will see flat-text fallback",
					"adapter", adapterID,
					"contentType", bareCT,
					"bodySize", len(rawBody),
					"error", err,
				)
			}
			return
		}
		ci.Normalized = &payload
		if onPayload != nil {
			invokeOnPayloadWithRecover(onPayload, &payload, rawBody, logger)
		}
	}
}

// normalizeWithRecover wraps reg.Normalize so a panic in any Tier 1/2/3
// normalizer is caught and logged. Returns the same (payload, err)
// shape as Registry.Normalize; on panic returns an empty payload + the
// recovered value coerced to an error stand-in (returned as a normal
// error so the caller's existing "err != nil → silent drop" branch
// triggers). #97 panic-safety.
func normalizeWithRecover(
	ctx context.Context,
	reg *normcore.Registry,
	body []byte,
	meta normcore.Meta,
	logger *slog.Logger,
) (out normcore.NormalizedPayload, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			recordPanic("registry")
			logger.Warn("responseprehook: Registry.Normalize panicked — pre-hook drop, SSE pipeline continues",
				"adapter", meta.AdapterType,
				"direction", meta.Direction,
				"contentType", meta.ContentType,
				"bodySize", len(body),
				"panic", r,
			)
			out = normcore.NormalizedPayload{}
			retErr = errPanicked
		}
	}()
	return reg.Normalize(ctx, body, meta)
}

// invokeOnPayloadWithRecover guards the caller-supplied OnPayload so
// the SSE pipeline survives buggy audit-stamp closures. #97
// panic-safety.
func invokeOnPayloadWithRecover(
	fn func(*normcore.NormalizedPayload, []byte),
	payload *normcore.NormalizedPayload,
	rawBody []byte,
	logger *slog.Logger,
) {
	defer func() {
		if r := recover(); r != nil {
			recordPanic("on_payload")
			logger.Warn("responseprehook: OnPayload callback panicked — pre-hook drop, SSE pipeline continues",
				"bodySize", len(rawBody),
				"panic", r,
			)
		}
	}()
	fn(payload, rawBody)
}

// errPanicked is the sentinel returned by normalizeWithRecover when
// the inner Registry.Normalize panicked. Used by Build's callback to
// distinguish the panic path (already counted by
// nexus_normalize_panic_total in the recover defer) from a non-panic
// normalize error path (counted by nexus_prehook_normalize_drop_total).
// errors.Is(err, errPanicked) is the disjoint-counter discriminator.
//
// PR #24 / S6: changed from a string-typed panicError sentinel to a
// stdlib errors.New value. Future callers that branch on this error
// (e.g. errors.Is in the disjoint-counter check) get the standard
// stdlib sentinel comparison semantics without a custom type.
var errPanicked = errors.New("responseprehook: Registry.Normalize panicked")
