import { describe, it, expect } from 'vitest';
import { thingStatusVariant } from '../../src/lib/thingStatus';

describe('thingStatusVariant', () => {
  it('maps the Hub-written lowercase statuses to their canonical variants', () => {
    expect(thingStatusVariant('online')).toBe('success');
    expect(thingStatusVariant('enrolled')).toBe('warning');
    expect(thingStatusVariant('offline')).toBe('default');
    expect(thingStatusVariant('revoked')).toBe('danger');
  });

  it('renders the drift status as a warning (config not converged, node still serving)', () => {
    // Regression guard: `drift` is what the Hub drift-reconciliation job actually
    // writes. Without an entry it falls through to the gray default.
    expect(thingStatusVariant('drift')).toBe('warning');
  });

  it('falls back to default for the retired out-of-sync token the Hub never writes', () => {
    expect(thingStatusVariant('out-of-sync')).toBe('default');
  });

  it('maps uppercase legacy AgentDevice aliases to the same variants', () => {
    expect(thingStatusVariant('ACTIVE')).toBe('success');
    expect(thingStatusVariant('ENROLLED')).toBe('warning');
    expect(thingStatusVariant('OFFLINE')).toBe('default');
    expect(thingStatusVariant('REVOKED')).toBe('danger');
  });

  it('is case-insensitive for the lowercase status set', () => {
    expect(thingStatusVariant('ONLINE')).toBe('success');
    expect(thingStatusVariant('Drift')).toBe('warning');
  });

  it('falls back to default for unknown statuses', () => {
    expect(thingStatusVariant('')).toBe('default');
    expect(thingStatusVariant('something-else')).toBe('default');
  });
});
