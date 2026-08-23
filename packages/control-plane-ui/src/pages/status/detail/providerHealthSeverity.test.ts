import { describe, it, expect } from 'vitest';
import { providerHealthSeverity } from './providerHealthSeverity';

// The status page carried its own status vocabulary, and it did not match the
// one the backend emits. The rollup writes exactly healthy / degraded /
// unavailable; the page's list named unhealthy, down and disabled — none of
// which any producer writes — and omitted unavailable, so a provider refusing
// traffic rendered in the neutral colour and read as "unknown".
describe('providerHealthSeverity', () => {
  it('covers every value the rollup emits', () => {
    // These three are the complete set written by
    // nexus-hub/internal/jobs/defs/rollup/provider_health_rollup.go.
    expect(providerHealthSeverity('healthy')).toBe('ok');
    expect(providerHealthSeverity('degraded')).toBe('warn');
    expect(providerHealthSeverity('unavailable')).toBe('bad');
  });

  it('never leaves a real verdict in the neutral bucket', () => {
    for (const status of ['healthy', 'degraded', 'unavailable']) {
      expect(providerHealthSeverity(status), status).not.toBe('unknown');
    }
  });

  it('still reads the configured-state values the same pill renders', () => {
    expect(providerHealthSeverity('enabled')).toBe('ok');
    expect(providerHealthSeverity('disabled')).toBe('bad');
  });

  it('is case-insensitive and safe on anything else', () => {
    expect(providerHealthSeverity('UNAVAILABLE')).toBe('bad');
    expect(providerHealthSeverity('')).toBe('unknown');
    expect(providerHealthSeverity('something-new')).toBe('unknown');
  });
});
