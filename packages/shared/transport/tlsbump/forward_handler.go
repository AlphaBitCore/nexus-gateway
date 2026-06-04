package tlsbump

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// authHeaderSet is a pre-built lookup set of auth-related headers (lowercase
// keys) that are stripped from the compliance pipeline but still forwarded
// to the upstream (they contain credentials). Using a map avoids a linear
// scan on every header of every request.
var authHeaderSet = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
}

// isAuthHeader returns true if the header name is an auth-related header
// that should be stripped from the compliance pipeline.
func isAuthHeader(name string) bool {
	return authHeaderSet[strings.ToLower(name)]
}

// requestAuditKey is a context key carrying the audit context for a request.
type requestAuditKey struct{}

// requestAuditCtx bundles the per-request data needed for post-upstream audit
// emission that is not present in HookInput (transaction/connection IDs, headers).
type requestAuditCtx struct {
	input   *core.HookInput
	info    compliance.AuditInfo
	headers map[string][]string // sanitised request headers (auth stripped)
	// requestBody holds the client-original request bytes captured for
	// audit storage when payload_capture.storeRequestBody is true; nil
	// otherwise. Reused across the post-upstream Emit call sites so the
	// per-request snapshot decision stays stable after the initial read.
	requestBody []byte
	// storeResponseBody mirrors the payload-capture snapshot for this
	// request so the response pipeline path can decide whether to
	// forward response bytes to the audit emitter without re-reading
	// the Store.
	storeResponseBody bool
	// requestPipelineResult is the request-stage hook pipeline outcome
	// (HookResults + Decision + Reason + BlockingRule). Stashed here so
	// post-upstream emit sites — including the SSE path that historically
	// dropped this value — can record the request-stage executions on
	// traffic_event.request_hooks_pipeline. Nil when the request was on
	// the compliance-disabled fast path.
	requestPipelineResult *core.CompliancePipelineResult
	// matchedDomain is the interception_domain row that admitted this
	// request, carrying the per-host StreamingPolicy override columns.
	// Nil for requests on the compliance-disabled fast path. Read by
	// handleSSEResponse so per-host streaming mode + capture flags
	// resolve through shared/streaming/policy.Resolve at request time.
	matchedDomain *domain.InterceptionDomain
	// adapter is the traffic adapter resolved from matchedDomain.AdapterID
	// at request time. Reused on the response path (DetectResponseUsage,
	// ExtractResponse) so the same instance handles both halves of the
	// request/response pair. Nil when no adapter matched or complianceEnabled
	// is false.
	adapter traffic.Adapter
}

// domainName returns the matched domain's name for log fields, or the
// empty string when no domain matched (defensive — host should already
// have been admitted at CONNECT).
func domainName(d *domain.InterceptionDomain) string {
	if d == nil {
		return ""
	}
	return d.Name
}

