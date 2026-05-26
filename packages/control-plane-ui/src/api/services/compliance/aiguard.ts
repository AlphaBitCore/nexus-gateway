/**
 * AI Guard API service — typed functions for the admin AI Guard endpoints
 * (single-instance config + dry-run classify probe). Backed by the Go
 * `/api/admin/ai-guard/*` handlers introduced by P-B Tasks 23-24 and the
 * `/v1/ai-guard/classify` endpoint contract from P-B Task 21 (see
 * `docs/users/api/openapi/e27-s01-ai-guard.yaml`).
 */
import { api } from '../../client';

/**
 * Backend dispatch mode for the AI Guard classifier.
 * - `configured_provider` — reuse an existing registered provider + model
 *   (goes through the same provider-adapter stack as user traffic).
 * - `external_url` — call a plain-HTTP OpenAI-compatible endpoint with
 *   optional headers and a separately-managed credential.
 */
export type AIGuardBackendMode = 'configured_provider' | 'external_url';

/** Singleton config record stored server-side as `ai_guard_config`. */
export interface AIGuardConfig {
  id: string;
  backendMode: AIGuardBackendMode;
  providerId?: string | null;
  modelId?: string | null;
  externalUrl?: string | null;
  externalCredentialId?: string | null;
  customHeaders?: Record<string, string> | null;
  promptTemplate: string;
  timeoutMs: number;
  cacheTtlSeconds: number;
  /** sha256 fingerprint of the effective backend wiring — caches invalidate on change. */
  backendFingerprint: string;
}

/** Ingress codes recognised by the classifier context. */
export type ClassifyIngress = 'AI_GATEWAY' | 'COMPLIANCE_PROXY' | 'AGENT';

export interface ClassifyContext {
  ingress?: ClassifyIngress;
  target_provider?: string;
  target_model?: string;
  upstream_tags?: string[];
  hook_name?: string;
}

export interface ClassifyRequest {
  detector_type: string;
  content: string;
  context?: ClassifyContext;
}

export type ClassifyDecision = 'approve' | 'reject_hard' | 'block_soft' | 'modify';

export interface ClassifyMetadata {
  judge_model?: string;
  judge_latency_ms: number;
  cache_hit: boolean;
  backend_mode?: string;
}

/**
 * One structured redaction suggestion from the judge. Offsets reference
 * UTF-8 byte positions in `ClassifyRequest.content`.
 */
export interface ClassifyRedaction {
  start: number;
  end: number;
  replacement?: string;
  action?: 'redact' | 'strip' | 'replace';
  reason?: string;
}

export interface ClassifyResponse {
  decision: ClassifyDecision;
  confidence?: number;
  reason?: string;
  labels?: string[];
  redactions?: ClassifyRedaction[];
  metadata: ClassifyMetadata;
}

/** Admin dry-run envelope: echoes the normalised request + classifier response. */
export interface DryRunResult {
  request: ClassifyRequest;
  response: ClassifyResponse;
}

export const aiGuardApi = {
  getConfig: () => api.get<AIGuardConfig>('/api/admin/ai-guard/config'),

  saveConfig: (cfg: Partial<AIGuardConfig>) =>
    api.put<AIGuardConfig>('/api/admin/ai-guard/config', cfg),

  dryRun: (req: ClassifyRequest) =>
    api.post<DryRunResult>('/api/admin/ai-guard/dry-run', req),
};
