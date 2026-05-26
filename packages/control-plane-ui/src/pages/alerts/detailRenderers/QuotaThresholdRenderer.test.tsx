import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import type { Alert } from '@/api/services';
import { QuotaThresholdRenderer } from './QuotaThresholdRenderer';

function mkAlert(details: Record<string, unknown>): Alert {
  return {
    id: 'a1',
    ruleId: 'quota.threshold',
    sourceType: 'quota',
    targetKey: 'override:o1|period:2026-04',
    targetLabel: 'user:u1',
    severity: 'high',
    state: 'firing',
    message: 'Quota 80% threshold crossed',
    details,
    firedAt: '2026-04-22T00:00:00Z',
    lastSeenAt: '2026-04-22T00:00:00Z',
    duplicateCount: 0,
  };
}

describe('QuotaThresholdRenderer', () => {
  it('renders usage percent, spend, target, period', () => {
    renderWithProviders(
      <QuotaThresholdRenderer
        alert={mkAlert({
          pct: 87.345,
          threshold: 80,
          costLimitUsd: 100,
          currentCostUsd: 87.35,
          targetType: 'user',
          targetId: 'u1',
          period: '2026-04',
        })}
      />,
    );
    // pct is formatted to one decimal.
    expect(screen.getByText(/87\.3%/)).toBeInTheDocument();
    // Spend formatting: "$87.35 / $100.00"
    expect(screen.getByText(/\$87\.35/)).toBeInTheDocument();
    expect(screen.getByText(/\$100\.00/)).toBeInTheDocument();
    // Target rendered as "user:u1".
    expect(screen.getByText('user:u1')).toBeInTheDocument();
    // Period label and value both present.
    expect(screen.getByText('2026-04')).toBeInTheDocument();
  });

  it('renders em dashes when fields are missing or wrong type', () => {
    renderWithProviders(<QuotaThresholdRenderer alert={mkAlert({})} />);
    // Missing pct → no bar rendered, fall through to em dash grid.
    const dashes = screen.getAllByText('—');
    expect(dashes.length).toBeGreaterThanOrEqual(3); // spend + target + period
  });

  it('ignores non-numeric pct/threshold values', () => {
    renderWithProviders(
      <QuotaThresholdRenderer
        alert={mkAlert({
          pct: 'eighty-seven', // wrong type — treated as missing
          threshold: null,
          costLimitUsd: 50,
        })}
      />,
    );
    // No usage percent emphasis element should be rendered.
    expect(screen.queryByText(/%/)).not.toBeInTheDocument();
    // Spend still renders with $50.00 limit + dash for current.
    expect(screen.getByText(/\$50\.00/)).toBeInTheDocument();
  });
});