// buildForwardHandler returns an http.Handler that rewrites each request's
// URL to target the upstream and copies the response back to the client.
// When compliance deps are provided via bumpOptions, the handler runs
// request hooks before upstream and response/SSE hooks after upstream.
func buildForwardHandler(
	ctx context.Context,
	targetHost string,
	upstream *UpstreamTransport,
	logger *slog.Logger,
	bo *bumpOptions,
) http.Handler {
	complianceEnabled := bo != nil && bo.policyResolver != nil

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestStart := time.Now()

		// Thread the captured ClientHello bytes into the request context so
		// UpstreamTransport.DialTLSContext can replay the client's fingerprint.
		if bo != nil && len(bo.clientHelloRaw) > 0 {
			r = r.WithContext(context.WithValue(r.Context(), clientHelloKey{}, bo.clientHelloRaw))
		}

		// Peek the X-Nexus-Attestation header BEFORE any compliance
		// machinery spins up. When the agent signs the inner request and
		// the verifier accepts, this CONNECT is pure passthrough — skip
		// the entire compliance pipeline AND audit emission AND payload
		// capture, just forward the request upstream and stream the
		// response back. CP becomes transparent on agent-attested
		// traffic; the agent's own audit row is the system-of-record.
		//
		// The verifier is fail-open — invalid / missing / replayed all
		// return false, and we fall through to the normal MITM path.
		// The verifier increments nexus_cp_attestation_total{outcome=...}
		// per call so the operational metric stays consistent regardless
		// of which branch fires.
		if bo != nil && bo.attestationVerifier != nil {
			if valid, agentID := bo.attestationVerifier(r.Context(), r.Header.Get(AttestationHeaderName)); valid {
				logger.Info("attestation verified — passthrough (no hooks, no audit)",
					"target", targetHost,
					"agent_id", agentID,
					"method", r.Method,
					"path", r.URL.Path,
				)
				attestationPassthrough(w, r, upstream, logger)
				return
			}
		}

		// A per-request PhaseSink that the shared/traffic tracing
		// transport (wrapped around UpstreamTransport in upstream.go)
		// populates with upstream TTFB and upstream-total. The same
		// pointer is stamped onto AuditInfo so buildEvent can read it
		// at emit time across every Emit / EmitDual call site.
		phaseSink := traffic.NewPhaseSink()
		r = r.WithContext(traffic.WithPhaseSink(r.Context(), phaseSink))
		// Stamp conn_setup_ms (cheap server-side bookkeeping) and
		// — on the FIRST request of this bumped tunnel only —
		// tls_handshake_ms (sourced from BumpConnection). Subsequent
		// keep-alive requests on the same bo skip the handshake stamp
		// via sync.Once so we don't double-count it across the row set.
		phaseBreakdown := map[string]int{}
		connSetupStart := time.Now()
		if bo != nil {
			bo.tlsHandshakeOnce.Do(func() {
				if bo.tlsHandshakeMs > 0 {
					phaseBreakdown["tls_handshake_ms"] = bo.tlsHandshakeMs
				}
			})
		}

		// Snapshot the payload-capture config once per request so the
		// read cap and the capture gates agree across all downstream
		// emit call-sites, even if the admin invalidates mid-request.
		pcCfg := payloadcapture.DefaultConfig()
		if bo != nil && bo.payloadCaptureStore != nil {
			pcCfg = bo.payloadCaptureStore.Get()
		}

		// Use client-supplied correlation ID if present; otherwise generate one.
		// X-Nexus-Request-Id is the single canonical correlation header — it
		// doubles as the cross-service trace id (seeded by the agent for
		// intercepted flows, generated here for direct proxy traffic) and is
		// forwarded to ai-gateway so its audit records share the same id.
		txID := r.Header.Get("X-Nexus-Request-Id")
		if txID == "" {
			txID = uuid.NewString()
		}
		// Set the header on the outgoing request so upstream services can correlate.
		r.Header.Set("X-Nexus-Request-Id", txID)
		traceID := txID

		// Rewrite the URL to point to the upstream.
		r.URL.Scheme = "https"
		r.URL.Host = targetHost

		// Resolve the matched InterceptionDomain (priority-ordered) +
		// per-path action. BLOCK rejects the request before any work;
		// PASSTHROUGH skips the compliance pipeline for this request
		// only by shadowing the closure-captured complianceEnabled
		// with a request-local override; PROCESS leaves the existing
		// flow untouched. The matched domain's NetworkZone is captured
		// for audit tagging downstream.
		complianceEnabled := complianceEnabled // request-local shadow
		logger.Info("[DBG] request entry",
			"method", r.Method,
			"path", r.URL.Path,
			"host", r.Host,
			"target", targetHost,
			"complianceEnabled", complianceEnabled,
			"contentType", r.Header.Get("Content-Type"),
			"txID", txID,
		)
		var matchedDomain *domain.InterceptionDomain
		var matchedZone string
		// Lift pathAction + domainRuleID to outer scope so the audit-
		// emitter call sites stamp them onto every audit_event row.
		// The agent UI's classify() reads these to distinguish Inspect
		// (matched + PASSTHROUGH) from Processed (matched + PROCESS +
		// hooks ran).
		var resolvedPathAction domain.PathAction
		var domainRuleID string
		if bo != nil && bo.domainEngine != nil {
			matchedDomain = bo.domainEngine.MatchHost(r.Host)
			if matchedDomain == nil {
				host, _, _ := net.SplitHostPort(targetHost)
				if host == "" {
					host = targetHost
				}
				matchedDomain = bo.domainEngine.MatchHost(host)
			}
			resolvedPathAction = bo.domainEngine.PathAction(matchedDomain, r.URL.Path)
			if matchedDomain != nil {
				matchedZone = string(matchedDomain.NetworkZone)
				// Capture domainRuleID unconditionally on match — even when
				// no adapter resolves later. Without this the audit row
				// shows DomainRuleID="" and the UI mislabels the row as
				// Untracked.
				domainRuleID = matchedDomain.ID
			}
			logger.Info("[DBG] domain/path resolved",
				"matchedDomain", domainName(matchedDomain),
				"pathAction", resolvedPathAction,
				"complianceEnabledAfter", complianceEnabled,
				"txID", txID,
			)
			switch resolvedPathAction {
			case domain.PathActionBlock:
				logger.Warn("request blocked by interception path policy",
					"target", targetHost,
					"path", r.URL.Path,
					"transactionId", txID,
					"domain", domainName(matchedDomain),
					"networkZone", matchedZone,
					"action", "BLOCK",
				)
				http.Error(w, "Request blocked by compliance policy", http.StatusForbidden)
				return
			case domain.PathActionPassthrough:
				logger.Info("request passthrough — compliance hooks skipped by path policy",
					"target", targetHost,
					"path", r.URL.Path,
					"transactionId", txID,
					"domain", domainName(matchedDomain),
					"networkZone", matchedZone,
					"action", "PASSTHROUGH",
				)
				complianceEnabled = false
			}
		}
		_ = matchedZone // network_zone audit tagging lands in a follow-up

		// Classify the endpoint from (method, path) so the compliance
		// pipeline can apply endpoint-aware hook filtering. Falls back
		// to empty when no rule matches — all hooks run on unclassified
		// endpoints. E87-S3a-1 (2026-05-25): direct typology call,
		// removing the classify.Classifier injection seam (and its
		// WithEndpointClassifier option).
		endpointType, _, _ := typology.ClassifyPath(r.Method, r.URL.Path)

		// Pre-upstream compliance (request hooks)
		//
		// reqHookResult is populated inside the compliance block when a
		// domain rule matches and the hook pipeline runs. Used below to
		// build the CPMarker that downstream response writers
		// (upstream.go Task 3.2, sse.go Task 3.3) read via CPMarkerFromContext.
		var reqHookResult *core.CompliancePipelineResult
		if complianceEnabled {
			// Read request body for both compliance inspection and upstream forwarding.
			bodyBytes, err := readBody(r, pcCfg.MaxRequestBytes)
			if err != nil {
				logger.Error("failed to read request body for compliance",
					"target", targetHost,
					"error", err,
				)
				// Continue without compliance rather than blocking the request.
			} else {
				// Collect sanitised headers (auth stripped) for audit User-Agent extraction.
				sanitisedHeaders := copyHeadersStrippingAuth(r.Header)

				// Extract and normalize content via domain-specific adapter.
				// Run the request-side LLM signal detector on the same adapter
				// so provider/model/api-key class are stamped onto the hook
				// input before any hook sees the request.
				var content *normalize.NormalizedPayload
				var reqMeta traffic.RequestMeta
				var resolvedAdapter traffic.Adapter
				if bo.adapterRegistry != nil && matchedDomain != nil && matchedDomain.AdapterID != "" {
					if factory := bo.adapterRegistry.Get(matchedDomain.AdapterID); factory != nil {
						resolvedAdapter = factory()
						// (domainRuleID was set at the outer scope on
						// matchedDomain != nil; no need to reassign here.)
						// Hot-path normalize: if the adapter implements
						// normalize.Normalizer, call it directly to get a
						// structured NormalizedPayload with role-aware
						// Messages. Falls back to the legacy ExtractRequest →
						// Segments → PayloadFromTextSegments chain for adapters
						// not yet on Normalize (cursor's gRPC-protobuf path) or
						// for adapters whose probe rejected the body (returns
						// ErrUnsupported when confidence falls below the
						// per-adapter threshold).
						content = runtimeNormalize(r.Context(), bo.normalizeRegistry, resolvedAdapter, bodyBytes, r.URL.Path, r.Header.Get("Content-Type"), normalize.DirectionRequest, logger, txID)
						reqMeta = resolvedAdapter.DetectRequestMeta(r, bodyBytes)
					}
				}

				reqInput := &core.HookInput{
					Stage:             "request",
					Normalized:        content,
					SourceIP:          bo.sourceIP,
					TargetHost:        targetHost,
					Method:            r.Method,
					Path:              r.URL.Path,
					IngressType:       "COMPLIANCE_PROXY",
					BodySize:          int64(len(bodyBytes)),
					ContentType:       r.Header.Get("Content-Type"),
					DetectedProvider:  reqMeta.Provider,
					DetectedModel:     reqMeta.Model,
					ApiKeyClass:       reqMeta.ApiKeyClass,
					ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
					EndpointType:      endpointType,
				}

				// Finalize conn_setup_ms = elapsed from handler entry to the
				// audit info build (covers CONNECT parse + header sanitize +
				// reqMeta detection). tls_handshake_ms was set earlier in
				// phaseBreakdown via tlsHandshakeOnce.
				if connSetupMs := int(time.Since(connSetupStart).Milliseconds()); connSetupMs > 0 {
					phaseBreakdown["conn_setup_ms"] = connSetupMs
				}
				auditInfo := compliance.AuditInfo{
					TransactionID:    txID,
					ConnectionID:     bo.connectionID,
					TraceID:          traceID,
					Headers:          sanitisedHeaders,
					RequestMeta:      reqMeta,
					PhaseSink:        phaseSink,
					LatencyBreakdown: phaseBreakdown,
					// Stamp classification inputs so the agent audit row
					// carries enough context for classify() to distinguish
					// Inspect / Processed / Blocked / Bump failed /
					// Untracked at query time.
					DomainRuleID: domainRuleID,
					PathAction:   string(resolvedPathAction),
					// Stamp originating process attribution so admin Traffic
					// UI's App column populates for inspect rows. cp callers
					// leave the procName/Bundle/User strings empty so the
					// audit row stays unchanged for cp-originated traffic.
					SourceProcess:       bo.procName,
					SourceProcessBundle: bo.procBundle,
					SourceUser:          bo.procUser,
				}
				// Stamp request-side normalize result (computed above by
				// runtimeNormalize) so agent SQLite + Hub MQ wire both
				// carry the pre-computed NormalizedPayload. Falls back to
				// empty when content is nil (non-AI adapter / parse miss).
				if content != nil {
					if b, err := json.Marshal(content); err == nil {
						auditInfo.RequestNormalized = b
					}
				}

				// Build and run request pipeline. Pass endpointType so
				// endpoint-aware hooks (e.g. embedding-specific or chat-only
				// hooks) apply correctly. Empty string when unclassified —
				// all hooks that SupportsEndpoint("") are included.
				reqPipeline, pErr := bo.policyResolver.BuildPipeline(
					"request", "COMPLIANCE_PROXY",
					endpointType, nil,
					bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks, logger,
				)
				if pErr != nil {
					logger.Error("failed to build request pipeline",
						"target", targetHost,
						"transactionId", txID,
						"error", pErr,
					)
				} else if reqPipeline != nil {
					reqPipeline.SetClearSoftOnApprove(true)
					result := reqPipeline.Execute(ctx, reqInput)
					// Capture the result so the CPMarker below can convert it
					// into a HookOutcomeInput (used in Tasks 3.2/3.3).
					reqHookResult = result

					switch result.Decision {
					case compliance.RejectHard:
						logger.Info("request blocked by compliance (REJECT_HARD)",
							"target", targetHost,
							"transactionId", txID,
							"reason", result.Reason,
						)
						if bo.auditEmitter != nil {
							bo.auditEmitter.Emit(reqInput, auditInfo, result, "BUMP_SUCCESS", http.StatusForbidden, int(time.Since(requestStart).Milliseconds()), captureBodyIfEnabled(pcCfg.StoreRequestBody, bodyBytes), nil, traffic.UsageMeta{})
						}
						stampRejectMarkers(w.Header(), bo.identity, txID, domainRuleID, cpHookOutcomeFromResult(result))
						WriteRejectResponse(w, r, bo.rejectConfig, txID, result.Reason, result.ReasonCode, http.StatusForbidden)
						return

					case compliance.BlockSoft:
						logger.Info("request soft-rejected by compliance (BLOCK_SOFT)",
							"target", targetHost,
							"transactionId", txID,
							"reason", result.Reason,
						)
						if bo.auditEmitter != nil {
							bo.auditEmitter.Emit(reqInput, auditInfo, result, "BUMP_SUCCESS", 246, int(time.Since(requestStart).Milliseconds()), captureBodyIfEnabled(pcCfg.StoreRequestBody, bodyBytes), nil, traffic.UsageMeta{})
						}
						w.WriteHeader(246)
						_, _ = fmt.Fprintf(w, "Request flagged by policy: %s", result.Reason)
						return

					case compliance.Modify:
						// Hook requested inflight redact. Try to rewrite the
						// upstream body via the resolved adapter. If the adapter
						// declares ErrRewriteUnsupported, fall back to
						// "upstream sees original, audit log stores spans" and
						// stamp REDACT_INFLIGHT_UNSUPPORTED on the result so the
						// audit trail reflects the degraded path.
						if resolvedAdapter != nil && len(result.ModifiedContent) > 0 {
							rewriteContent := contentBlocksToNormalized(result.ModifiedContent)
							rewritten, _, rErr := resolvedAdapter.RewriteRequestBody(ctx, bodyBytes, r.URL.Path, rewriteContent)
							switch {
							case errors.Is(rErr, traffic.ErrRewriteUnsupported):
								logger.Warn("inflight rewrite unsupported; forwarding original body",
									"target", targetHost,
									"transactionId", txID,
									"adapter", resolvedAdapter.ID(),
								)
								result.ReasonCode = core.ReasonRedactInflightUnsupported
							case rErr != nil:
								logger.Error("inflight rewrite failed",
									"target", targetHost,
									"transactionId", txID,
									"error", rErr,
								)
								result.ReasonCode = core.ReasonRedactInflightUnsupported
							default:
								bodyBytes = rewritten
								logger.Info("request body redacted by compliance hook",
									"target", targetHost,
									"transactionId", txID,
								)
							}
						}
						// MODIFY does NOT short-circuit upstream; fall through.
					}

					// APPROVE / ABSTAIN / MODIFY-handled — continue to upstream.
				}

				// Restore the request body for upstream forwarding since we consumed it.
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				r.ContentLength = int64(len(bodyBytes))

				// Store the request audit context for post-upstream use.
				// requestPipelineResult is reqHookResult so the SSE / non-SSE
				// post-upstream emit can record request-stage executions on
				// traffic_event.request_hooks_pipeline.
				r = r.WithContext(context.WithValue(r.Context(), requestAuditKey{}, &requestAuditCtx{
					input:                 reqInput,
					info:                  auditInfo,
					headers:               sanitisedHeaders,
					requestBody:           captureBodyIfEnabled(pcCfg.StoreRequestBody, bodyBytes),
					storeResponseBody:     pcCfg.StoreResponseBody,
					requestPipelineResult: reqHookResult,
					matchedDomain:         matchedDomain,
					adapter:               resolvedAdapter,
				}))
			}
		}

		// Stash the per-request marker state on the context so that downstream
		// response write sites (upstream.go Task 3.2, sse.go Task 3.3) can inject
		// x-nexus-cp-* headers without re-deriving these values. The marker is
		// always set — even on the compliance-disabled fast path — so callers
		// never need to handle a nil check for the basic request-id field.
		r = r.WithContext(contextWithCPMarker(r.Context(), &CPMarker{
			RequestID:    txID,
			DomainRuleID: domainRuleID,
			HookOutcome:  cpHookOutcomeFromResult(reqHookResult),
		}))

		// Forward to upstream
		// Use r.Context() (not the connection-level ctx) so the clientHelloKey
		// value flows through to UpstreamTransport.DialTLSContext.
		var responseBlocked bool
		resp, err := upstream.ForwardRequest(r.Context(), r)
		if err != nil {
			logger.Error("upstream request failed",
				"target", targetHost,
				"method", r.Method,
				"path", r.URL.Path,
				"error", err,
			)
			// Emit audit for failed upstream if compliance enabled.
			if complianceEnabled {
				if audCtx, ok := r.Context().Value(requestAuditKey{}).(*requestAuditCtx); ok && audCtx != nil {
					if bo.auditEmitter != nil {
						approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
						// Upstream failed before any body — no usage available.
						usage := traffic.UsageMeta{Status: traffic.UsageStatusNoBody}
						bo.auditEmitter.Emit(audCtx.input, audCtx.info, approveResult, "BUMP_SUCCESS", http.StatusBadGateway, int(time.Since(requestStart).Milliseconds()), audCtx.requestBody, nil, usage)
					}
				}
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}

		// Post-upstream compliance (response / SSE hooks)
		if complianceEnabled {
			audCtx, _ := r.Context().Value(requestAuditKey{}).(*requestAuditCtx)
			contentType := resp.Header.Get("Content-Type")
			// Stamp the upstream response Content-Type onto the shared
			// audit info so every downstream Emit / EmitDual call in
			// this branch (SSE handler, buffered AI path, fast-path)
			// hands a truthful CT to spillstore.EmitBody. Without this,
			// Hub-side normalization must guess the body shape from raw
			// bytes.
			if audCtx != nil {
				audCtx.info.ResponseContentType = contentType
			}
			// Connect-RPC streaming (application/connect+proto|json) uses the same
			// streaming passthrough path as SSE: we must not buffer the full body
			// with io.ReadAll or the client times out waiting for the first byte.
			isSSE := strings.Contains(contentType, "text/event-stream") ||
				strings.Contains(contentType, "application/connect+proto") ||
				strings.Contains(contentType, "application/connect+json")

			logger.Info("[DBG] post-upstream",
				"path", r.URL.Path,
				"statusCode", resp.StatusCode,
				"contentType", contentType,
				"isSSE", isSSE,
				"audCtxNil", audCtx == nil,
				"txID", txID,
			)

			if isSSE {
				// Build a response HookInput for SSE processing.
				var respInput *core.HookInput
				if audCtx != nil {
					respInput = &core.HookInput{
						Stage:        "response",
						SourceIP:     audCtx.input.SourceIP,
						TargetHost:   audCtx.input.TargetHost,
						Method:       audCtx.input.Method,
						Path:         audCtx.input.Path,
						IngressType:  audCtx.input.IngressType,
						ContentType:  contentType,
						EndpointType: endpointType,
					}
				}
				// Route to streaming pipeline.
				var auditInfo *compliance.AuditInfo
				if audCtx != nil {
					ai := audCtx.info
					auditInfo = &ai
				}
				// Use r.Context() (not the connection-level ctx) so the SSE
				// handler's stampMarkers can read the CPMarker injected by
				// contextWithCPMarker — needed for request-id, mode, hook,
				// and domain-rule headers.
				handleSSEResponse(r.Context(), w, resp, audCtx, respInput, auditInfo, bo, logger, requestStart)
				return
			}

			// Non-SSE response: run response pipeline if hooks exist.
			// Reuse the endpointType classified at request time so the
			// response pipeline applies the same endpoint-aware filtering.
			if audCtx != nil {
				respPipeline, pErr := bo.policyResolver.BuildPipeline(
					"response", "COMPLIANCE_PROXY",
					endpointType, nil,
					bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks, logger,
				)
				providerDetected := audCtx.info.RequestMeta.Provider != ""
				// Buffer the response body when either a response hook is
				// configured OR the request was detected as AI traffic (we
				// need the body to extract usage tokens). Non-AI traffic
				// with no hooks stays on the stream-through fast path.
				needBuffer := respPipeline != nil || providerDetected

				//nolint:gocritic // ifElseChain: the three arms (pipeline-build error / buffered AI path / stream-through fast path) each carry distinct ~50-line bodies; flattening to switch hurts readability without removing nesting.
				if pErr != nil {
					logger.Warn("failed to build response pipeline",
						"target", targetHost,
						"transactionId", audCtx.info.TransactionID,
						"error", pErr,
					)
					// Emit audit with approve.
					if bo.auditEmitter != nil {
						approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
						bo.auditEmitter.Emit(audCtx.input, audCtx.info, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(requestStart).Milliseconds()), audCtx.requestBody, nil, traffic.UsageMeta{})
					}
				} else if needBuffer {
					// Read response body so we can (a) run response hooks if
					// any, and/or (b) extract LLM usage via the adapter.
					respBody, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if readErr != nil {
						logger.Error("failed to read response body for compliance",
							"target", targetHost,
							"error", readErr,
						)
						// Restore an empty body so copyResponse doesn't read from closed reader.
						resp.Body = io.NopCloser(bytes.NewReader(nil))
						// Emit audit with approve (best-effort: body unreadable, let through).
						if bo.auditEmitter != nil {
							approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
							bo.auditEmitter.Emit(audCtx.input, audCtx.info, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(requestStart).Milliseconds()), audCtx.requestBody, nil, traffic.UsageMeta{Status: traffic.UsageStatusNoBody})
						}
					} else {
						// Decompress once before normalize / usage / capture.
						// Go's http.Transport only auto-decompresses gzip;
						// some origins ship brotli (br) or zstd-encoded SSE,
						// so respBody after io.ReadAll may be compressed bytes.
						// decompressForCapture is idempotent — respBody stays
						// the original compressed bytes for copyResponse so the
						// client receives the encoding it requested.
						decompressedBody := decompressForCapture(respBody, resp)
						// Extract usage signals on the AI path. Done once per
						// request on the already-buffered body.
						var usage traffic.UsageMeta
						var respContent *normalize.NormalizedPayload
						if adapter := audCtx.adapter; adapter != nil {
							if providerDetected {
								usage = adapter.DetectResponseUsage(resp, decompressedBody)
							}
							// Hot-path normalize (response side): prefer
							// adapter.Normalize for structured Messages when the
							// adapter implements normalize.Normalizer; fall back
							// to the legacy ExtractResponse → Segments chain for
							// adapters not yet on Normalize.
							respContent = runtimeNormalize(r.Context(), bo.normalizeRegistry, adapter, decompressedBody, r.URL.Path, contentType, normalize.DirectionResponse, logger, audCtx.info.TransactionID)
						}

						var respResult *core.CompliancePipelineResult
						if respPipeline != nil {
							respInput := &core.HookInput{
								Stage:             "response",
								Normalized:        respContent,
								SourceIP:          audCtx.input.SourceIP,
								TargetHost:        audCtx.input.TargetHost,
								Method:            audCtx.input.Method,
								Path:              audCtx.input.Path,
								IngressType:       audCtx.input.IngressType,
								BodySize:          int64(len(respBody)),
								ContentType:       contentType,
								DetectedProvider:  audCtx.info.RequestMeta.Provider,
								DetectedModel:     audCtx.info.RequestMeta.Model,
								ApiKeyClass:       audCtx.info.RequestMeta.ApiKeyClass,
								ApiKeyFingerprint: audCtx.info.RequestMeta.ApiKeyFingerprint,
								EndpointType:      endpointType,
							}
							respPipeline.SetClearSoftOnApprove(true)
							respResult = respPipeline.Execute(ctx, respInput)
						} else {
							respResult = &core.CompliancePipelineResult{Decision: compliance.Approve}
						}

						if bo.auditEmitter != nil {
							// Reuse the already-decompressed body; calling
							// decompressForCapture again would be redundant.
							captureBody := decompressedBody
							// Stamp response-side normalize result onto
							// audCtx.info before emit so agent SQLite + Hub
							// MQ wire both carry the pre-computed response
							// NormalizedPayload (mirrors the request-side stamp).
							if respContent != nil {
								if b, err := json.Marshal(respContent); err == nil {
									audCtx.info.ResponseNormalized = b
								}
							}
							bo.auditEmitter.Emit(audCtx.input, audCtx.info, respResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(requestStart).Milliseconds()), audCtx.requestBody, captureBodyIfEnabled(audCtx.storeResponseBody, captureBody), usage)
						}

						// If a response hook hard-rejects, return HTTP 451 to the client
						// instead of forwarding the upstream response. The response body has
						// already been buffered, so we can safely suppress it and write an
						// error response in its place.
						if respResult.Decision == compliance.RejectHard {
							logger.Info("response blocked by compliance (REJECT_HARD)",
								"target", targetHost,
								"transactionId", audCtx.info.TransactionID,
								"reason", respResult.Reason,
							)
							stampRejectMarkers(w.Header(), bo.identity, audCtx.info.TransactionID, domainRuleID, cpHookOutcomeFromResult(respResult))
							WriteRejectResponse(w, r, bo.rejectConfig, audCtx.info.TransactionID,
								respResult.Reason, respResult.ReasonCode, http.StatusUnavailableForLegalReasons)
							responseBlocked = true
							resp.Body = io.NopCloser(bytes.NewReader(nil))
						} else {
							resp.Body = io.NopCloser(bytes.NewReader(respBody))
						}
					}
				} else {
					// Non-AI traffic with no response hooks — stream through.
					// Emit audit with non_llm usage status. No response body
					// buffered on this fast path, so ResponseBody stays nil
					// regardless of the capture flag.
					//nolint:gocritic // elseif: the comment block above documents the entire branch ("non-AI fast path"), not just the inner auditEmitter check; flattening to `else if` would orphan that documentation.
					if bo.auditEmitter != nil {
						approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
						bo.auditEmitter.Emit(audCtx.input, audCtx.info, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(requestStart).Milliseconds()), audCtx.requestBody, nil, traffic.UsageMeta{Status: traffic.UsageStatusNonLLM})
					}
				}
			}
		}

		if !responseBlocked {
			if err := copyResponse(w, resp, markerHook(r.Context(), bo.identity)); err != nil {
				logger.Error("failed to copy upstream response",
					"target", targetHost,
					"method", r.Method,
					"path", r.URL.Path,
					"error", err,
				)
				// Response may be partially written; nothing more we can do.
			}
		}
	})
}

// captureBodyIfEnabled returns the body slice when the corresponding
// capture flag is true and the slice is non-empty; otherwise nil. The
// returned slice is a reference to the caller's bytes — callers must not
// mutate it after handing it off to the audit emitter. We intentionally
// store the pre-hook bytes ("what the caller sent") since CP's request
// pipeline runs with allowModify=false and cannot rewrite the body.
func captureBodyIfEnabled(enabled bool, body []byte) []byte {
	if !enabled || len(body) == 0 {
		return nil
	}
	return body
}
