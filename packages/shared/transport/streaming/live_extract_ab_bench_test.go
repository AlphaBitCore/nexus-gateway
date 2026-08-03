package streaming

import "testing"

// Paired before/after benchmarks for the extractDeltaText validity gate (C-31's
// fix, whose cost lands on the C-16 path) and the accumulator key gates (C-20).
//
// The "before" arm calls the reference implementations kept in
// live_extract_equivalence_test.go / usage_gating_equivalence_test.go, so both arms
// share one oracle with the differential correctness tests. That part is sound.
//
// WHAT IS NOT SOUND, and cost this program a set of retracted numbers: `go test
// -bench X -count=N` does NOT interleave sub-benchmarks. It runs all N repetitions
// of impl=before, THEN all N of impl=after. Verified directly — with -count=3,
// samples 1-3 are before and 4-6 are after. So each arm gets a contiguous block of
// wall time and a load ramp lands entirely on one arm, which is the failure the
// pairing was meant to prevent. It made a ±2% instrument look like ±141% noise, and
// in review it made an arm doing strictly MORE work measure 54% faster.
//
// So DO NOT run these with -count=N and read the two arms against each other.
// Run one arm per process, rotating arm order every round:
//
//	for r in $(seq 1 16); do
//	  for arm in after before; do
//	    go test ./transport/streaming/ -run '^$' -benchmem -count=1 \
//	      -bench "^BenchmarkAB_Extract_Anthropic/impl=$arm\$" >> "$arm.txt"
//	  done
//	done
//	benchstat before.txt after.txt
//
// And take a same-minute `after`-vs-`after` null control: on this host, under load,
// identical binaries drifted +14.1% at p=0.62. Without that control a 14% "result"
// is indistinguishable from the machine.

func benchExtractAB(b *testing.B, data string) {
	evt := &SSEEvent{Event: "message", Data: data}
	b.Run("impl=before", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for range b.N {
			_, _ = extractDeltaTextReference(evt)
		}
	})
	b.Run("impl=after", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for range b.N {
			_ = extractDeltaText(evt)
		}
	})
}

// BenchmarkAB_Extract_OpenAIContent is the dominant modelled shape. Every extract
// arm now measures the same thing — the price of the validity scan the safety fix
// adds — because the gate runs on every frame regardless of shape. There is no
// throughput win to look for here; the number is the cost side of a correctness
// trade, and recording it is what makes that trade explicit.
func BenchmarkAB_Extract_OpenAIContent(b *testing.B) { benchExtractAB(b, frameOpenAIContent) }

// These three are OpenAI frames that cannot carry delta text. They still decode
// in full, so they pay the gate plus the decode.
func BenchmarkAB_Extract_OpenAIRole(b *testing.B)   { benchExtractAB(b, frameOpenAIRole) }
func BenchmarkAB_Extract_OpenAIFinish(b *testing.B) { benchExtractAB(b, frameOpenAIFinish) }
func BenchmarkAB_Extract_OpenAIUsage(b *testing.B)  { benchExtractAB(b, frameOpenAIUsage) }

// BenchmarkAB_Extract_Anthropic is the binding-B9 shape — a valid-JSON frame this
// function does not model — and the one whose response hooks the C-17 fix
// restored, so it is now on the hot path for real rather than hypothetically.
func BenchmarkAB_Extract_Anthropic(b *testing.B) { benchExtractAB(b, frameAnthropic) }

// BenchmarkAB_Extract_ResponsesAPI and _Gemini are the other two choices-less
// wires named in the C-17 analysis.
func BenchmarkAB_Extract_ResponsesAPI(b *testing.B) {
	benchExtractAB(b, `{"type":"response.output_text.delta","delta":"hello ","item_id":"msg_1","output_index":0}`)
}

func BenchmarkAB_Extract_Gemini(b *testing.B) {
	benchExtractAB(b, `{"candidates":[{"content":{"parts":[{"text":"hello "}],"role":"model"},"index":0}]}`)
}

// BenchmarkAB_Extract_RawText is the raw-fallback path, and the one arm where the
// gate REPLACES work rather than adding it: the validity scan rejects the frame
// and the decode never runs at all.
func BenchmarkAB_Extract_RawText(b *testing.B) { benchExtractAB(b, frameRawText) }

// --- C-20: the usage accumulators ---

// benchFeedAB builds a FRESH accumulator inside each sub-benchmark run on both
// sides. Reusing one across runs let its fallback text buffer grow with every
// -count repetition, so the later runs measured appending into a megabyte-sized
// builder rather than the per-frame extraction — a harness artifact that showed
// up as an implausible +224% B/op on the "after" arm.
func benchFeedAB(b *testing.B, evt *SSEEvent, newRef func() func(*SSEEvent), newAcc func() UsageAccumulator) {
	b.Run("impl=before", func(b *testing.B) {
		ref := newRef()
		b.SetBytes(int64(len(evt.Data)))
		b.ReportAllocs()
		for range b.N {
			ref(evt)
		}
	})
	b.Run("impl=after", func(b *testing.B) {
		acc := newAcc()
		b.SetBytes(int64(len(evt.Data)))
		b.ReportAllocs()
		for range b.N {
			acc.Feed(evt)
		}
	})
}

// There are deliberately no openai/gemini Feed arms. Their byte-level key gates
// were reverted as unsound, so `impl=before` and `impl=after` are the same code and
// the arms would report a meaningless ~0%. The measured win that remains is the
// anthropic event-name hoist below.

// BenchmarkAB_Feed_AnthropicIgnoredEvent is the hoisted-switch win: a `ping`
// frame the accumulator reads nothing from, which used to pay a full validity
// scan before the switch discarded it.
func BenchmarkAB_Feed_AnthropicIgnoredEvent(b *testing.B) {
	evt := &SSEEvent{Event: "ping", Data: `{"type":"ping"}`}
	benchFeedAB(b, evt,
		func() func(*SSEEvent) {
			refAcc := &anthropicAccumulator{}
			return func(e *SSEEvent) { referenceAnthropicFeed(refAcc, e) }
		},
		func() UsageAccumulator { return &anthropicAccumulator{} })
}

// BenchmarkAB_Feed_AnthropicTextDelta is the Anthropic frame that IS read, so
// the hoisted switch adds only a string comparison — local-cost check.
func BenchmarkAB_Feed_AnthropicTextDelta(b *testing.B) {
	evt := &SSEEvent{
		Event: "content_block_delta",
		Data:  `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`,
	}
	benchFeedAB(b, evt,
		func() func(*SSEEvent) {
			refAcc := &anthropicAccumulator{}
			return func(e *SSEEvent) { referenceAnthropicFeed(refAcc, e) }
		},
		func() UsageAccumulator { return &anthropicAccumulator{} })
}
