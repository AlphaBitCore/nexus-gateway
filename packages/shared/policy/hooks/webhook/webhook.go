package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// WebhookForward sends hook input to a configured HTTP endpoint and reads
// the decision from the response.
//
// Shared across all three data-plane services (agent, compliance-proxy,
// ai-gateway) so a webhook hook configured in the admin UI fires on traffic
// captured by any service.
//
// Agent caveat: the agent's local intercept pipeline has no outbound HTTP
// egress for hook plugins. Webhook hooks bound to agent ingress will fail
// at execute time. Use rule-pack-engine hooks for agent-side enforcement.
//
// AI-Guard redaction spans: when the endpoint speaks the AI-Guard extended
// shape (redactions[] of {start, end, replacement, action, reason}),
// webhook-forward decodes each redaction into a normalize.TransformSpan.
// The offsets index a flat joined projection; spans use
// ContentAddress = "webhook.flat" which ApplySpans does not resolve, so
// they land in the audit row but are not applied inflight. Inflight
// redaction of AI-Guard suggestions requires the internal aiguard-classify
// path in ai-gateway.
//
// WebhookPayloadMode controls how much of the captured payload is sent to
// the remote endpoint. Default `redacted` ships projected text segments;
// `full` ships the entire NormalizedPayload; `metadata-only` ships only
// the envelope with no body content.
type WebhookPayloadMode string

const (
	WebhookPayloadRedacted     WebhookPayloadMode = "redacted"
	WebhookPayloadFull         WebhookPayloadMode = "full"
	WebhookPayloadMetadataOnly WebhookPayloadMode = "metadata-only"
)

// WebhookForward sends compliance hook input to a configured HTTP endpoint.
// Applies to all endpoints and modalities via AnyEndpointAnyModality;
// the remote webhook decides what to do with the payload.
type WebhookForward struct {
	core.AnyEndpointAnyModality
	endpoint       string
	timeout        time.Duration
	client         *http.Client
	payloadMode    WebhookPayloadMode
	onMatch        core.OnMatchConfig
	projectionOpts normalize.TextProjectionOptions
}

// NewWebhookForward creates a webhook-forward hook from config.
// Config must have "endpoint" (URL string). Optional "timeoutMs" (int).
// Creates its own http.Client; prefer NewWebhookForwardWithClient for
// shared pooling (ai-gateway swaps in a shared client via Registry.Replace).
func NewWebhookForward(cfg *core.HookConfig) (core.Hook, error) {
	return NewWebhookForwardWithClient(cfg, nil)
}

// NewWebhookForwardWithClient creates a webhook-forward hook using a shared
// HTTP client. If client is nil a per-hook client is created as fallback.
//
// Config shape:
//
//	{
//	  "endpoint":    "https://...",
//	  "timeoutMs":   5000,
//	  "payloadMode": "redacted"|"full"|"metadata-only",   // default redacted
//	  "onMatch":     {...}                                  // optional ceiling
//	}
func NewWebhookForwardWithClient(cfg *core.HookConfig, client *http.Client) (core.Hook, error) {
	endpoint, _ := cfg.Config["endpoint"].(string)
	if endpoint == "" {
		return nil, fmt.Errorf("webhook-forward: endpoint is required")
	}

	timeout := 5 * time.Second
	if ms, ok := cfg.Config["timeoutMs"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	if client == nil {
		client = nexushttp.New(nexushttp.Config{
			Timeout:        timeout,
			Caller:         "webhook-hook",
			PropagateReqID: true,
		})
	}

	mode := WebhookPayloadRedacted
	if v, ok := cfg.Config["payloadMode"].(string); ok && v != "" {
		switch WebhookPayloadMode(v) {
		case WebhookPayloadFull, WebhookPayloadRedacted, WebhookPayloadMetadataOnly:
			mode = WebhookPayloadMode(v)
		default:
			return nil, fmt.Errorf("webhook-forward: unknown payloadMode %q (expected full|redacted|metadata-only)", v)
		}
	}

	onMatch, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("webhook-forward: %w", err)
	}
	// webhook-forward is uniquely advisory: unlike pii-detector /
	// keyword-filter / content-safety (where "match → block by default" is
	// the right security default), the webhook's reply IS the decision.
	// If the admin did not configure an explicit `inflightAction` value,
	// treat the ceiling as `approve` so the webhook's suggestion flows
	// through without being silently clobbered to RejectHard by
	// ParseOnMatch's block-hard default. Admins who want
	// webhook-bounded-by-ceiling behavior must opt in via an explicit
	// `onMatch.inflightAction`.
	//
	// The gate checks the inner `inflightAction` key (not the outer
	// `onMatch` block) so a partial config like `onMatch: {storageAction:
	// "redact"}` — which leaves `InflightAction` defaulted — still gets
	// the webhook-forward approve-ceiling override.
	if !hasInflightActionConfigured(cfg.Config) {
		onMatch.InflightAction = core.InflightApprove
	}

	return &WebhookForward{
		endpoint:       endpoint,
		timeout:        timeout,
		client:         client,
		payloadMode:    mode,
		onMatch:        onMatch,
		projectionOpts: cfg.ProjectionOptions(),
	}, nil
}

