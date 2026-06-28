package streaming_test

// The true cross-tier consistency test. An earlier test verified that
// the SHARED helper (responseprehook.Build)
// produces equivalent payloads through two CALLER SHAPES — but the
// real requirement is that the three PIPELINE IMPLEMENTATIONS
// (shared.BufferPipeline, shared.LivePipeline, ai-gateway.LivePipeline)
// produce equivalent payloads for the same SSE body.
//
// This test instantiates all three pipelines, wires the SAME PreHook
// callback against the SAME Registry, feeds each a fresh copy of the
// same SSE body, and asserts the final ci.Normalized JSON is
// bit-identical across all three. Any future fork — a pipeline
// switching to a different PreHook shape, a Modify path that mutates
// Normalized differently, a Registry call dropped from one
// pipeline — fails this test.

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	aigwstreaming "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
	anthropicstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/stream"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/responseprehook"
	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// pipelineExecAdapter adapts a plain hook-runner function (the shape
// ai-gateway uses) to shared.PipelineExecutor (the interface
// shared.LivePipeline / BufferPipeline expects). 1-line method —
// trivial bridge, no business logic so the cross-tier comparison
// stays apples-to-apples.
type pipelineExecAdapter struct {
	run func(context.Context, *hookcore.HookInput) *hookcore.CompliancePipelineResult
}

func (a pipelineExecAdapter) Execute(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
	return a.run(ctx, input)
}

// captureLast holds the last Normalized payload pointer seen by the
// hookRun across all checkpoints. For shared.BufferPipeline that's
// the single checkpoint; for live pipelines it's the final
// checkpoint. Both should equal the Registry.Normalize result of the
// full body bytes.
type captureLast struct {
	mu         sync.Mutex
	normalized *normcore.NormalizedPayload
}

func (c *captureLast) record(input *hookcore.HookInput) {
	if input == nil || input.Normalized == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.normalized = input.Normalized
}

func (c *captureLast) get() *normcore.NormalizedPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.normalized
}

// responseWriterStub satisfies http.ResponseWriter (what ai-gateway's
// LivePipeline.Process needs) backed by a bytes.Buffer so the test
// can ignore the wire output and focus on the captured Normalized.
type responseWriterStub struct {
	buf    bytes.Buffer
	header http.Header
}

func (r *responseWriterStub) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}
func (r *responseWriterStub) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *responseWriterStub) WriteHeader(_ int)           {}

