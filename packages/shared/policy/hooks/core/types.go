// Package core defines the core types, interfaces, and framework primitives
// for the compliance hook pipeline shared by all three data plane services.
package core

import (
	"context"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Decision vocabulary — type-aliased from policy/decision so that every
// existing caller using hooks.Decision / hooks.Approve / etc. continues to
// compile unchanged. New code should import policy/decision directly.
type Decision = decision.Decision

const (
	Approve    = decision.Approve
	RejectHard = decision.RejectHard
	BlockSoft  = decision.BlockSoft
	// Modify indicates the transaction should be modified before forwarding.
	// Valid in the Hook interface; the Go compliance-proxy never binds MODIFY hooks.
	Modify  = decision.Modify
	Abstain = decision.Abstain
)

// ContentBlock is a provider-agnostic content unit aliased from decision.ContentBlock.
// Retained for hook implementations that still emit transitional ModifiedContent;
// new consumers should use TransformSpans via normalize.ApplySpans.
type ContentBlock = decision.ContentBlock

// BlockingRule is the attribution record for a rule-pack match that caused
// a hook to reject (hard or soft) a request.
type BlockingRule = decision.BlockingRule

// InflightAction — aliased from decision.
type InflightAction = decision.InflightAction

const (
	InflightApprove   = decision.InflightApprove
	InflightBlockHard = decision.InflightBlockHard
	InflightBlockSoft = decision.InflightBlockSoft
	InflightRedact    = decision.InflightRedact
)

// StorageAction — aliased from decision.
type StorageAction = decision.StorageAction

const (
	StorageKeep        = decision.StorageKeep
	StorageRedact      = decision.StorageRedact
	StorageDropContent = decision.StorageDropContent
)

// OnMatchConfig is the unified shape every content-touching hook reads
// from cfg.Config["onMatch"].
type OnMatchConfig struct {
	InflightAction InflightAction `yaml:"inflightAction" json:"inflightAction"`
	StorageAction  StorageAction  `yaml:"storageAction"  json:"storageAction"`
	Replacement    string         `yaml:"replacement"    json:"replacement,omitempty"`
}

// CompliancePipelineResult — aliased from decision.
type CompliancePipelineResult = decision.CompliancePipelineResult

// HookResult — aliased from decision.
type HookResult = decision.HookResult

// Standard ReasonCode constants used on HookResult.ReasonCode.
const (
	ReasonRedactInflightUnsupported = decision.ReasonRedactInflightUnsupported
	ReasonRedactStorageOnlyByPolicy = decision.ReasonRedactStorageOnlyByPolicy
	ReasonStorageDroppedByPolicy    = decision.ReasonStorageDroppedByPolicy
	ReasonAIGuardSuggestedVsPolicy  = decision.ReasonAIGuardSuggestedVsPolicy
	ReasonFailClosed                = decision.ReasonFailClosed
)

// HookInput is the structured data injected by the scheduler into hooks.
// Hooks never receive raw provider JSON — content arrives as the
// canonical NormalizedPayload produced by shared/normalize at capture
// time. Producers (ai-gateway provider adapters, compliance-proxy /
// agent traffic adapters) populate Normalized before invoking the
// pipeline. Regex-based hooks read text via Normalized.TextProjection().
type HookInput struct {
	RequestID string // X-Nexus-Request-Id for traceability

	// Stage this hook is executing in
	Stage string // "request" / "response" / "connection"

	// Normalized is the canonical kind-discriminated payload produced
	// by shared/normalize. nil for connection-stage hooks (no body),
	// for empty captures, or when the provider/content-type has no
	// registered normalizer (the hook should ABSTAIN or operate on
	// metadata only — content-scanning hooks treat nil as "no text").
	Normalized *normalize.NormalizedPayload

	// AI metadata
	Model        string
	FinishReason string
	TokenCount   int

	// LLM signal detection — populated by the traffic adapter's
	// DetectRequestMeta before the hook pipeline runs.
	DetectedProvider  string // e.g. "openai", "anthropic", "gemini"
	DetectedModel     string // populated when the adapter parsed a model from the body
	ApiKeyClass       string // "sk-ant-", "nvk_", "AIza", … ("" = unknown)
	ApiKeyFingerprint string // SHA256(key)[:8] hex (16 chars)

	// Network context
	SourceIP    string
	TargetHost  string
	Path        string
	Method      string
	IngressType string // "AI_GATEWAY" / "COMPLIANCE_PROXY" / "AGENT"

	// Size info
	BodySize    int64
	ContentType string

	// Upstream compliance context (set by earlier hooks in the pipeline).
	UpstreamTags   []string `json:"upstreamTags,omitempty"`
	ProviderRegion string   // target provider's deployment region

	// TLS carries connection-stage TLS context (SNI, client cert fingerprint).
	TLS *TLSInfo `json:"tls,omitempty"`

	// Endpoint + modality context populated by the handler before invoking
	// the pipeline. Connection-stage hooks leave these zero-valued. An empty
	// string is treated as compatible by all SupportsEndpoint / SupportsModality
	// helpers so unclassified requests still pass all hooks.
	EndpointType   EndpointType `json:"endpointType,omitempty"`
	InputModality  []Modality   `json:"inputModality,omitempty"`
	OutputModality []Modality   `json:"outputModality,omitempty"`
}

// TextSegments is the canonical helper hooks use to obtain the flat
// list of text fragments to scan.
func (i *HookInput) TextSegments() []string {
	if i == nil || i.Normalized == nil {
		return nil
	}
	return i.Normalized.TextProjection()
}

// PreHookCallback is the canonical "stamp Normalized before hook
// executor sees the input" contract used by SSE streaming pipelines
// across all three ingress services (agent / compliance-proxy /
// ai-gateway).
//
// Defined here at the hooks/core top-level so:
//   - shared/transport/streaming.PreHookCallback type-aliases it
//   - ai-gateway/internal/platform/streaming.PreHookCallback type-
//     aliases it
//   - both pipelines accept the same value at WithPreHook time
//   - the helper builder (shared/transport/normalize/responseprehook)
//     returns a single concrete type that all three services consume
//
// Contract: invoked between Phase 1 (read SSE body) and Phase 2 (run
// hooks). MUST be idempotent — LivePipeline fires per-checkpoint, so
// the same callback runs many times with increasing rawBody. Should
// stamp ci.Normalized to a normalize.NormalizedPayload (typically via
// a Registry call on rawBody) so hooks see structured content instead
// of the flat-text fallback the pipelines build by default. Callback
// also typically stamps the audit info's ResponseNormalized field so
// the audit row carries the same payload.
//
// nil-safe / panic-safe is the caller's responsibility (see #97 for
// the recover wrappers in the pipeline impls).
type PreHookCallback func(rawBody []byte, ci *HookInput)

// TextSegmentsWith is the scope-aware sibling of TextSegments.
func (i *HookInput) TextSegmentsWith(opts normalize.TextProjectionOptions) []string {
	if i == nil || i.Normalized == nil {
		return nil
	}
	return i.Normalized.TextProjectionWith(opts)
}

// ProjectionOptions returns the TextProjectionOptions implied by the
// hook config's Scope field.
func (c *HookConfig) ProjectionOptions() normalize.TextProjectionOptions {
	if c == nil {
		return normalize.TextProjectionOptions{}
	}
	opts := normalize.TextProjectionOptions{}
	if c.Scope == "include_reasoning" {
		opts.IncludeReasoning = true
	}
	return opts
}

// PayloadFromTextSegments is a convenience used by test fixtures and
// transitional adapter paths to construct a NormalizedPayload from a
// flat list of user-role text segments.
func PayloadFromTextSegments(segments []string) *normalize.NormalizedPayload {
	if len(segments) == 0 {
		return &normalize.NormalizedPayload{Kind: normalize.KindAIChat, NormalizeVersion: normalize.SchemaVersion}
	}
	content := make([]normalize.ContentBlock, 0, len(segments))
	for _, s := range segments {
		content = append(content, normalize.ContentBlock{Type: normalize.ContentText, Text: s})
	}
	return &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Protocol:         "synthetic",
		Messages:         []normalize.Message{{Role: normalize.RoleUser, Content: content}},
	}
}

