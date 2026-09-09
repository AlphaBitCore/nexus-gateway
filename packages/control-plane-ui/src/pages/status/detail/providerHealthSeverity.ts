/**
 * Severity for a provider's health status, from the vocabulary the backend
 * actually emits.
 *
 * The rollup writes exactly three values — `healthy`, `degraded`,
 * `unavailable` (nexus-hub provider_health_rollup.go). A status page that
 * carries its own list in three near-identical colour functions drifts: such a list
 * named `unhealthy`, `down` and `disabled` — none of which any producer emits —
 * while omitting `unavailable`, the one value that means a provider is
 * refusing traffic, which then renders in the neutral colour: the single
 * state an operator most needs to see reads as "unknown" rather than "bad".
 *
 * `enabled` / `disabled` are kept because the same pill renders a provider's
 * configured state elsewhere on the page, where those are the real values.
 */
export type ProviderHealthSeverity = 'ok' | 'warn' | 'bad' | 'unknown';

export function providerHealthSeverity(status: string): ProviderHealthSeverity {
  switch (status?.toLowerCase()) {
    case 'healthy':
    case 'enabled':
      return 'ok';
    case 'degraded':
      return 'warn';
    case 'unavailable':
    case 'disabled':
      return 'bad';
    default:
      return 'unknown';
  }
}
