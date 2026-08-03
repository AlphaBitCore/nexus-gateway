package streaming

// Delta-text extraction for the live relay: turning one SSE frame's `data:`
// payload into the text the compliance checkpoint accumulates.
//
// Split out of live.go along that seam because it is where the wire-shape
// knowledge and the JSON-decoder safety constraints live, while live.go owns the
// relay, the checkpoint cadence and the goroutine structure. Its tests and
// benchmarks already sat under the live_extract* names.

import (
	stdjson "encoding/json"
	"strings"

	"github.com/goccy/go-json"
)

// extractDeltaText attempts to extract the delta content from an SSE event's
// data field. For OpenAI-compatible streaming responses, the data is JSON with
// choices[0].delta.content. Falls back to the raw data if parsing fails.
//
// The three outcomes are deliberately NOT collapsible, and the discriminator is
// not JSON validity — it is "does the payload unmarshal into THAT struct":
//
//	unmarshal ok, len(choices) > 0 → choices[0].delta.content
//	unmarshal ok, len(choices) == 0 → ""
//	unmarshal error                → the raw data, verbatim
//
// So `[1,2,3]` and `"a json string"` are valid JSON yet return raw verbatim
// (an array/string cannot decode into a struct), and `{"choices":{"a":1}}`
// returns raw verbatim on the type mismatch. Field matching is also
// case-INsensitive, so `{"CHOICES":[…]}` returns the content. Every one of
// those was verified empirically against the pinned goccy version, and every
// one of them is why a plain `bytes.Contains(data, "choices")` cheap-reject
// would be behaviour-changing rather than merely faster.
func extractDeltaText(evt *SSEEvent) string {
	if evt.Done {
		return ""
	}
	data := evt.Data
	if data == "" {
		return ""
	}

	// Validate BEFORE decoding. This is memory safety, it is not free, and the
	// cost is accepted deliberately — see the cost note at the end of this comment.
	//
	// goccy v0.10.6 reads past its own internal buffer on malformed escape
	// sequences. decodeKeyCharByEscapedChar has a permissive default arm that
	// accepts any unrecognized escapee — including goccy's own NUL terminator — so
	// the key scanner steps over the terminator and walks adjacent heap through an
	// unchecked pointer read.
	//
	// `{"\` produces `panic: runtime error: index out of range [13] with length 4`,
	// but only ~4% of the time: the panic form needs a backslash to appear in the
	// adjacent heap before a quote or NUL, and the other ~96% are SILENT
	// out-of-bounds reads. Under -race, where checkptr is active, it is instead
	// `fatal error: checkptr: pointer arithmetic result points to an invalid
	// allocation` — a runtime throw that recover() CANNOT catch. A transparent MITM carries arbitrary upstream wire
	// bytes (binding B9), so a frame cut mid-escape is a routine input rather than
	// a hypothetical, and this runs on the delivery goroutine where the fault
	// tears down the client connection on a compliance product.
	//
	// Three cheaper fixes were implemented and rejected, each by measurement or by
	// counterexample — all recorded in
	// docs/handoffs/perf-compliance-agent-program.md so they are not retried:
	//
	//   - recover() around the decode: does NOT work. checkptr's throw is not a
	//     panic and cannot be recovered.
	//   - a trailing-backslash guard, which is O(1) and provably touches only
	//     invalid JSON: INSUFFICIENT. The fuzzer found `{"\00\.`, which faults
	//     while ending in '.', so the malformed escape need not be at the end.
	//   - decoding with stdlib instead of goccy, which needs no pre-gate: correct
	//     but measured +337% to +545% on this function, worse than validating.
	//
	// The validator is the STANDARD LIBRARY's, and the choice is a safety
	// constraint rather than a speed one. Speed alone said otherwise on a 170-byte
	// frame — gjson.Valid 152 ns, stdjson.Valid 437 ns, goccy's own 1275 ns and 42
	// allocs — and gjson was shipped on that basis. It was wrong:
	//
	//	stdjson.Valid  bounded depth, iterative scanner       SAFE
	//	gjson.Valid    mutually recursive, NO depth limit     ~128 B of stack per
	//	               (validany -> validarray -> validany)   nesting level, so a
	//	                                                      deep payload becomes
	//	                                                      fatal: stack overflow
	//	goccy .Valid   also accepts `{}}`, which its own decoder rejects
	//
	// A stack overflow is an unrecoverable runtime throw that kills the PROCESS,
	// where the fault this gate exists to prevent only kills one connection. So the
	// faster validator made the situation strictly worse on the very axis the gate
	// is for, and on the same delivery goroutine. Reproduced end to end with ~8 MB
	// of upstream spread over 8 `data:` lines, each individually under
	// maxSSELineSize — note that cap bounds a LINE, not the number of lines an event
	// may concatenate, so the payload handed here is effectively unbounded.
	//
	// Cost of the safe choice: the frame is scanned twice, a real regression on this
	// function. Binding B1 makes correctness the precondition rather than something
	// traded against throughput.
	//
	// Honest bound on what this gate does and does not achieve: it closes the
	// malformed-escape fault for THIS call site, verified by ~975 k -race fuzz
	// executions across two independent reviews. It is not a general closure of
	// goccy's memory safety — goccy also computes an out-of-range pointer while
	// unescaping a lone surrogate (`{"\ud800":1}`), which is VALID JSON that any
	// validator must accept. That one is benign in a production build (the pointer
	// is never dereferenced) and checkptr-fatal only under -race.
	//
	// Behaviour delta, and it is wider than "a few sequences" — an earlier version
	// of this comment said that and was wrong. goccy accepts a routine class the
	// standard rejects: a raw control byte inside a string, a trailing NUL, a
	// trailing comma, a leading-zero number. A single-byte-insertion sweep over one
	// 43-byte OpenAI frame produced 69 such mutations. Every one of them now takes
	// the raw-verbatim branch where goccy previously decoded the delta.
	//
	// Two consequences, neither of them "conservative":
	//
	//   - accumulatedAll receives the whole envelope instead of the delta, so
	//     pendingLen crosses the checkpoint step far sooner: 150 tab-bearing frames
	//     went from 1 pipeline execution to 11. That changes audit rows and the
	//     folded hook aggregate, not just cost. The mechanism is not new — a
	//     non-JSON frame always took this branch — but the input class that reaches
	//     it is now wider.
	//   - the envelope carries JSON escapes where the delta carried characters, and
	//     Go's encoder escapes < > & by default while Python's escapes non-ASCII by
	//     default. So a hook matching `<script>` can match the decoded delta and
	//     miss `\u003cscript\u003e`. On the tlsbump path the pre-hook overwrites
	//     ci.Normalized from the cumulative raw bytes through the Registry
	//     (responseprehook.go), so production hooks see a proper decode and this
	//     only bites when normalize errors or is unsupported — but that mitigation
	//     is the pre-hook's, not this function's.
	//
	// The delta is still confined to input that is invalid JSON per RFC 8259.
	// The backslash test is what keeps this affordable, and it is a mechanistic
	// precondition rather than a guess. goccy's faulting path is
	// decodeKeyCharByEscapedChar, which is reached ONLY after the key scanner has
	// consumed a backslash; its permissive default arm then accepts goccy's own NUL
	// terminator as an escapee and the scan walks off the buffer. The second known
	// goccy pointer defect (unescapeString on a lone surrogate) likewise requires an
	// escape. A frame containing no backslash anywhere therefore cannot reach
	// either. Note this is an EXISTENCE test, not a position test — the earlier
	// trailing-backslash guard failed precisely because it assumed the malformed
	// escape sits at the end, and the fuzzer produced `{"\00\.` to disprove that.
	//
	// Consequence for the behaviour delta: frames whose only defect is a raw control
	// byte, a trailing comma or a leading-zero number keep their previous answer
	// entirely, because they never reach the validator. Only a frame that carries a
	// backslash AND is invalid changes, which is a far narrower class than gating
	// unconditionally.
	if strings.IndexByte(data, '\\') >= 0 && !stdjson.Valid([]byte(data)) {
		return data
	}

	// Try to parse as OpenAI streaming chunk.
	//
	// A cheap-reject fast path that skipped this decode for choices-less frames was
	// also implemented and measured as a regression. Any sound shortcut has to
	// establish validity first, and the validity scan above already costs more than
	// this decode, so the shortcut is not reachable by making the key search faster.
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err == nil {
		if len(chunk.Choices) > 0 {
			return chunk.Choices[0].Delta.Content
		}
		return ""
	}

	// Fallback: return raw data as text.
	return data
}
