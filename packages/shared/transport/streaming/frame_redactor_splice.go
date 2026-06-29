package streaming

import (
	"strings"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// WireTextCodec decodes the visible assistant text carried by a single
// provider SSE data frame on its native wire. It is implemented per-host
// (the tlsbump seam wraps the traffic.Adapter's ExtractStreamChunk) and kept
// behind this interface so the splice logic stays provider-agnostic and
// coverage-gated in this package instead of leaking per-host JSON paths into
// the redactor.
//
// ChunkText returns the text the frame contributes to the assistant transcript
// and ok=true for a text frame. It returns ok=false for every non-text frame —
// tool_use / tool_call delta / ping / role / [DONE] / reasoning-only — which
// the redactor passes BYTE-VERBATIM, and ok=false on a decode error (the frame
// is then treated as opaque and forwarded verbatim; its bytes are absent from
// the reconstructed transcript, which the ModifiedContent fence catches).
type WireTextCodec interface {
	ChunkText(data string) (text string, ok bool)
}

// SpliceTextFrames rewrites the text frames of a buffered SSE timeline so the
// masked content the pipeline decided (result.TransformSpans + the
// authoritative result.ModifiedContent) reaches the client, while every
// non-text frame is passed byte-verbatim. It is the substrate that makes
// tlsbump buffer mode actually redact a Modify decision instead of replaying
// the original unredacted stream.
//
// The redaction is performed on the per-host normalized view (the codec
// decodes each frame via the adapter's ExtractStreamChunk), so non-OpenAI
// wires (Anthropic, Gemini, …) are redacted correctly — the logic is never
// OpenAI-only.
//
// It returns (redacted, true) on a sound rewrite. It returns
// (events, false) — the FAIL-OPEN signal — whenever the masked wire cannot be
// soundly reconstructed; the caller then forwards the original timeline and
// stamps the disclosed REDACT_INFLIGHT_UNSUPPORTED reason (never a fail-closed
// error). The fail-open cases are all sound (they never emit a partially- or
// mis-redacted transcript):
//   - the redaction targets tool-call arguments (undeliverable: tool frames
//     pass byte-verbatim and their args stream as fragmented JSON deltas);
//   - a span addresses something other than a single assistant text content
//     block (multi-block or non-text address — offsets cannot be mapped onto
//     one wire transcript);
//   - a span offset falls outside the reconstructed wire transcript, or spans
//     overlap (ambiguous splice) — both signal wire/normalized divergence;
//   - the spliced transcript does not byte-equal the pipeline's authoritative
//     masked text (result.ModifiedContent) — the divergence fence;
//   - a per-frame re-encode cannot be round-tripped back through the codec.
//
// Re-entrant: all state is local to the call (no package global), so
// BufferPipeline.Process may run it concurrently for many requests.
func SpliceTextFrames(events []*SSEEvent, result *core.CompliancePipelineResult, codec WireTextCodec) ([]*SSEEvent, bool) {
	if result == nil || codec == nil {
		return events, false
	}
	spans := result.TransformSpans
	if len(spans) == 0 {
		// A Modify with no byte-level spans carries no offsets to splice
		// against — cannot reconstruct soundly.
		return events, false
	}

	// 1. Classify span addresses. Only spans that address ONE assistant text
	//    content block are spliceable; a tool-arg span or any second address
	//    is undeliverable on the streaming wire.
	addr := ""
	for i := range spans {
		a := spans[i].ContentAddress
		if strings.Contains(a, ".toolUse.input.") {
			return events, false // tool-arg mask: tool frames pass verbatim
		}
		if !isTextContentAddress(a) {
			return events, false
		}
		if addr == "" {
			addr = a
		} else if a != addr {
			return events, false // multi-block: offsets cannot map to one wire transcript
		}
	}

	// 2. Reconstruct the wire transcript and each text frame's window into it.
	type frameWindow struct {
		idx   int    // index into events
		start int    // inclusive offset in wireText
		end   int    // exclusive offset in wireText
		text  string // original delta text
	}
	var (
		windows []frameWindow
		wb      strings.Builder
	)
	for i, evt := range events {
		txt, ok := codec.ChunkText(evt.Data)
		if !ok || txt == "" {
			continue // non-text frame: forwarded verbatim, absent from transcript
		}
		start := wb.Len()
		wb.WriteString(txt)
		windows = append(windows, frameWindow{idx: i, start: start, end: wb.Len(), text: txt})
	}
	wireText := wb.String()
	if len(windows) == 0 {
		return events, false // spans present but no text frames to splice
	}

	// 3. Project spans onto wire-transcript offsets. The single addressed block
	//    IS the assistant transcript == wireText (proven by the fence in step
	//    6), so the block-relative offsets are wireText-relative. A span that
	//    falls outside the transcript signals wire/normalized divergence.
	sp := make([]spliceSpan, 0, len(spans))
	for i := range spans {
		s, e := spans[i].Start, spans[i].End
		if s < 0 || e < s || s > len(wireText) || e > len(wireText) {
			return events, false
		}
		sp = append(sp, spliceSpan{start: s, end: e, repl: spans[i].Replacement})
	}

	// 4. Reject overlapping spans — the pipeline emits disjoint spans; an
	//    overlap would make the splice ambiguous.
	sortSpliceAscending(sp)
	for i := 1; i < len(sp); i++ {
		if sp[i].start < sp[i-1].end {
			return events, false
		}
	}

	// 5. Per-frame cross-frame splice. The replacement of a span is emitted
	//    exactly once, in the frame that contains the span START; the rest of
	//    the span's bytes in later frames are deleted. Offsets stay valid
	//    because the transcript is rebuilt left-to-right from the original
	//    offsets rather than mutated in place (the equivalent of ApplySpans'
	//    descending-offset apply, without the in-place offset shift).
	masked := make(map[int]string, len(windows)) // event idx -> new delta text
	var maskedAgg strings.Builder
	for _, w := range windows {
		nt := maskFrameWindow(wireText, w.start, w.end, sp)
		masked[w.idx] = nt
		maskedAgg.WriteString(nt)
	}

	// 6. Divergence fence: the spliced transcript MUST byte-equal the
	//    pipeline's authoritative masked text. This guards against the wire
	//    transcript differing from the normalized text the spans were computed
	//    against (different codec, trimming, or a wrongly-addressed block):
	//    any such mismatch fails open rather than ship a mis-redacted stream.
	if !maskedMatchesModified(maskedAgg.String(), result.ModifiedContent) {
		return events, false
	}

	// 7. Re-encode each changed frame and verify it round-trips through the
	//    codec, so a literal rewrite that landed on the wrong JSON field (or a
	//    wire shape the codec cannot reproduce) fails open instead of leaking.
	//    Non-text and unchanged frames keep their original pointer => their
	//    serialization is byte-identical.
	out := make([]*SSEEvent, len(events))
	copy(out, events)
	for _, w := range windows {
		nt := masked[w.idx]
		if nt == w.text {
			continue // frame unchanged
		}
		newData, ok := reencodeChunkText(events[w.idx].Data, w.text, nt, codec)
		if !ok {
			return events, false
		}
		ne := *events[w.idx]
		ne.Data = newData
		out[w.idx] = &ne
	}
	return out, true
}

// spliceSpan is a wire-transcript-relative redaction range.
type spliceSpan struct {
	start int
	end   int
	repl  string
}

func sortSpliceAscending(spans []spliceSpan) {
	// insertion sort — span counts per response are tiny.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

// maskFrameWindow returns the masked delta text for one text frame whose
// content occupies wireText[fs:fe). spans must be disjoint and ascending. The
// replacement is emitted only in the frame that contains the span start so a
// span straddling several frames yields its replacement exactly once; the
// span's remaining bytes in later frames are deleted (emitted as empty).
func maskFrameWindow(wireText string, fs, fe int, spans []spliceSpan) string {
	var b strings.Builder
	cursor := fs
	for _, sp := range spans {
		if sp.end <= fs || sp.start >= fe {
			continue // span does not overlap this frame
		}
		localS := sp.start
		if localS < fs {
			localS = fs
		}
		localE := sp.end
		if localE > fe {
			localE = fe
		}
		if localS > cursor {
			b.WriteString(wireText[cursor:localS])
		}
		if sp.start >= fs && sp.start < fe {
			b.WriteString(sp.repl) // replacement lives in the start frame only
		}
		if localE > cursor {
			cursor = localE
		}
	}
	if cursor < fe {
		b.WriteString(wireText[cursor:fe])
	}
	return b.String()
}

// maskedMatchesModified reports whether the spliced transcript byte-equals the
// concatenation of the text-type blocks in the pipeline's ModifiedContent. An
// empty ModifiedContent (no authoritative masked text to verify against)
// returns false so the caller fails open rather than ship an unverified splice.
func maskedMatchesModified(agg string, modified []core.ContentBlock) bool {
	if len(modified) == 0 {
		return false
	}
	var b strings.Builder
	any := false
	for _, blk := range modified {
		if blk.Type != "" && blk.Type != "text" {
			continue
		}
		b.WriteString(blk.Text)
		any = true
	}
	if !any {
		return false
	}
	return b.String() == agg
}

// reencodeChunkText writes masked in place of orig inside a single SSE frame's
// data by replacing the JSON-encoded original text value with the JSON-encoded
// masked value, then verifies the rewrite round-trips through the codec. ok is
// false when the original value is not found (nothing rewritten) or the
// re-decoded frame does not carry exactly the masked text — either way the
// caller fails open.
func reencodeChunkText(data, orig, masked string, codec WireTextCodec) (string, bool) {
	escOrig := jsonInner(orig)
	escMasked := jsonInner(masked)
	newData := strings.Replace(data, `"`+escOrig+`"`, `"`+escMasked+`"`, 1)
	if newData == data {
		return data, false // original text value not located => cannot rewrite
	}
	txt, ok := codec.ChunkText(newData)
	if masked == "" {
		// Fully-redacted frame: the codec must no longer see any text.
		if ok && txt != "" {
			return data, false
		}
		return newData, true
	}
	if !ok || txt != masked {
		return data, false
	}
	return newData, true
}

// jsonInner returns the JSON string-encoding of s without the surrounding
// quotes, i.e. the exact bytes that appear between the quotes of a JSON string
// value carrying s. json.Marshal of a Go string never errors and always
// yields at least the two quote bytes, so the slice is always valid.
func jsonInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// isTextContentAddress reports whether addr targets an assistant text content
// block — the address grammar "messages.<i>.content.<j>" with no further
// suffix (a toolUse / toolResult leaf is not plain text).
func isTextContentAddress(addr string) bool {
	parts := strings.Split(addr, ".")
	if len(parts) != 4 || parts[0] != "messages" || parts[2] != "content" {
		return false
	}
	if _, err := normalize.ParseInt(parts[1]); err != nil {
		return false
	}
	if _, err := normalize.ParseInt(parts[3]); err != nil {
		return false
	}
	return true
}
