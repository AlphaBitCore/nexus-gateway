import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';

import { renderWithProviders } from '@/test/test-utils';
import type { Alert } from '@/api/services';
import { QuotaVkExpiringRenderer } from '../../../../src/pages/alerts/detailRenderers/QuotaVkExpiringRenderer';

function mkAlert(details: Record<string, unknown>): Alert {
  return {
    id: 'a2',
    ruleId: 'quota.vk_expiring',
    sourceType: 'quota',
    targetKey: 'vk:k1',
    targetLabel: 'prod-key',
    severity: 'medium',
    state: 'firing',
    message: 'expiring',
    details,
    firedAt: '2026-04-22T00:00:00Z',
    lastSeenAt: '2026-04-22T00:00:00Z',
    duplicateCount: 0,
  };
}

describe('QuotaVkExpiringRenderer', () => {
  it('renders name, expiresAt, and daysLeft in a single-row table', () => {
    renderWithProviders(
      <QuotaVkExpiringRenderer
        alert={mkAlert({
          vkId: 'k1',
          name: 'prod-key',
          expiresAt: '2026-05-01T00:00:00Z',
          daysLeft: 9,
        })}
      />,
    );
    expect(screen.getByText('prod-key')).toBeInTheDocument();
    expect(screen.getByText('9')).toBeInTheDocument();
    // The exact date rendering depends on locale; verify the element exists.
    const dateCell = screen.getByRole('cell', {
      name: new RegExp(new Date('2026-05-01T00:00:00Z').getFullYear().toString()),
    });
    expect(dateCell).toBeInTheDocument();
  });

  it('falls back to vkId when name is missing, and em dash for other missing fields', () => {
    renderWithProviders(
      <QuotaVkExpiringRenderer alert={mkAlert({ vkId: 'k2' })} />,
    );
    expect(screen.getByText('k2')).toBeInTheDocument();
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(2);
  });

  it('renders all em dashes when details is empty', () => {
    renderWithProviders(<QuotaVkExpiringRenderer alert={mkAlert({})} />);
    expect(screen.getAllByText('—').length).toBe(3);
  });
});
