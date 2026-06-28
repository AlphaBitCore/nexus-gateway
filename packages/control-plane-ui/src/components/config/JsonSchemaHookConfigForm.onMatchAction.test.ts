import { describe, it, expect } from 'vitest';
import { onMatchAction } from './JsonSchemaHookConfigForm';

describe('onMatchAction', () => {
  it('reads the new single action verbatim', () => {
    expect(onMatchAction({ action: 'approve' })).toBe('approve');
    expect(onMatchAction({ action: 'redact' })).toBe('redact');
    expect(onMatchAction({ action: 'block' })).toBe('block');
  });

  it('defaults to block when no action and no legacy keys are present', () => {
    expect(onMatchAction({})).toBe('block');
  });

  it('maps a legacy inflightAction/storageAction pair so an unmigrated config opens correctly', () => {
    expect(onMatchAction({ inflightAction: 'block-hard', storageAction: 'redact' })).toBe('block');
    expect(onMatchAction({ inflightAction: 'block-soft' })).toBe('block');
    expect(onMatchAction({ inflightAction: 'redact' })).toBe('redact');
    expect(onMatchAction({ inflightAction: 'approve', storageAction: 'keep' })).toBe('approve');
    // approve + a redacting storage policy upgrades to redact (compliance-safe).
    expect(onMatchAction({ inflightAction: 'approve', storageAction: 'redact' })).toBe('redact');
    expect(onMatchAction({ inflightAction: 'approve', storageAction: 'drop-content' })).toBe('redact');
  });

  it('prefers a valid new action over lingering legacy keys', () => {
    expect(onMatchAction({ action: 'approve', inflightAction: 'block-hard' })).toBe('approve');
  });
});