// SpansFromModifiedContent computes TransformSpans for a transitional
// hook implementation that still produces ModifiedContent as a flat
// projection of the input.TextSegments.
func SpansFromModifiedContent(input *HookInput, modified []ContentBlock, source normalize.TransformSource, sourceID string, action normalize.TransformAction) []normalize.TransformSpan {
	if input == nil || input.Normalized == nil || len(modified) == 0 {
		return nil
	}
	original := input.TextSegments()
	if len(original) == 0 {
		return nil
	}
	limit := len(modified)
	if len(original) < limit {
		limit = len(original)
	}
	spans := make([]normalize.TransformSpan, 0, limit)
	idx := 0
	for mi, m := range input.Normalized.Messages {
		for ci, b := range m.Content {
			if b.Type != normalize.ContentText && b.Type != normalize.ContentToolResult {
				continue
			}
			if idx >= limit {
				return spans
			}
			origText := original[idx]
			newText := modified[idx].Text
			idx++
			if origText == newText {
				continue
			}
			addr := fmt.Sprintf("messages.%d.content.%d", mi, ci)
			if b.Type == normalize.ContentToolResult {
				addr = fmt.Sprintf("messages.%d.content.%d.toolResult", mi, ci)
			}
			spans = append(spans, normalize.TransformSpan{
				Source:         source,
				SourceID:       sourceID,
				Action:         action,
				ContentAddress: addr,
				Start:          0,
				End:            len(origText),
				Replacement:    newText,
			})
		}
	}
	return spans
}

