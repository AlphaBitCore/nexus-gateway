import { describe, it, expect } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';
import type { Alert } from '@/api/services';
import { GenericRenderer } from '../../../../src/pages/alerts/detailRenderers/GenericRenderer';

function mkAlert(details: Record<string, unknown>): Alert {
  return {
    id: 'a1',
    ruleId: 'unknown.rule',
    sourceType: 'proxy',
    targetKey: 'proxy:p1',
    targetLabel: 'proxy p1',
    severity: 'medium',
    state: 'firing',
    message: 'hi',
    details,
    firedAt: '2026-04-22T00:00:00Z',
    lastSeenAt: '2026-04-22T00:00:00Z',
    duplicateCount: 0,
  };
}

describe('GenericRenderer', () => {
  it('pretty-prints the details JSON', () => {
    renderWithProviders(
      <GenericRenderer alert={mkAlert({ foo: 'bar', n: 42 })} />,
    );
    // JSON.stringify produces text nodes; find by matching substring.
    const pre = document.querySelector('pre');
    expect(pre).not.toBeNull();
    expect(pre!.textContent).toContain('"foo": "bar"');
    expect(pre!.textContent).toContain('"n": 42');
  });

  it('falls back to {} when details is empty', () => {
    renderWithProviders(<GenericRenderer alert={mkAlert({})} />);
    const pre = document.querySelector('pre');
    expect(pre).not.toBeNull();
    expect(pre!.textContent?.trim()).toBe('{}');
  });
});
