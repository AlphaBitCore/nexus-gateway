/**
 * Semantic Cache Negative-Feedback API service.
 *
 * Endpoint: POST /api/admin/cache/semantic-feedback
 * IAM: requires admin:semantic-cache.update
 *
 * Marks a cache entry as poisoned so the gateway treats future KNN hits
 * against it as misses.
 */
import { api } from '../../client';

export interface PostFeedbackInput {
  /** The cache entry key (typically the traffic_event id for semantic hits). */
  entryKey: string;
  /**
   * VK scope string that isolates the cache entry (e.g. "v1:vk:nvk_xxx").
   * Omit or pass empty string when the entry is not VK-scoped.
   */
  vkScope: string;
  /** Admin-provided reason for marking the entry as bad (5–500 chars). */
  reason: string;
}

export interface PostFeedbackResponse {
  ok: boolean;
}

export interface FeedbackEntry {
  entryKey: string;
  vkScope: string;
  reason: string;
  actorId: string;
  createdAt: string;
}

export interface ListFeedbackResponse {
  entries: FeedbackEntry[];
}

export const semanticFeedbackApi = {
  /**
   * POST /api/admin/cache/semantic-feedback
   *
   * Poisons the given entry key so the gateway treats future semantic matches
   * against it as misses. Emits an admin audit row server-side.
   */
  postFeedback: (input: PostFeedbackInput): Promise<PostFeedbackResponse> =>
    api.post<PostFeedbackResponse>('/api/admin/cache/semantic-feedback', input),

  /**
   * GET /api/admin/cache/semantic-feedback?limit=N
   *
   * Returns the most recent N "Mark as bad cache hit" submissions from the
   * Control Plane's in-process ring buffer (capped server-side at 1000).
   * Used by the Cache Settings page "Recent feedback" panel so admins have
   * a single place to audit which L2 hits were flagged + why.
   */
  listFeedback: (limit = 100): Promise<ListFeedbackResponse> =>
    api.get<ListFeedbackResponse>(`/api/admin/cache/semantic-feedback?limit=${limit}`),
};