// HookConfig is the declarative configuration for a hook instance.
type HookConfig struct {
	ID                string   `yaml:"id"                json:"id"`
	ImplementationID  string   `yaml:"implementationId"  json:"implementationId"`
	Name              string   `yaml:"name"              json:"name"`
	Priority          int      `yaml:"priority"          json:"priority"`
	Enabled           bool     `yaml:"enabled"           json:"enabled"`
	Stage             string   `yaml:"stage"             json:"stage"`        // "request", "response", "connection"
	FailBehavior      string   `yaml:"failBehavior"      json:"failBehavior"` // "fail-open" or "fail-closed"
	TimeoutMs         int      `yaml:"timeoutMs"         json:"timeoutMs"`
	ApplicableIngress []string `yaml:"applicableIngress" json:"applicableIngress"` // e.g. ["ALL"], ["COMPLIANCE_PROXY"]
	// ApplicableTrafficKinds filters this hook by NormalizedPayload.kind.
	ApplicableTrafficKinds []string `yaml:"applicableTrafficKinds" json:"applicableTrafficKinds,omitempty"`
	// Scope opts the rule in to scanning canonical content blocks beyond the default.
	Scope  string         `yaml:"scope,omitempty"        json:"scope,omitempty"`
	Config map[string]any `yaml:"config"                 json:"config"`
}

// EndpointType identifies the API endpoint category a request targets.
// String constants match Prometheus label values and Postgres column
// values; no translation layer is needed.
//
// Type-aliased to typology.EndpointKind (E87-S2) so the entire codebase
// shares one canonical Axis-1 enum. The EndpointType* constants below
// are forwarded to typology.EndpointKind* — same underlying values, same
// wire format.
type EndpointType = typology.EndpointKind

const (
	EndpointTypeChat            = typology.EndpointKindChat
	EndpointTypeEmbeddings      = typology.EndpointKindEmbeddings
	EndpointTypeImageGeneration = typology.EndpointKindImageGeneration
	EndpointTypeTTS             = typology.EndpointKindTTS
	EndpointTypeSTT             = typology.EndpointKindSTT
	EndpointTypeVideoGeneration = typology.EndpointKindVideoGeneration
	EndpointTypeBatch           = typology.EndpointKindBatch
	EndpointTypeJob             = typology.EndpointKindJob
)

// Modality identifies the content modality carried by a request or response.
// String constants match Prometheus label values.
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
	ModalityAudio Modality = "audio"
	ModalityVideo Modality = "video"
)

// Hook is the interface every compliance hook must implement.
//
// SupportsEndpoint and SupportsModality are queried at BuildPipeline time so
// the pipeline is filtered before any request is executed. Embed one of the
// pre-built helper structs (ChatOnly, AnyEndpointAnyModality,
// TextOnlyContentScanning) to satisfy these methods without boilerplate.
type Hook interface {
	Execute(ctx context.Context, input *HookInput) (*HookResult, error)
	// SupportsEndpoint returns true when this hook applies to the given
	// endpoint type. An empty EndpointType ("") must return true so that
	// callers that have not yet classified the endpoint (e.g. connection-stage
	// hooks built before the classifier runs) still include the hook.
	SupportsEndpoint(EndpointType) bool
	// SupportsModality returns true when this hook applies to at least one of
	// the modalities present in the request/response. An empty Modality ("")
	// must return true for the same backward-compatibility reason.
	SupportsModality(Modality) bool
}

