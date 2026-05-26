import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import type { Alert } from '@/api/services';
import { ProxyHighErrorRateRenderer } from './ProxyHighErrorRateRenderer';

function mkAlert(details: Record<string, unknown>): Alert {
  return {
    id: 'a4',
    ruleId: 'proxy.high_error_rate',
    sourceType: 'proxy',
    targetKey: 'proxy:p1',
    targetLabel: 'compliance-proxy p1',
    severity: 'high',
    state: 'firing',
    message: 'High 5xx rate',
    details,
    firedAt: '2026-04-22T00:00:00Z',
    lastSeenAt: '2026-04-22T00:00:00Z',
    duplicateCount: 0,
  };
}

describe('ProxyHighErrorRateRenderer', () => {
  it('renders ratePct plus error and request counts', () => {
    renderWithProviders(
      <ProxyHighErrorRateRenderer
        alert={mkAlert({
          ratePct: 15.5,
          thresholdPct: 10,
          errors: 30,
          total: 200,
          windowSec: 300,
        })}
      />,
    );
    expect(screen.getByText(/15\.5%/)).toBeInTheDocument();
    expect(screen.getByText('30')).toBeInTheDocument();
    expect(screen.getByText('200')).toBeInTheDocument();
  });

  it('accepts alternate field names fivexx / requests', () => {
    renderWithProviders(
      <ProxyHighErrorRateRenderer
        alert={mkAlert({ ratePct: 11, fivexx: 11, requests: 100 })}
      />,
    );
    expect(screen.getByText(/11\.0%/)).toBeInTheDocument();
    expect(screen.getByText('11')).toBeInTheDocument();
    expect(screen.getByText('100')).toBeInTheDocument();
  });

  it('omits the window row when windowSec is absent and em-dashes the rest', () => {
    renderWithProviders(<ProxyHighErrorRateRenderer alert={mkAlert({})} />);
    // two em dashes (errors + requests); window row absent.
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(3);
  });
});
