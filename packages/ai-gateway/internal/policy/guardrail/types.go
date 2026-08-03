// Package guardrail holds the wire contract + pure verdict-mapping logic for
// the standalone compliance-verdict endpoint (POST /v1/guardrail). The endpoint
// runs the deployment's configured compliance pipeline (rule-pack + PII +
// AI-Guard judge) over caller-supplied text and returns an allow/block/redact
// verdict — without relaying an LLM completion.
//
// This package is pipeline-independent and Handler-independent: it defines the
// request/response DTOs and maps a decision.CompliancePipelineResult to the wire
// verdict. The proxy Handler owns admission + BuildPipeline + Execute and calls
// MapResult here. Keeping the mapping pure makes it unit-testable against a
// hand-built CompliancePipelineResult without standing up a pipeline.
package guardrail

// Stage constants select the pipeline direction a guardrail call evaluates.
const (
	StageInput  = "input"  // evaluate a prompt (request-stage hooks)
	StageOutput = "output" // evaluate a completion (response-stage hooks)
)

// Message is one entry of the structured `messages` request input. It carries
// wire json tags (role/content), distinct from the internal
// inputstaging.Message which has none — this is the API DTO, that is the
// judge-truncation staging type.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the POST body of /v1/guardrail. Exactly one of Content / Messages
// must be non-empty. Unknown fields are rejected by the handler
// (DisallowUnknownFields).
type Request struct {
	// Stage is "input" | "output"; empty defaults to "input".
	Stage string `json:"stage,omitempty"`
	// Content is the flat text to evaluate (mutually exclusive with Messages).
	Content string `json:"content,omitempty"`
	// Messages is the structured alternative; contents are joined in turn order.
	Messages []Message `json:"messages,omitempty"`
	// IncludeRedactedContent controls whether RedactedContent is echoed on a
	// redact verdict. nil (omitted) defaults to TRUE (opt-out); the sanitized
	// text is the primary deliverable of the redact path.
	IncludeRedactedContent *bool `json:"include_redacted_content,omitempty"`
}

// WantRedactedContent reports the effective IncludeRedactedContent (default true).
func (r *Request) WantRedactedContent() bool {
	return r.IncludeRedactedContent == nil || *r.IncludeRedactedContent
}

// Redaction is one rule-pack/PII redaction span, as byte offsets into the
// joined evaluated text (UTF-8). AI-Guard judge spans are audit-only and are
// NOT included here (they carry no applicable content address).
type Redaction struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// Assessment is the per-policy breakdown: one entry per hook that flagged the
// content, projected from CompliancePipelineResult.HookResults. It lets a caller
// act on WHY the verdict was reached (block-for-PII vs block-for-injection).
type Assessment struct {
	Check      string   `json:"check"`            // the hook / policy family (HookResult.HookName)
	Decision   string   `json:"decision"`         // approve|reject_hard|block_soft|modify (lowercased)
	Action     string   `json:"action,omitempty"` // approve|redact|block
	Labels     []string `json:"labels,omitempty"` // the hook's tags
	ReasonCode string   `json:"reason_code,omitempty"`
}

// Blocking carries the category/severity/labels of the rule that forced a
// block. Pack / pack_version / rule_id are deliberately NOT exposed — a
// VK-facing endpoint that returned exact rule identifiers is an evasion oracle.
type Blocking struct {
	Category string   `json:"category,omitempty"`
	Severity string   `json:"severity,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

// Metadata carries non-decision diagnostics. v1 carries only latency; the
// judge's cache-hit and per-call cost are not reachable at the verdict layer
// without surfacing them through the hook boundary (a shipped-contract change),
// so they ride the joinable internal ai-guard audit row instead (see e90-s1).
type Metadata struct {
	LatencyMs int `json:"latency_ms"`
}

// Response is the /v1/guardrail 200 body.
type Response struct {
	Action          string       `json:"action"`   // allow|block|redact
	Decision        string       `json:"decision"` // approve|reject_hard|block_soft|modify
	Coverage        string       `json:"coverage"` // full|degraded|none
	Reason          string       `json:"reason,omitempty"`
	ReasonCode      string       `json:"reason_code,omitempty"`
	Labels          []string     `json:"labels,omitempty"`
	Assessments     []Assessment `json:"assessments,omitempty"`
	Blocking        *Blocking    `json:"blocking,omitempty"`
	Redactions      []Redaction  `json:"redactions,omitempty"`
	RedactedContent string       `json:"redacted_content,omitempty"`
	Metadata        Metadata     `json:"metadata"`
}

// Coverage values — the fail-open honesty signal (see verdict.go DeriveCoverage).
const (
	CoverageFull     = "full"     // every applicable hook scanned cleanly
	CoverageDegraded = "degraded" // a hook failed open (errored) — the verdict is NOT a full scan
	CoverageNone     = "none"     // no compliance hooks were bound for this stage (nothing scanned)
)
