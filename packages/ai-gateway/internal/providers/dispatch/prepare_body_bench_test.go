package dispatch

// G1-G4 benchmark matrix for the native-leg body preparation
// (provider-adapter request-contract program; the slice gate is the DELTA
// against a baseline measured at the pre-change HEAD on the same machine,
// never an absolute number — absolute numbers here drift with hardware).
//
// Pinned body shapes:
//   G1  fast path — model already the provider id, non-stream; 50KB and 2KB.
//       Expected: verbatim forward (no decode, no re-marshal).
//   G2  stamp path — model is an alias and must be rewritten; 50KB.
//       Expected: surgical sjson set, no full decode.
//   G3  streaming all-skip — conformant streaming body (provider model id,
//       stream:true, stream_options.include_usage present) with the stream
//       fields at the HEAD of the object; 50KB.
//       Expected: probe-only forward; allocs stay flat.
//   G4  streaming tail-position — same conformant body but stream fields at
//       the TAIL (worst case for the probe scan); 50KB. The allocs pin
//       (≤4) guards "no second body pass / no hoisted dup-key scan":
//       an unconditional dup-key or validity scan added to the streaming
//       leg shows up as allocs or bytes here.
//
// G1-G4 bodies are non-quirk models (gpt-4o). G5/G6 are the quirk-family
// gates added by the openai migration slice:
//   G5  quirk edit — reasoning model (o3) WITH the rejected sampling params
//       present (tail position); 50KB, non-stream. The strip must fire.
//   G6  quirk conformant — reasoning model (o3), body already conformant
//       (nothing due). This is the post-bridge re-entry shape: the legacy
//       dispatch path paid a full model-keyed map round-trip here even
//       though no edit was needed; presence-based probing must forward
//       verbatim (byte-identical output, near-G1 cost).

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/rewrites"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// benchPad builds a deterministic chat body of roughly size bytes with the
// given head/tail field placement. Field ordering is part of the pinned
// shape: G3 places stream fields first, G4 places them last.
func benchChatBody(size int, model string, streamFields string, tail bool) []byte {
	filler := strings.Repeat("x", 64)
	var msgs []string
	msgs = append(msgs, `{"role":"system","content":"You are a benchmark."}`)
	for body := 128; body < size; body += 96 {
		msgs = append(msgs, `{"role":"user","content":"`+filler+`"}`)
	}
	core := `"model":"` + model + `","messages":[` + strings.Join(msgs, ",") + `]`
	switch {
	case streamFields == "":
		return []byte(`{` + core + `}`)
	case tail:
		return []byte(`{` + core + `,` + streamFields + `}`)
	default:
		return []byte(`{` + streamFields + `,` + core + `}`)
	}
}

func benchAdapter(b *testing.B) *specAdapter {
	b.Helper()
	// Mirror the real openai spec: the contract-carrying codec, no
	// transitional callback (openai is drained).
	spec := specFrom(&fakeTransport{}, openaicodec.New(rewrites.OpenAIContract()), &fakeStreamDecoder{}, &fakeErrorNormalizer{}, provcore.FormatOpenAI)
	return &specAdapter{spec: spec, log: slog.Default()}
}

func benchReq(body []byte, stream bool) Request {
	return Request{
		Body:       body,
		BodyFormat: provcore.FormatOpenAI,
		WireShape:  typology.WireShapeOpenAIChat,
		Stream:     stream,
		Target: provcore.CallTarget{
			ProviderModelID: "gpt-4o",
			Format:          provcore.FormatOpenAI,
		},
	}
}

func runPrepareBench(b *testing.B, body []byte, stream bool) {
	a := benchAdapter(b)
	req := benchReq(body, stream)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, _, err := a.prepareBodyFull(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrepareBody_G1_FastPath(b *testing.B) {
	for _, size := range []int{2 << 10, 50 << 10} {
		b.Run(fmt.Sprintf("%dKB", size>>10), func(b *testing.B) {
			runPrepareBench(b, benchChatBody(size, "gpt-4o", "", false), false)
		})
	}
}

func BenchmarkPrepareBody_G2_Stamp(b *testing.B) {
	runPrepareBench(b, benchChatBody(50<<10, "gpt-4o-alias", "", false), false)
}

// benchQuirkAdapter mirrors the production openai wiring for quirk-family
// models. The G5/G6 baseline was measured on the transitional
// dispatch-callback map path this codec replaced; the gate is the
// before/after delta of the same logical scenario.
func benchQuirkAdapter(b *testing.B) *specAdapter {
	b.Helper()
	return benchAdapter(b)
}

func benchQuirkReq(body []byte, stream bool) Request {
	req := benchReq(body, stream)
	req.Target.ProviderModelID = "o3"
	return req
}

func runQuirkPrepareBench(b *testing.B, body []byte, stream bool) {
	a := benchQuirkAdapter(b)
	req := benchQuirkReq(body, stream)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, _, err := a.prepareBodyFull(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrepareBody_G5_QuirkEdit(b *testing.B) {
	body := benchChatBody(50<<10, "o3",
		`"temperature":0,"top_p":0.9,"max_tokens":256`, true)
	runQuirkPrepareBench(b, body, false)
}

func BenchmarkPrepareBody_G6_QuirkConformant(b *testing.B) {
	b.Run("nonstream", func(b *testing.B) {
		runQuirkPrepareBench(b, benchChatBody(50<<10, "o3", "", false), false)
	})
	b.Run("stream", func(b *testing.B) {
		runQuirkPrepareBench(b, benchChatBody(50<<10, "o3",
			`"stream":true,"stream_options":{"include_usage":true}`, true), true)
	})
}

func BenchmarkPrepareBody_G3_StreamAllSkipHead(b *testing.B) {
	runPrepareBench(b, benchChatBody(50<<10, "gpt-4o",
		`"stream":true,"stream_options":{"include_usage":true}`, false), true)
}

func BenchmarkPrepareBody_G4_StreamAllSkipTail(b *testing.B) {
	runPrepareBench(b, benchChatBody(50<<10, "gpt-4o",
		`"stream":true,"stream_options":{"include_usage":true}`, true), true)
}