// TestThreePipelineConsistency_SameBody_SameNormalized is the
// cross-tier assertion. For each fixture body, instantiate shared.
// BufferPipeline, shared.LivePipeline, and ai-gateway.LivePipeline
// with the same PreHook + Registry, run each against a fresh copy of
// the body, then compare the final ci.Normalized JSON.
func TestThreePipelineConsistency_SameBody_SameNormalized(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)

	// Bodies must be OpenAI-shape: ai-gateway's LivePipeline triggers
	// checkpoints by extracting `choices.0.delta.content` from each
	// SSE chunk. Non-OpenAI bodies (Anthropic typed events, Gemini
	// candidates) never grow `accumulated` past FirstInspectChars and
	// thus never fire a checkpoint via this path. In production those
	// bodies arrive ALREADY transcoded to OpenAI shape by the
	// `canonicalbridge.StreamTranscoder` in proxy_cache.go's
	// chunkSSEReader, so the LivePipeline only ever sees OpenAI
	// wire. The cross-tier test mirrors that contract — feeding raw
	// Anthropic bytes here would test a code path that prod never
	// reaches.
	//
	// adapterID values still cover the three Tier 1 codec families so
	// the Registry routing surface gets exercised; the bodies are
	// kept OpenAI-shape so the LivePipeline checkpoint trigger fires.
	cases := []struct {
		name        string
		adapterID   string
		contentType string
		body        string
	}{
		{
			name:        "openai_adapter",
			adapterID:   "openai",
			contentType: "text/event-stream",
			body: "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hello world this is a long enough delta to cross the checkpoint threshold\"}}]}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:        "anthropic_adapter_routed_to_openai_codec_via_bedrock",
			adapterID:   "bedrock", // Anthropic-Messages normalizer routes via "bedrock" key too
			contentType: "text/event-stream",
			// Bedrock surface SOMETIMES carries OpenAI-shape chunks
			// (when a customer fronts Bedrock with the openai-compat
			// adapter). This case pins that the OpenAI body still
			// produces a Normalized that all three pipelines agree on
			// even when adapterID isn't "openai".
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"plenty of text to trigger checkpoint at every fixture level we configure\"}}]}\n\n" +
				"data: [DONE]\n\n",
		},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Per-pipeline PreHook — same Build options, fresh callback
			// instance per call so each pipeline gets its own closure
			// (closing over a shared rawAcc would couple them).
			newPreHook := func() hookcore.PreHookCallback {
				return responseprehook.Build(responseprehook.Options{
					Ctx:         ctx,
					Registry:    reg,
					AdapterID:   tc.adapterID,
					ContentType: tc.contentType,
					Direction:   normcore.DirectionResponse,
				})
			}

			approveRun := func(c *captureLast) func(context.Context, *hookcore.HookInput) *hookcore.CompliancePipelineResult {
				return func(_ context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
					c.record(input)
					return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
				}
			}

			// --- 1. shared.BufferPipeline ---
			bufCap := &captureLast{}
			bp := sharedstreaming.NewBufferPipeline(sharedstreaming.BufferConfig{}, pipelineExecAdapter{run: approveRun(bufCap)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			bp.WithPreHook(newPreHook())
			if _, err := bp.Process(ctx, strings.NewReader(tc.body), &bytes.Buffer{}, &hookcore.HookInput{
				RequestID: "cross-tier",
				Stage:     "response",
			}); err != nil {
				t.Fatalf("shared.BufferPipeline.Process: %v", err)
			}

			// --- 2. shared.LivePipeline ---
			liveCap := &captureLast{}
			lp := sharedstreaming.NewLivePipeline(sharedstreaming.LiveConfig{
				CheckpointChars:    10,
				MinCheckpointChars: 10,
				MaxCheckpointChars: 100,
			}, pipelineExecAdapter{run: approveRun(liveCap)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			lp.WithPreHook(newPreHook())
			if _, err := lp.Process(ctx, strings.NewReader(tc.body), &bytes.Buffer{}, &hookcore.HookInput{
				RequestID: "cross-tier",
				Stage:     "response",
			}); err != nil {
				t.Fatalf("shared.LivePipeline.Process: %v", err)
			}

			// --- 3. ai-gateway.LivePipeline ---
			aiCap := &captureLast{}
			aiRunner := approveRun(aiCap)
			aiLp := aigwstreaming.NewLivePipeline(aigwstreaming.LiveConfig{
				FirstInspectChars:  10,
				ReinspectStepChars: 10,
			}, aiRunner, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			aiLp.WithPreHook(newPreHook())
			aiLp.Process(ctx, strings.NewReader(tc.body), &responseWriterStub{}, &aigwstreaming.StreamHookContext{
				RequestID: "cross-tier",
			})

			// --- Compare ---
			bufJSON := mustMarshalNormalized(t, "shared.BufferPipeline", bufCap.get())
			liveJSON := mustMarshalNormalized(t, "shared.LivePipeline", liveCap.get())
			aiJSON := mustMarshalNormalized(t, "ai-gateway.LivePipeline", aiCap.get())

			if bufJSON != liveJSON {
				t.Errorf("shared.BufferPipeline vs shared.LivePipeline divergence:\n  buffer: %s\n  live:   %s", bufJSON, liveJSON)
			}
			if liveJSON != aiJSON {
				t.Errorf("shared.LivePipeline vs ai-gateway.LivePipeline divergence:\n  shared: %s\n  aigw:   %s", liveJSON, aiJSON)
			}
		})
	}
}

func mustMarshalNormalized(t *testing.T, pipeline string, p *normcore.NormalizedPayload) string {
	t.Helper()
	if p == nil {
		t.Fatalf("%s never stamped a Normalized payload — hookRun must see at least one non-nil checkpoint", pipeline)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("%s marshal: %v", pipeline, err)
	}
	return string(b)
}

// TestThreePipelineConsistency_TranscoderWired_AnthropicUpstream. The original
// transcoder-wired variant fabricated provcore.Chunk values directly,
// which only exercised the encoder→pipeline edge — the upstream
// decoder→encoder hand-off (the surface that actually breaks in prod
// when an Anthropic codec change drops a field the encoder maps from)
// was still untested.
//
// This variant feeds REAL Anthropic SSE wire bytes through the
// Anthropic stream decoder (the exact code path proxy_cache.go's
// chunkSSEReader runs in prod), then through
// canonicalbridge.NewChatCompletionsStreamEncoder to derive the
// transcoded OpenAI SSE bytes the pipelines receive, then runs all
// three pipelines against the transcoded body and asserts
// ci.Normalized parity.
//
// What this catches that the prior fixture didn't:
//   - Anthropic decoder emits a chunk shape the encoder doesn't
//     handle (drift between two unrelated codec packages).
//   - Encoder maps from a provcore.Chunk field the decoder stopped
//     populating (e.g. NativeEvent gets renamed).
//   - End-to-end character-set / escaping issues across the decode
//     → encode boundary.
//
// Sanity guard: if the encoder emits a wire shape the OpenAI chat
// normalizer can't parse, BufferPipeline's capture comes back nil
// and mustMarshalNormalized fails with a clear "never stamped"
// message — the early signal we'd otherwise lack.
func TestThreePipelineConsistency_TranscoderWired_AnthropicUpstream(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)
	ctx := context.Background()

	// Real Anthropic SSE wire — exactly the bytes an /v1/messages
	// upstream emits. Multiple deltas + message_start with usage so the
	// decoder exercises both header (model id from message_start) and
	// streaming text paths.
	anthropicWire := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_xyz\",\"model\":\"claude-3-sonnet-20240229\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":42,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first delta with plenty of text to cross checkpoint thresholds easily\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" second delta to ensure live pipelines fire at least one checkpoint\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	// Run through the real Anthropic decoder — this is what
	// proxy_cache.go does in prod via chunkSSEReader.
	decoder := anthropicstream.NewStreamDecoder(slog.New(slog.NewTextHandler(io.Discard, nil)))
	session, err := decoder.Open(io.NopCloser(strings.NewReader(anthropicWire)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("anthropic decoder Open: %v", err)
	}

	// Pump canonical chunks through the OpenAI encoder. This is the
	// second prod hop: chunkSSEReader feeds the decoded provcore.Chunk
	// values into the StreamTranscoder returned by
	// CanonicalBridge.NewStreamTranscoder(ingress, target, model).
	enc := canonicalbridge.NewChatCompletionsStreamEncoder("claude-3-sonnet-20240229")
	var transcoded bytes.Buffer
	for {
		chunk, err := session.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("anthropic session.Next: %v", err)
		}
		out, encErr := enc.Write(ctx, chunk)
		if encErr != nil {
			t.Fatalf("openai encoder Write chunk %+v: %v", chunk, encErr)
		}
		transcoded.Write(out)
		if chunk.Done {
			break
		}
	}
	// Encoders omit data:[DONE] (LivePipeline appends it via
	// EmitOpenAIDone in prod). Add it so the test fixture's normalizer
	// sees the standard OpenAI terminator regardless of pipeline.
	if !bytes.Contains(transcoded.Bytes(), []byte("[DONE]")) {
		transcoded.WriteString("data: [DONE]\n\n")
	}
	body := transcoded.String()

	newPreHook := func() hookcore.PreHookCallback {
		return responseprehook.Build(responseprehook.Options{
			Ctx:         ctx,
			Registry:    reg,
			AdapterID:   "openai", // post-transcode shape — that's what the pipelines see
			ContentType: "text/event-stream",
			Direction:   normcore.DirectionResponse,
		})
	}
	approveRun := func(c *captureLast) func(context.Context, *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		return func(_ context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
			c.record(input)
			return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
		}
	}

	// --- 1. shared.BufferPipeline ---
	bufCap := &captureLast{}
	bp := sharedstreaming.NewBufferPipeline(sharedstreaming.BufferConfig{}, pipelineExecAdapter{run: approveRun(bufCap)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bp.WithPreHook(newPreHook())
	if _, err := bp.Process(ctx, strings.NewReader(body), &bytes.Buffer{}, &hookcore.HookInput{
		RequestID: "transcoder-tier",
		Stage:     "response",
	}); err != nil {
		t.Fatalf("shared.BufferPipeline.Process: %v", err)
	}

	// --- 2. shared.LivePipeline ---
	liveCap := &captureLast{}
	lp := sharedstreaming.NewLivePipeline(sharedstreaming.LiveConfig{
		CheckpointChars:    10,
		MinCheckpointChars: 10,
		MaxCheckpointChars: 100,
	}, pipelineExecAdapter{run: approveRun(liveCap)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lp.WithPreHook(newPreHook())
	if _, err := lp.Process(ctx, strings.NewReader(body), &bytes.Buffer{}, &hookcore.HookInput{
		RequestID: "transcoder-tier",
		Stage:     "response",
	}); err != nil {
		t.Fatalf("shared.LivePipeline.Process: %v", err)
	}

	// --- 3. ai-gateway.LivePipeline ---
	aiCap := &captureLast{}
	aiLp := aigwstreaming.NewLivePipeline(aigwstreaming.LiveConfig{
		FirstInspectChars:  10,
		ReinspectStepChars: 10,
	}, approveRun(aiCap), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	aiLp.WithPreHook(newPreHook())
	aiLp.Process(ctx, strings.NewReader(body), &responseWriterStub{}, &aigwstreaming.StreamHookContext{
		RequestID: "transcoder-tier",
	})

	// --- Compare ---
	bufJSON := mustMarshalNormalized(t, "shared.BufferPipeline (transcoded)", bufCap.get())
	liveJSON := mustMarshalNormalized(t, "shared.LivePipeline (transcoded)", liveCap.get())
	aiJSON := mustMarshalNormalized(t, "ai-gateway.LivePipeline (transcoded)", aiCap.get())

	if bufJSON != liveJSON {
		t.Errorf("transcoded body: shared.BufferPipeline vs shared.LivePipeline divergence:\n  buffer: %s\n  live:   %s", bufJSON, liveJSON)
	}
	if liveJSON != aiJSON {
		t.Errorf("transcoded body: shared.LivePipeline vs ai-gateway.LivePipeline divergence:\n  shared: %s\n  aigw:   %s", liveJSON, aiJSON)
	}
}