func (w *WebhookForward) Execute(ctx context.Context, input *core.HookInput) (*core.HookResult, error) {
	// Build payload. The envelope is always included; content visibility
	// is controlled by payloadMode.
	payload := map[string]any{
		"stage":       input.Stage,
		"method":      input.Method,
		"path":        input.Path,
		"targetHost":  input.TargetHost,
		"sourceIP":    input.SourceIP,
		"bodySize":    input.BodySize,
		"contentType": input.ContentType,
		"model":       input.Model,
		"ingressType": input.IngressType,
		"payloadMode": string(w.payloadMode),
	}
	switch w.payloadMode {
	case WebhookPayloadFull:
		if input.Normalized != nil {
			payload["normalized"] = input.Normalized
		}
	case WebhookPayloadRedacted:
		if segs := input.TextSegmentsWith(w.projectionOpts); len(segs) > 0 {
			payload["normalizedContent"] = segs
		}
	case WebhookPayloadMetadataOnly:
		// envelope only — no content fields.
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("webhook-forward: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("webhook-forward: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webhook-forward: request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if err != nil {
		return nil, fmt.Errorf("webhook-forward: read response: %w", err)
	}

	// Parse response. Supports two shapes:
	//   - Generic webhook (decision + reason + reasonCode)
	//   - AI-Guard's extended ComplianceWebhookResponse (adds redactions[])
	// Decision string is matched case-insensitively; AI-Guard returns
	// lowercase ("approve", "reject_hard", …) while the legacy contract
	// used uppercase ("REJECT", "BLOCK_SOFT", …) — accept both.
	var result struct {
		Decision   string                 `json:"decision"`
		Reason     string                 `json:"reason"`
		ReasonCode string                 `json:"reasonCode"`
		Redactions []webhookRedactionWire `json:"redactions,omitempty"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return &core.HookResult{Decision: core.Approve, Reason: "webhook response unparseable"}, nil
	}

	suggested := core.Approve
	switch strings.ToLower(strings.TrimSpace(result.Decision)) {
	case "reject", "reject_hard":
		suggested = core.RejectHard
	case "block_soft":
		suggested = core.BlockSoft
	case "modify":
		suggested = core.Modify
	case "abstain":
		suggested = core.Abstain
	}

	// Reconcile the webhook's suggested decision against the admin policy
	// ceiling carried in w.onMatch.InflightAction. The strictest of the
	// two wins so an admin block-hard ceiling cannot be undercut by a
	// permissive webhook reply, and a webhook reject-hard cannot be
	// undercut by a permissive admin ceiling. When the reconciled
	// decision differs from the suggestion, stamp ReasonAIGuardSuggestedVsPolicy
	// and carry both values in Reason so the audit row + UI chip can
	// surface that the override happened.
	//
	// Abstain is the "no opinion" decision; the pipeline aggregator skips
	// abstaining hooks. The reconcile short-circuits on Abstain so the
	// per-hook label stays Abstain (the policy ceiling cannot manufacture
	// an opinion out of a non-opinion).
	reconciled := suggested
	reason := result.Reason
	reasonCode := result.ReasonCode
	if suggested != core.Abstain {
		policyCeiling := core.DecisionForInflight(w.onMatch.InflightAction)
		reconciled = core.StrictestDecision(suggested, policyCeiling)
		if reconciled != suggested {
			reasonCode = core.ReasonAIGuardSuggestedVsPolicy
			// Both sides render in the admin-configured InflightAction
			// vocabulary (block-hard / block-soft / redact / approve) so
			// the audit row + UI chip read in the same language the
			// operator wrote in the hook config, instead of mixing the
			// internal Decision enum (`reject_hard`) with the YAML
			// inflightAction strings (`block-hard`).
			reason = fmt.Sprintf(
				"webhook suggested %s; policy ceiling: %s",
				core.LabelForDecision(suggested),
				string(w.onMatch.InflightAction),
			)
		}
	}

	spans := redactionsToTransformSpans(result.Redactions, w.endpoint)

	return &core.HookResult{
		Decision:       reconciled,
		Reason:         reason,
		ReasonCode:     reasonCode,
		TransformSpans: spans,
	}, nil
}

// hasInflightActionConfigured inspects the raw hook config to determine
// whether the admin supplied an explicit `onMatch.inflightAction` value.
// Returns true only when both the `onMatch` block exists and the
// `inflightAction` key inside it carries a non-empty string. A bare
// `onMatch: {storageAction: ...}` block is treated as "no inflight
// configured" so webhook-forward's approve-ceiling override still fires.
func hasInflightActionConfigured(cfg map[string]any) bool {
	raw, ok := cfg["onMatch"]
	if !ok || raw == nil {
		return false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m["inflightAction"].(string)
	return ok && v != ""
}

// webhookRedactionWire is the on-wire shape of an AI-Guard / generic
// compliance webhook redaction suggestion. Mirrors aiguard.Redaction.
type webhookRedactionWire struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// redactionsToTransformSpans converts webhook-returned redactions into the
// canonical TransformSpan shape consumed by the rest of the pipeline.
//
// Source is SourceHook; SourceID is the webhook endpoint URL so audit
// consumers can trace which webhook produced each span. Action defaults to
// redact when the wire field is missing; explicit "strip" / "inject" /
// "replace" values are honored so the audit record is faithful.
//
// ContentAddress is "webhook.flat" because the wire offsets index a flat
// joined projection that webhook-forward did not construct (the
// compliance-webhook shim on the AI-Guard side builds it). ApplySpans does
// not resolve this sentinel, so spans land in the audit row but do not
// mutate inflight bytes.
func redactionsToTransformSpans(redactions []webhookRedactionWire, endpoint string) []normalize.TransformSpan {
	if len(redactions) == 0 {
		return nil
	}
	out := make([]normalize.TransformSpan, 0, len(redactions))
	for _, r := range redactions {
		action := normalize.ActionRedact
		switch strings.ToLower(strings.TrimSpace(r.Action)) {
		case "redact":
			action = normalize.ActionRedact
		case "strip":
			action = normalize.ActionStrip
		case "inject":
			action = normalize.ActionInject
		case "replace":
			action = normalize.ActionReplace
		}
		out = append(out, normalize.TransformSpan{
			Source:         normalize.SourceHook,
			SourceID:       endpoint,
			Action:         action,
			ContentAddress: "webhook.flat",
			Start:          r.Start,
			End:            r.End,
			Replacement:    r.Replacement,
			Reason:         r.Reason,
		})
	}
	return out
}