// ChatOnly is the applicability helper for hooks that exclusively apply to
// text-based chat traffic. Embed it into a hook struct to satisfy
// SupportsEndpoint and SupportsModality: SupportsEndpoint returns true only
// for EndpointTypeChat (or ""); SupportsModality returns true only for
// ModalityText (or "").
//
// Usage: declare your hook struct with this field embedded, e.g.:
//
//	type MyHook struct {
//	    core.ChatOnly
//	    // ... other fields ...
//	}
type ChatOnly struct{}

// SupportsEndpoint returns true for EndpointTypeChat and for the empty string
// (backward-compatible default when the caller has not yet classified the
// endpoint).
func (ChatOnly) SupportsEndpoint(e EndpointType) bool { return e == EndpointTypeChat || e == "" }

// SupportsModality returns true for ModalityText and for the empty string.
func (ChatOnly) SupportsModality(m Modality) bool { return m == ModalityText || m == "" }

// AnyEndpointAnyModality is the applicability helper for Class-B hooks that
// must run on every endpoint and every modality (rate limiter, IP filter,
// data-residency, request-size, webhook-forward, noop).
type AnyEndpointAnyModality struct{}

// SupportsEndpoint always returns true.
func (AnyEndpointAnyModality) SupportsEndpoint(EndpointType) bool { return true }

// SupportsModality always returns true.
func (AnyEndpointAnyModality) SupportsModality(Modality) bool { return true }

// TextOnlyContentScanning is the applicability helper for text-content
// scanning hooks (PII detector, keyword filter, content safety, quality
// checker, rulepack engine). These hooks apply to endpoints that carry
// synchronous human-readable text content.
//
// EndpointTypeEmbeddings is included: embedding request inputs are plain
// text and must be scanned by PII / keyword / safety hooks. The pipeline's
// stage filter (BuildPipeline with stage="response") separately skips
// these hooks on the response side because embedding responses contain
// only float vectors with no scannable text.
//
// EndpointTypeBatch and EndpointTypeJob are excluded because those
// endpoints have no synchronous text body available at hook evaluation time.
type TextOnlyContentScanning struct{}

// TextOnlyContentScanningMarker is the opt-in marker interface that lets
// BuildPipeline identify Class-A text hooks for stage-aware filtering.
// Embed TextOnlyContentScanning (which implements this method) to satisfy
// the marker without boilerplate.
type TextOnlyContentScanningMarker interface {
	// TextOnlyContentScanningMark is a no-op discriminator. Its presence
	// signals "this hook is a text-only content scanner" to the pipeline
	// builder so it can skip the hook on embedding response stages.
	TextOnlyContentScanningMark()
}

// TextOnlyContentScanningMark satisfies the TextOnlyContentScanningMarker
// interface. Embedding TextOnlyContentScanning in a hook struct causes the
// pipeline builder to skip this hook on embedding-response stages.
func (TextOnlyContentScanning) TextOnlyContentScanningMark() {}

// SupportsEndpoint returns true for endpoints that carry synchronous
// human-readable text content (chat, STT, image-gen prompt, TTS, video-gen
// prompt, embeddings request inputs, or the empty string). Returns false for
// EndpointTypeBatch and EndpointTypeJob.
//
// Although this returns true for EndpointTypeEmbeddings, text hooks must NOT
// run on embedding responses (float vectors contain no scannable text).
// BuildPipeline enforces this via the TextOnlyContentScanningMarker check when
// stage="response" and endpoint=EndpointTypeEmbeddings.
func (TextOnlyContentScanning) SupportsEndpoint(e EndpointType) bool {
	switch e {
	case EndpointTypeBatch, EndpointTypeJob:
		return false
	}
	return true // chat, embeddings, stt, image_generation, tts, video_generation, or ""
}

// SupportsModality returns true for ModalityText and for the empty string.
func (TextOnlyContentScanning) SupportsModality(m Modality) bool {
	return m == ModalityText || m == ""
}

// HookFactory creates a Hook from its declarative config.
type HookFactory func(cfg *HookConfig) (Hook, error)

// ConnectionStageCompatible is an opt-in marker. Hooks that want to run at
// stage="connection" must implement this interface (a no-op method) to
// declare that they never return MODIFY and never depend on Content.
type ConnectionStageCompatible interface {
	ConnectionStageOK()
}
