/**
 * suggest.test.ts — table-driven tests for suggestStrategy().
 *
 * Matrix mirrors the Go test in packages/shared/transport/inputstaging/staging_test.go
 * to guarantee TS / Go parity on the Suggest() heuristic.
 */

import { describe, it, expect } from 'vitest';
import { suggestStrategy } from './suggest';
import type { InputStagingProfile, InputStagingStrategy } from './suggest';

interface Case {
  limit: number;
  profile: InputStagingProfile;
  want: InputStagingStrategy;
  label: string;
}

const cases: Case[] = [
  // ── Tiny window (limit <= 1024) ───────────────────────────────────────────
  { limit: 1,    profile: 'generic',         want: 'last_user',            label: 'limit=1, generic' },
  { limit: 512,  profile: 'generic',         want: 'last_user',            label: 'limit=512, generic' },
  { limit: 1024, profile: 'generic',         want: 'last_user',            label: 'limit=1024, generic (boundary)' },
  { limit: 1024, profile: 'short_answer',    want: 'last_user',            label: 'limit=1024, short_answer' },
  { limit: 1024, profile: 'long_completion', want: 'last_user',            label: 'limit=1024, long_completion' },

  // ── Small window (1024 < limit <= 4096) ───────────────────────────────────
  { limit: 1025, profile: 'generic',         want: 'system_plus_last_user', label: 'limit=1025, generic' },
  { limit: 2048, profile: 'generic',         want: 'system_plus_last_user', label: 'limit=2048, generic' },
  { limit: 2048, profile: 'short_answer',    want: 'system_plus_last_user', label: 'limit=2048, short_answer' },
  { limit: 2048, profile: 'long_completion', want: 'last_user',             label: 'limit=2048, long_completion (output eats budget)' },
  { limit: 4096, profile: 'generic',         want: 'system_plus_last_user', label: 'limit=4096, generic (boundary)' },
  { limit: 4096, profile: 'long_completion', want: 'last_user',             label: 'limit=4096, long_completion (boundary)' },

  // ── Medium window (4096 < limit <= 16384) ─────────────────────────────────
  { limit: 4097,  profile: 'generic',         want: 'system_plus_last_user', label: 'limit=4097, generic' },
  { limit: 8192,  profile: 'generic',         want: 'system_plus_last_user', label: 'limit=8192, generic' },
  { limit: 8192,  profile: 'short_answer',    want: 'system_plus_last_user', label: 'limit=8192, short_answer' },
  { limit: 8192,  profile: 'long_completion', want: 'recent_turns',          label: 'limit=8192, long_completion' },
  { limit: 16384, profile: 'generic',         want: 'system_plus_last_user', label: 'limit=16384, generic (boundary)' },
  { limit: 16384, profile: 'short_answer',    want: 'system_plus_last_user', label: 'limit=16384, short_answer (boundary)' },
  { limit: 16384, profile: 'long_completion', want: 'recent_turns',          label: 'limit=16384, long_completion (boundary)' },

  // ── Large window (limit > 16384) ──────────────────────────────────────────
  { limit: 16385,  profile: 'generic',         want: 'recent_turns', label: 'limit=16385, generic' },
  { limit: 32000,  profile: 'generic',         want: 'recent_turns', label: 'limit=32000, generic' },
  { limit: 128000, profile: 'generic',         want: 'recent_turns', label: 'limit=128000, generic (GPT-4 class)' },
  { limit: 200000, profile: 'generic',         want: 'recent_turns', label: 'limit=200000, generic (Claude 3 class)' },
  { limit: 32000,  profile: 'short_answer',    want: 'recent_turns', label: 'limit=32000, short_answer' },
  { limit: 32000,  profile: 'long_completion', want: 'recent_turns', label: 'limit=32000, long_completion' },
];

describe('suggestStrategy', () => {
  it.each(cases)('$label → $want', ({ limit, profile, want }) => {
    expect(suggestStrategy(limit, profile)).toBe(want);
  });

  it('returns recent_turns for any profile when limit > 16384 (full large-window sweep)', () => {
    const profiles: InputStagingProfile[] = ['generic', 'short_answer', 'long_completion'];
    for (const profile of profiles) {
      expect(suggestStrategy(32768, profile)).toBe('recent_turns');
    }
  });

  it('boundary: limit=1024 always last_user regardless of profile', () => {
    const profiles: InputStagingProfile[] = ['generic', 'short_answer', 'long_completion'];
    for (const profile of profiles) {
      expect(suggestStrategy(1024, profile)).toBe('last_user');
    }
  });

  it('boundary: limit=4096 long_completion is last_user (not system_plus)', () => {
    expect(suggestStrategy(4096, 'long_completion')).toBe('last_user');
  });

  it('boundary: limit=4097 long_completion switches to recent_turns', () => {
    expect(suggestStrategy(4097, 'long_completion')).toBe('recent_turns');
  });

  it('boundary: limit=16384 generic is still system_plus_last_user (not recent_turns)', () => {
    expect(suggestStrategy(16384, 'generic')).toBe('system_plus_last_user');
  });

  it('boundary: limit=16385 generic flips to recent_turns', () => {
    expect(suggestStrategy(16385, 'generic')).toBe('recent_turns');
  });
});
