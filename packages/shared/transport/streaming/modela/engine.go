// Package modela implements the substrate-agnostic streaming compliance engine
// shared by every ingress that relays an SSE response under a redact-scope
// chunked_async policy (the AI Gateway canonical relay and the tlsbump transparent
// proxy used by the agent and compliance-proxy).
//
// The engine is the prescan-gated REAL-TIME streaming algorithm: it forwards the
// response to the client in real time EXCEPT a trailing bounded tail held
// undelivered, runs a cheap union prescan once a batch of new content accumulates
// (and ALWAYS before releasing any held unit and at EOF, so batching never delivers
// unscanned content), pays for one full compliance confirm only on a prescan hit,
// and on a confirmed redact/block hit ESCALATES to buffer-to-end redaction so the
// held tail (and the remainder) are delivered redacted, never raw.
//
// The bounded tail is the load-bearing safety guarantee: a sensitive value no longer
// than MaxPatternBytes is still HELD AND re-scanned when its pattern-completing bytes
// arrive, so the prescan hit + confirm fire while the value is undelivered and the
// escalation redacts it — the complete value is never delivered. The firm guarantee is
// scoped to MaxPatternBytes (the longest enforceable pattern), NOT the whole window:
// because the prescan is batched (see Config.PrescanBatchBytes), a unit is re-scanned
// for completion only up to MaxPatternBytes past its end (the flush-before-deliver
// lookahead), and a unit is evicted once the held window fills regardless. Disclosed
// best-effort surfaces (strong compliance for these is the buffer or block modes, not
// Model A): (1) a value LARGER than the tail window leaks a bounded fragment before its
// completion is observed. (2) a value in (MaxPatternBytes, TailWindowBytes] that
// straddles a unit boundary where the leading unit's size approaches the window — its
// completion may not be held-AND-scanned before the leading unit is evicted. Soundness
// for sub-MaxPatternBytes values requires TailWindowBytes > MaxPatternBytes +
// PrescanBatchBytes + maxUnitSize; for substrates whose units approach the window (the
// tlsbump large-frame path) the operator MUST size TailWindowBytes (and MaxPatternBytes,
// to cover the longest contiguous enforceable pattern — e.g. JWT/PEM) accordingly. (3) a
// memory-cap (MaxBufferBytes) eviction that would otherwise flush an incomplete content
// unit raw instead ESCALATES (see Run), so memory pressure never silently overrides the
// in-window redaction guarantee.
//
// Everything substrate-specific — what a "unit" is, how its redactable text is
// extracted, how a unit is delivered to the client, how the confirmed remainder is
// redacted, and whether an unproducible redaction fails OPEN (tlsbump host-packet
// path) or CLOSED (the gateway appliance) — lives behind the Substrate interface.
// The engine carries no transport, no canonical/wire knowledge, and no fail
// posture: it only decides WHEN to hold, confirm, release, and escalate.
package modela

