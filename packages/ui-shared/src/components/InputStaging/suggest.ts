/**
 * TS port of Go Suggest() from
 * packages/shared/transport/inputstaging/staging.go.
 *
 * MUST stay in sync with packages/shared/transport/inputstaging/staging.go
 * on any heuristic table change.
 *
 * Heuristic table:
 *   limit <= 1024                        → last_user
 *   1024 < limit <= 4096, generic        → system_plus_last_user
 *   1024 < limit <= 4096, long_compl.    → last_user
 *   4096 < limit <= 16384, generic/short → system_plus_last_user
 *   4096 < limit <= 16384, long_compl.   → recent_turns
 *   limit > 16384                        → recent_turns
 */

export type InputStagingStrategy =
  | 'last_user'
  | 'system_plus_last_user'
  | 'recent_turns'
  | 'head_plus_tail'
  | 'full_truncated';

export type InputStagingProfile = 'generic' | 'short_answer' | 'long_completion';

/**
 * Recommends an InputStagingStrategy from the model's context window size
 * and expected output profile. Mirrors Go Suggest() in
 * packages/shared/transport/inputstaging/staging.go — keep in sync.
 *
 * The result is a heuristic; admin config takes precedence. Called by
 * InputStagingSelector to highlight the recommended option.
 */
export function suggestStrategy(
  modelContextLimit: number,
  profile: InputStagingProfile,
): InputStagingStrategy {
  if (modelContextLimit <= 1024) {
    return 'last_user';
  }
  if (modelContextLimit <= 4096) {
    if (profile === 'long_completion') {
      return 'last_user';
    }
    return 'system_plus_last_user';
  }
  if (modelContextLimit <= 16384) {
    if (profile === 'long_completion') {
      return 'recent_turns';
    }
    return 'system_plus_last_user';
  }
  // modelContextLimit > 16384
  return 'recent_turns';
}
