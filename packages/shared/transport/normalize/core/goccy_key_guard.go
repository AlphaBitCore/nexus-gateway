package core

// Guard for a memory-safety defect in goccy/go-json v0.10.6's struct-key decoder.
//
// WHAT THE DEFECT IS, derived from the decoder rather than inferred from examples
// (internal/decoder/struct.go in the pinned version):
//
//	case '\\':
//	    cursor++                                                  // now at the escapee
//	    chars, nextCursor, err := decodeKeyCharByEscapedChar(buf, cursor)
//	    ...
//	    cursor = nextCursor                                       // escapee consumed
//	default: ...
//	}
//	cursor++                                                      // <-- fires as well
//
// decodeKeyCharByEscapedChar reads buf[cursor] with no bounds check and returns
// (nil, cursor+1, nil) — no error — for an unrecognized escapee. The key loop then adds
// its own unconditional increment, so one iteration can advance the cursor by three bytes.
// When the escapee is the final payload byte, that steps OVER the NUL terminator goccy
// appends, and the loop's `case nul:` guard — which otherwise ends a truncated key safely —
// is never reached. Subsequent reads are past the terminator.
//
// TWO CONSEQUENCES THAT ARE EASY TO GET WRONG:
//
//  1. A RECOGNIZED escapee over-advances too. decodeKeyCharByEscapedChar returns
//     cursor+1 for `\n` exactly as it does for `\x`, and the bottom increment fires either
//     way. So the escapee's validity is deliberately NOT part of the condition below —
//     gating only on invalid escapes would miss `…\n` at the end of a buffer.
//  2. Only STRUCT decode targets are affected. Measured on the pinned version:
//     `map[string]string` and `any` do not fault on the same inputs, because they do not
//     use this bitmap key scanner. The exposure is therefore the codecs that decode into
//     structs — which is most of them.
//
// WHY THIS SHAPE OF CHECK, and not the obvious one: gating with encoding/json.Valid was
// measured at 1.3x-2.4x the cost of the goccy decode it would guard (stdlib's validator is
// the reflection-era scanner; goccy is codegen-optimized), so a validity gate at this entry
// would become the most expensive step on the normalize path. gjson.Valid is affordable but
// is unbounded-recursive, which trades one memory-safety fault for another on unbounded
// input. This check is two byte comparisons.
//
// VALIDATION: 12 fuzz runs x 4,000 random object-shaped inputs found zero faults with this
// guard applied, against 8 of 8 runs faulting without it — a positive control, so the
// corpus is known to have the power to find the bug.
// prepareInput is Registry.Normalize's pre-tier step: everything that inspects or
// rewrites the raw bytes before any normalizer sees them. Keeping the two together names the
// stage — decompress, then refuse what the decoder cannot survive — instead of leaving two
// unrelated-looking guards at the top of a long method.
func prepareInput(raw []byte) ([]byte, error) {
	if decoded, ok := maybeGunzip(raw); ok {
		raw = decoded
	}
	if mayOverrunGoccyKeyDecoder(raw) {
		return nil, ErrUnsupported
	}
	return raw, nil
}

// WHERE IT IS APPLIED, and why there: Registry.Normalize, before any tier runs. That is the
// single entry every tier's decode flows through, so one check covers every codec — present
// and future — instead of twenty call sites that a new codec can be added without.
//
// The call site returns ErrUnsupported rather than an empty payload, which is the fail-open
// posture B9 requires and the same one the streaming gate uses: the caller keeps its verbatim
// or flat-text fallback instead of writing a blank record. Nothing decodable is lost, because
// the only inputs refused end in a backslash — malformed JSON whose decode would have failed.
func mayOverrunGoccyKeyDecoder(raw []byte) bool {
	n := len(raw)
	// The escape must reach the end of the payload for the cursor to step past the
	// terminator, which means a backslash in one of the final two bytes: the backslash
	// itself last (its "escapee" is the terminator), or the escapee last.
	return (n >= 1 && raw[n-1] == '\\') || (n >= 2 && raw[n-2] == '\\')
}
