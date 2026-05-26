/**
 * Semantic Cache Pre-warm API service.
 *
 * Endpoint:
 *   POST /api/admin/semantic-cache/prewarm
 */
import { api } from '../../client';

/** A single corpus entry to embed and write into the L2 semantic cache. */
export interface PrewarmEntry {
  /** The user prompt / question text. Used as the embedding source. */
  prompt: string;
  /** The assistant response body to store in the L2 HSET. */
  response: string;
  /**
   * Optional model discriminator (e.g. "gpt-4o"). When provided, scopes the
   * cache entry to that model so other models cannot match it.
   */
  model?: string;
  /**
   * Optional VK scope (e.g. "v1:vk:nvk_xxx"). Scopes the entry to a specific
   * virtual-key audience. Omit to make the entry matchable by any VK.
   */
  vkScope?: string;
  /**
   * TTL in seconds for the L2 cache entry.
   * Must be in [60, 604800] (1 minute to 7 days).
   * @default 86400
   */
  ttlSeconds?: number;
}

/** Input shape for the pre-warm request. */
export interface PrewarmInput {
  /** Corpus entries to embed + write. Maximum 500 entries per request. */
  entries: PrewarmEntry[];
  /**
   * When true, performs embeddings but skips the HSET write.
   * Use for cost estimation / validation before a real import.
   * @default false
   */
  dryRun?: boolean;
}

/** Result shape returned by POST /api/admin/semantic-cache/prewarm. */
export interface PrewarmResult {
  /** Number of entries successfully written to L2 (0 when dryRun=true). */
  written: number;
  /** Number of entries skipped (embedding error, dry-run, etc.). */
  skipped: number;
  /** Total embedding API calls made (coalesced calls counted once). */
  embeddingsCalls: number;
  /** Accumulated embedding cost in USD for this batch. */
  embeddingCostUsd: number;
  /** Total wall-clock duration of the pre-warm operation in milliseconds. */
  durationMs: number;
  /**
   * Per-entry results. Always present and same length as input entries.
   * Useful for identifying which entries failed in a partial-success batch.
   */
  results?: PrewarmEntryResult[];
}

/** Per-entry outcome in a pre-warm response. */
export interface PrewarmEntryResult {
  /** Zero-based index matching the input entries array. */
  index: number;
  /** Whether this entry was written to L2 (false when dryRun or skipped). */
  stored: boolean;
  /**
   * Non-empty string when stored=false, describing why the entry was skipped.
   * Values: "dry_run", "embedding_error", "l2_disabled", "skip_<reason>" etc.
   */
  skipReason?: string;
  /** Embedding error message when the embedding call failed for this entry. */
  error?: string;
}

/**
 * Sends a pre-warm corpus to the admin semantic-cache prewarm endpoint.
 * IAM: requires admin:semantic-cache.update.
 */
export function prewarm(input: PrewarmInput): Promise<PrewarmResult> {
  return api.post<PrewarmResult>('/api/admin/semantic-cache/prewarm', input);
}
