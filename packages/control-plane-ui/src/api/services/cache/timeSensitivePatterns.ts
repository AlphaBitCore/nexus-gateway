/**
 * Time-Sensitive Freshness Patterns API service.
 *
 * Endpoints:
 *   GET    /api/admin/cache/time-sensitive-patterns
 *   PUT    /api/admin/cache/time-sensitive-patterns/:id
 *   POST   /api/admin/cache/time-sensitive-patterns
 *   DELETE /api/admin/cache/time-sensitive-patterns/:id
 *   POST   /api/admin/cache/time-sensitive-patterns/test
 *
 * queryKey conventions:
 *   ['admin', 'cache', 'time-sensitive-patterns']
 *   ['admin', 'cache', 'time-sensitive-patterns', 'test']
 */
import { api } from '../../client';

/** Mirrors TimeSensitivePattern from the backend (time_sensitive.go). */
export interface TimeSensitivePattern {
  id: string;
  keywords: string[];
  requireQuestionMark: boolean;
  requireEntity: boolean;
  languages: string[];
  enabled: boolean;
}

/** Response from GET /api/admin/cache/time-sensitive-patterns. */
export interface TimeSensitivePatternsResponse {
  patterns: TimeSensitivePattern[];
  /** "seed" = built-in default; "shadow" = admin-pushed override active. */
  source: 'seed' | 'shadow';
}

/** Response from POST /api/admin/cache/time-sensitive-patterns/test. */
export interface TimeSensitiveTestResult {
  decision: 'match' | 'no_match';
  matchedRuleId: string | null;
  matchedKeywords: string[];
}

export const timeSensitivePatternsApi = {
  list: (): Promise<TimeSensitivePatternsResponse> =>
    api.get<TimeSensitivePatternsResponse>('/api/admin/cache/time-sensitive-patterns'),

  update: (id: string, pattern: TimeSensitivePattern): Promise<TimeSensitivePatternsResponse> =>
    api.put<TimeSensitivePatternsResponse>(`/api/admin/cache/time-sensitive-patterns/${encodeURIComponent(id)}`, pattern),

  create: (pattern: TimeSensitivePattern): Promise<TimeSensitivePattern> =>
    api.post<TimeSensitivePattern>('/api/admin/cache/time-sensitive-patterns', pattern),

  delete: (id: string): Promise<void> =>
    api.delete(`/api/admin/cache/time-sensitive-patterns/${encodeURIComponent(id)}`),

  test: (prompt: string, language?: string): Promise<TimeSensitiveTestResult> =>
    api.post<TimeSensitiveTestResult>('/api/admin/cache/time-sensitive-patterns/test', { prompt, language }),
};