import (
	"context"
	"errors"
	"io"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

const (
	// defaultTailWindowBytes bounds the trailing redactable content held
	// undelivered. A sensitive value shorter than this window is fully retained
	// when its completing bytes arrive, so the prescan hit + escalation redact it
	// before delivery; longer values carry the disclosed bounded-fragment risk.
	defaultTailWindowBytes = 8 * 1024
	// defaultMaxBufferBytes caps total held bytes so a long non-content (e.g.
	// reasoning) phase that holds many units cannot grow memory without bound. A
	// substrate's own Escalate drain cap SHOULD agree with this so the main-loop
	// ceiling and the buffer-drain ceiling do not diverge.
	defaultMaxBufferBytes = 8 * 1024 * 1024
	// defaultPrescanBatchBytes is the byte threshold of unscanned redactable content
	// that must accumulate before the engine triggers a (cgo) prescan. Deferring the
	// prescan to a batch collapses the per-unit scan count ~10-25x on small-unit
	// streams (each unit's redactable delta is tens of bytes) — the prescan is already
	// O(window) bounded, so the cost was the per-unit FREQUENCY, not the per-scan size.
	// 1KB is the cost/latency knee: smaller wastes scans; larger only marginally cuts
	// the scan count while widening detection lag. Soundness never depends on the
	// trigger firing — see Run's flush-before-deliver + EOF flush.
	defaultPrescanBatchBytes = 1024
	// defaultMaxPatternBytes bounds the longest enforceable pattern the flush-before-
	// deliver guard assumes. A unit is not released until the prescan has covered it PLUS
	// this lookahead, so a sub-window value STARTING in the released unit and completing in
	// a later still-held unit is caught even when batching deferred the scan (the unit was
	// prescanned ALONE before its completing successor arrived). 4KB covers realistic PII /
	// token patterns (cards, SSNs, emails, API keys, JWTs); a rule set with longer CONTIGUOUS
	// patterns must raise Config.MaxPatternBytes. The guard is steady-state silent while
	// TailWindowBytes > MaxPatternBytes + PrescanBatchBytes (the "comfortably larger than the
	// longest pattern" assumption the TailWindowBytes doc already states).
	defaultMaxPatternBytes = 4 * 1024
)

// DefaultMaxPatternBytes is the package default flush-before-deliver lookahead, exported
// so a substrate that DERIVES MaxPatternBytes from its rule set can floor the derived
// bound at this proven-safe baseline (never shrinking below the default even if the
// derivation under-counts). See Config.MaxPatternBytes.
const DefaultMaxPatternBytes = defaultMaxPatternBytes

// Config tunes the tail window and the held-bytes ceiling. Zero fields take the
// package defaults.
type Config struct {
	// TailWindowBytes is the trailing redactable-content budget held undelivered.
	// It MUST be set comfortably larger than the longest enforceable pattern so a
	// value straddling unit boundaries is always fully inside the re-scanned window
	// when its completing bytes arrive.
	TailWindowBytes int
	// MaxBufferBytes caps total held bytes (across all channels, content or not).
	MaxBufferBytes int
	// PrescanBatchBytes defers the cheap union prescan until this many NEW redactable
	// bytes have accumulated since the last scan, collapsing the per-unit cgo scan count
	// on small-unit streams. It is a perf knob: a confirmed hit is caught at the next
	// batch trigger, the flush-before-deliver guard (gated by MaxPatternBytes), or the EOF
	// flush — never delivering unscanned content. Zero/-ve takes the package default; set 1
	// for per-unit scanning.
	PrescanBatchBytes int
	// MaxPatternBytes is the longest CONTIGUOUS enforceable pattern (value) the rules can
	// match. The flush-before-deliver guard ensures the prescan has covered each released
	// unit PLUS this lookahead, so a sub-window value that STARTS inside the released unit
	// and completes in a later still-held unit is detected before the start unit is
	// delivered raw — the boundary case batching would otherwise leak when a large unit is
	// prescanned alone and released before its completing successor is scanned with it. MUST
	// be ≥ the longest contiguous enforceable pattern; over-setting only costs extra flush
	// scans on large-unit streams. Zero/-ve takes the package default; clamped below
	// TailWindowBytes. Perf note: a window below ~MaxPatternBytes+PrescanBatchBytes
	// degrades to a scan on (nearly) every release — safe, but loses the batching win; keep
	// TailWindowBytes comfortably above MaxPatternBytes+PrescanBatchBytes+maxUnitSize.
	MaxPatternBytes int
}

func (c Config) withDefaults() Config {
	if c.TailWindowBytes <= 0 {
		c.TailWindowBytes = defaultTailWindowBytes
	}
	if c.MaxBufferBytes <= 0 {
		c.MaxBufferBytes = defaultMaxBufferBytes
	}
	if c.PrescanBatchBytes <= 0 {
		c.PrescanBatchBytes = defaultPrescanBatchBytes
	}
	if c.MaxPatternBytes <= 0 {
		c.MaxPatternBytes = defaultMaxPatternBytes
	}
	// Clamp below the window: the lookahead can never need to reach past the held window
	// (a value wider than the window is the disclosed best-effort surface, not a guarantee).
	if c.MaxPatternBytes >= c.TailWindowBytes {
		c.MaxPatternBytes = c.TailWindowBytes - 1
	}
	return c
}

// Substrate adapts a concrete streaming transport to the engine. U is the
// substrate's unit type — a canonical provider chunk for the gateway relay, a raw
// SSE frame for the tlsbump wire path. The engine never inspects U; it only routes
// units through these operations.
//
// Method contract:
//   - Next yields the next unit. io.EOF ends the stream normally; any other error
//     is terminal — the engine calls OnError and stops. A substrate that records
//     usage / finish-reason / per-unit metadata does so inside Next as it produces
//     each unit (the engine does not surface units back to the substrate except via
//     Deliver and Escalate).
//   - AppendRedactableText appends THIS unit's prescan/confirm bytes onto dst and
//     returns the grown slice: the assistant content plus tool-call arguments /
//     name / id, with a field separator between channels so a pattern cannot span
//     two unrelated fields (over-matching the prefilter only wastes a confirm; it
//     never misses a match). Reasoning is omitted to match the canonical
//     redaction's coverage. Appending onto the engine's buffer (rather than
//     returning a fresh []byte) keeps the hot miss path allocation-free.
//   - UnitBytes returns the unit's total transport size, used for the MaxBufferBytes
//     held-bytes ceiling.
//   - ContentBytes returns the unit's redactable-content size WITHOUT field
//     separators. It MUST measure the SAME content AppendRedactableText emits (a
//     non-empty append iff ContentBytes>0; never over-report) — the engine uses it
//     symmetrically to admit and evict the unit from the tail window so reasoning /
//     non-content units do not evict redactable content from the window early, and
//     an over-report would evict the tail prematurely.
//   - IsDone reports whether this unit is the stream terminator.
//   - Deliver forwards one held/released unit's BODY to the client in real time. It
//     MUST emit only the unit body — never the stream terminator framing, even when
//     IsDone(u) is true; the terminal frame is DeliverTerminal's job. A delivery
//     error is terminal; the substrate records its own terminal classification and
//     the engine stops, returning the error.
//   - DeliverTerminal emits the stream's terminal framing (finish reason, usage, and
//     any wire sentinel) exactly once after the last unit is delivered.
//   - Prescan is the cheap union prefilter over accumulated redactable content. It
//     MUST be sound: return false only when no bound hook can possibly match the
//     bytes. A substrate whose prefilter is unavailable returns true ("always
//     confirm") so the gate never silently skips enforcement.
//   - Confirm runs ONE full compliance evaluation over the accumulated content and
//     returns the aggregate result. nil is treated as approve (resume streaming); a
//     substrate that CANNOT evaluate (build/exec error) MUST return a RejectHard
//     result, never nil, so a missing enforcer never streams unredacted.
//   - Escalate takes over on a confirmed redact/block hit (res carries the
//     triggering decision) OR a memory-pressure eviction of an incomplete content
//     unit (res is nil). It owns draining the remainder, RE-EVALUATING the buffered
//     remainder on its own waist (it does NOT trust res's spans — res is advisory /
//     audit context only), delivering ONLY the redacted remainder (the
//     already-delivered prefix is never re-delivered; the held tail is delivered
//     redacted, never raw), and the terminal framing. Its fail posture — fail-open
//     relay-original vs fail-closed error-frame — is the substrate's, not the
//     engine's. After Escalate returns the engine stops.
//   - OnConfirmApproved stamps the audit outcome of a confirm that did NOT enforce (a
//     prescan false positive, or a nil/approve result) so a SIEM sees "hooks
//     evaluated" plus any tags, distinguishing it from "no response hook
//     configured". res may be nil (nil-as-approve).
//   - OnApproveEOF stamps an approve outcome when the stream reached EOF without any
//     confirm ever running — the sound prescan cleared the whole stream, so the
//     response is provably approved (vs "no hook configured").
//   - OnError emits the substrate's terminal error framing for a non-EOF Next error
//     and returns the error the engine propagates.
type Substrate[U any] interface {
	Next(ctx context.Context) (U, error)
	AppendRedactableText(dst []byte, u U) []byte
	UnitBytes(u U) int
	ContentBytes(u U) int
	IsDone(u U) bool
	Deliver(ctx context.Context, u U) error
	DeliverTerminal(ctx context.Context) error
	Prescan(content []byte) bool
	Confirm(ctx context.Context, content string) *hookcore.CompliancePipelineResult
	Escalate(ctx context.Context, held []U, res *hookcore.CompliancePipelineResult) error
	OnConfirmApproved(res *hookcore.CompliancePipelineResult)
	OnApproveEOF()
	OnError(ctx context.Context, err error) error
}

// Run drives the prescan-gated real-time streaming algorithm over sub until EOF,
// an escalation, or a terminal error. It returns nil on a clean stream, the
// substrate's OnError result on a non-EOF Next error, a Deliver error if a real-time
// write fails, or the Escalate result on a confirmed redact/block hit or a
// memory-pressure escalation.
func Run[U any](ctx context.Context, sub Substrate[U], cfg Config) error {
	cfg = cfg.withDefaults()

	var (
		held             []U    // trailing units NOT yet delivered to the client
		heldScanLens     []int  // per-held-unit scanBuf byte length (parallel to held)
		heldBytes        int    // total transport bytes currently held (MaxBufferBytes ceiling)
		contentBytes     int    // redactable-content bytes currently held (tail window)
		scanBuf          []byte // accumulated redactable content the prescan/confirm read
		deliveredScanLen int    // scanBuf offset where the oldest still-held unit begins
		scannedLen       int    // high-water scanBuf offset already prescanned (hit or miss)
		anyConfirm       bool   // whether any confirm ran (audit: approve vs no-hook)
	)

	// scanThrough advances the windowed prescan to the current end of scanBuf and, on a
	// hit, pays for ONE full confirm. It returns a non-nil result ONLY when the caller
	// must escalate (a confirmed block/redact action); a miss or a false-positive approve
	// is handled in place. It is the SINGLE prescan primitive — invoked by the batch
	// trigger AND before every release / at EOF — so the invariant "scanned-through ≥
	// delivered" holds at every release point and batching never delivers unscanned
	// content. The prescan reads the still-HELD window [deliveredScanLen:] (already
	// bounded to ~TailWindowBytes by releases, so each scan is O(window)); scannedLen
	// high-waters to len(scanBuf) so a repeated call with no new content is a no-op.
	scanThrough := func() *hookcore.CompliancePipelineResult {
		if len(scanBuf) <= scannedLen {
			return nil
		}
		scannedLen = len(scanBuf)
		if !sub.Prescan(scanBuf[deliveredScanLen:]) {
			return nil
		}
		// Confirm over the still-HELD window only ([deliveredScanLen:]), not the whole
		// accumulated buffer: Escalate re-evaluates the held remainder anyway, and a
		// pattern entirely in the already-delivered (un-redactable) prefix could only fire
		// a redundant escalate. Windowing keeps the confirm O(window).
		res := sub.Confirm(ctx, string(scanBuf[deliveredScanLen:]))
		// A confirm executed (even a nil/approve result) — the response was evaluated,
		// which the EOF stamp must distinguish from "no hook".
		anyConfirm = true
		// Gate on the enforcing ACTION, not the raw decision enum: the pipeline aggregator
		// ranks a co-firing soft-block ABOVE a redact, so a Modify (redact) carrying real
		// spans can surface as BlockSoft. ActionFromDecision maps RejectHard/BlockSoft→block
		// and Modify→redact, so keying on the action escalates every enforcing outcome
		// instead of leaking a redact masked by a soft-block.
		if res != nil {
			act := hookcore.ActionFromDecision(res.Decision)
			if act == hookcore.ActionBlock || act == hookcore.ActionRedact {
				return res
			}
		}
		// Prescan false positive / nil-as-approve: record the outcome, resume streaming.
		sub.OnConfirmApproved(res)
		return nil
	}

	for {
		u, err := sub.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return sub.OnError(ctx, err)
		}

		// Accumulate the unit's redactable content (with field separators) for the
		// prescan, then HOLD the unit. Track the per-unit scanBuf length so a release
		// can advance deliveredScanLen, and the tail-window content accounting uses
		// the separator-free ContentBytes so the scan's separators do not perturb the
		// eviction math.
		before := len(scanBuf)
		scanBuf = sub.AppendRedactableText(scanBuf, u)
		held = append(held, u)
		heldScanLens = append(heldScanLens, len(scanBuf)-before)
		heldBytes += sub.UnitBytes(u)
		contentBytes += sub.ContentBytes(u)

		// Prescan GATE (BATCHED): defer the cheap union prescan until PrescanBatchBytes of
		// NEW redactable content have accumulated since the last scan, collapsing the
		// per-unit cgo scan count on small-unit streams. Soundness does NOT rely on this
		// trigger firing: the flush-before-deliver guard in the release loop and the EOF
		// flush force a scanThrough over any held content before it is delivered, so a
		// sub-threshold tail is never delivered unscanned. A confirmed enforcing hit hands
		// off to buffer-to-end redaction (the held unit completing the match is still
		// undelivered because the scan precedes its release).
		if len(scanBuf)-scannedLen >= cfg.PrescanBatchBytes {
			if res := scanThrough(); res != nil {
				return sub.Escalate(ctx, held, res)
			}
		}

		// Release the tail. Two eviction triggers:
		//   - contentBytes > TailWindowBytes: the oldest held content is now older than
		//     the window, so any sub-window value it carries is complete — safe to
		//     deliver raw (the normal real-time path).
		//   - heldBytes > MaxBufferBytes: a memory cap. Evicting a NON-content unit
		//     (reasoning, ContentBytes==0) raw is safe (reasoning is never redacted).
		//     But evicting a CONTENT unit raw under memory pressure alone — when the
		//     window has NOT filled with content (it filled with reasoning) — could
		//     leak a still-incomplete sub-window value, so that case ESCALATES instead.
		for len(held) > 0 {
			overWindow := contentBytes > cfg.TailWindowBytes
			overBuf := heldBytes > cfg.MaxBufferBytes
			if !overWindow && !overBuf {
				break
			}
			front := held[0]
			if overBuf && !overWindow && sub.ContentBytes(front) > 0 {
				// Memory-pressure eviction of a content-bearing unit whose value may be
				// incomplete: escalate (buffer + redact the remainder) rather than leak.
				return sub.Escalate(ctx, held, nil)
			}
			// FLUSH-BEFORE-DELIVER (with a MaxPatternBytes lookahead): release front only once
			// the prescan has covered front's end PLUS MaxPatternBytes. frontScanEnd is front's
			// absolute scanBuf end offset (deliveredScanLen is where front begins, heldScanLens[0]
			// its length). The lookahead is load-bearing: a sub-window value that STARTS inside
			// front and completes in a LATER still-held unit must be scanned before front is
			// delivered raw — and batching may have scanned front ALONE (a large unit batch-
			// triggered before its completing successor arrived), so requiring only
			// scannedLen ≥ frontScanEnd would deliver front's pattern-prefix raw and then scan
			// the suffix over a window that no longer holds the prefix (a boundary leak). With
			// the lookahead, any value of length ≤ MaxPatternBytes straddling front's boundary is
			// caught. Steady-state silent: the scan leads front by ~(TailWindow-PrescanBatch) ≫
			// MaxPatternBytes, so this only fires for a large single unit (or a tiny window). A
			// flush hit escalates instead of delivering raw; scanThrough scans the full held
			// window [deliveredScanLen:].
			if frontScanEnd := deliveredScanLen + heldScanLens[0]; scannedLen < len(scanBuf) && scannedLen < frontScanEnd+cfg.MaxPatternBytes {
				if res := scanThrough(); res != nil {
					return sub.Escalate(ctx, held, res)
				}
			}
			if derr := sub.Deliver(ctx, front); derr != nil {
				return derr
			}
			heldBytes -= sub.UnitBytes(front)
			contentBytes -= sub.ContentBytes(front)
			deliveredScanLen += heldScanLens[0]
			var zero U
			held[0] = zero // release the delivered unit's payload to the GC immediately
			held = held[1:]
			heldScanLens = heldScanLens[1:]
		}

		// Compact scanBuf: drop the already-delivered prefix so memory is bounded at
		// O(window) instead of O(N) for the whole stream (the prefix grows unbounded even
		// on a clean, prescan-miss stream). Threshold-gated on `> TailWindowBytes` so the
		// move is amortized O(1)/byte — never a per-unit memmove on the hot relay path.
		// Delivered bytes are past the window and can no longer be redacted, so dropping
		// them loses nothing the windowed prescan/confirm didn't already forgo. Rebase the
		// absolute offsets by the dropped length; heldScanLens are RELATIVE (untouched).
		// scannedLen is clamped at 0: if the scanned prefix lay entirely within the dropped
		// region it becomes 0 (re-scan the held window on the next trigger — the safe
		// direction). Because we only deliver content that has been scanned (flush-before-
		// deliver), scannedLen ≥ deliveredScanLen here, so the rebase stays non-negative.
		if deliveredScanLen > cfg.TailWindowBytes {
			drop := deliveredScanLen
			n := copy(scanBuf, scanBuf[drop:])
			scanBuf = scanBuf[:n]
			deliveredScanLen = 0
			// Defensive floor: scannedLen ≥ drop in practice (we only deliver — advancing
			// deliveredScanLen — content that flush-before-deliver already scanned), so this
			// never actually clamps. Even if it did, a smaller scannedLen only forces extra
			// (sound) scans, never a leak; max keeps the rebase robust without an untested branch.
			scannedLen = max(0, scannedLen-drop)
		}

		if sub.IsDone(u) {
			break
		}
	}

	// EOF FLUSH: scan any unscanned tail (the final < PrescanBatchBytes that never hit
	// the batch trigger) BEFORE delivering the held tail raw — else a sub-threshold final
	// content unit would be delivered unscanned. A hit escalates (redact/block); only a
	// miss / approve-cleared tail is delivered raw below.
	if res := scanThrough(); res != nil {
		return sub.Escalate(ctx, held, res)
	}

	// EOF without escalation → every prescan was a miss or a false-positive approve,
	// so the held tail is provably benign. When NO confirm ever ran, the sound
	// prescan cleared the whole stream, so stamp the response as approved (vs "no
	// response hook configured"). A confirm that ran already stamped its own outcome.
	if !anyConfirm {
		sub.OnApproveEOF()
	}
	for _, u := range held {
		if derr := sub.Deliver(ctx, u); derr != nil {
			return derr
		}
	}
	return sub.DeliverTerminal(ctx)
}
