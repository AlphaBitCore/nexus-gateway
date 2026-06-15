import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';

import { renderWithProviders } from '@/test/test-utils';
import type { Alert } from '@/api/services';
import { ProxyHookFailureRateRenderer } from '../../../../src/pages/alerts/detailRenderers/ProxyHookFailureRateRenderer';

function mkAlert(details: Record<string, unknown>): Alert {
  return {
    id: 'a3',
    ruleId: 'proxy.hook_failure_rate',
    sourceType: 'proxy',
    targetKey: 'proxy:p1',
    targetLabel: 'compliance-proxy p1',
    severity: 'high',
    state: 'firing',
    message: 'Hook failure rate is 25%',
    details,
    firedAt: '2026-04-22T00:00:00Z',
    lastSeenAt: '2026-04-22T00:00:00Z',
    duplicateCount: 0,
  };
}

describe('ProxyHookFailureRateRenderer', () => {
  it('renders ratePct prominently plus failure + sample counts', () => {
    renderWithProviders(
      <ProxyHookFailureRateRenderer
        alert={mkAlert({ ratePct: 24.7, failures: 25, decisions: 101 })}
      />,
    );
    expect(screen.getByText(/24\.7%/)).toBeInTheDocument();
    expect(screen.getByText('25')).toBeInTheDocument();
    expect(screen.getByText('101')).toBeInTheDocument();
  });

  it('accepts timeout-rate shape using `timeouts` alongside decisions', () => {
    renderWithProviders(
      <ProxyHookFailureRateRenderer
        alert={mkAlert({ ratePct: 12.0, timeouts: 4, decisions: 33 })}
      />,
    );
    expect(screen.getByText(/12\.0%/)).toBeInTheDocument();
    // timeouts surfaced via failureCount slot.
    expect(screen.getByText('4')).toBeInTheDocument();
    expect(screen.getByText('33')).toBeInTheDocument();
  });

  it('renders em dashes when fields are missing', () => {
    renderWithProviders(<ProxyHookFailureRateRenderer alert={mkAlert({})} />);
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(2);
  });

  it('renders the breakdown table when provided', () => {
    renderWithProviders(
      <ProxyHookFailureRateRenderer
        alert={mkAlert({
          ratePct: 30,
          failures: 9,
          decisions: 30,
          breakdown: { 'hook.a': 4, 'hook.b': 5 },
        })}
      />,
    );
    expect(screen.getByText('hook.a')).toBeInTheDocument();
    expect(screen.getByText('hook.b')).toBeInTheDocument();
  });
});
