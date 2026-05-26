/**
 * Integration test — RoutingRuleList renders rules and supports interactions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { ConfigRoutingPage } from './RoutingRuleList';
import { mockRoutingRule } from '@/test/msw-handlers';

function renderRoutingList() {
  return renderWithRouter(
    
      <ConfigRoutingPage />
    ,
  );
}

describe('RoutingRuleList', () => {
  it('renders page title', async () => {
    renderRoutingList();
    await waitFor(() => {
      expect(screen.getByText(/routing rules/i)).toBeDefined();
    });
  });

  it('displays routing rule data', async () => {
    renderRoutingList();
    await waitFor(() => {
      expect(screen.getByText(mockRoutingRule.name)).toBeDefined();
    });
  });

  it('shows empty message when no rules', async () => {
    server.use(
      http.get('/api/admin/routing-rules', () =>
        HttpResponse.json({ data: [], total: 0 }),
      ),
    );

    renderRoutingList();
    await waitFor(() => {
      expect(screen.getByText(/no routing rules/i)).toBeDefined();
    });
  });

  it('renders a custom-retry badge inline when the rule has a retryPolicy', async () => {
    server.use(
      http.get('/api/admin/routing-rules', () =>
        HttpResponse.json({
          data: [{
            ...mockRoutingRule,
            id: 'rule-with-retry',
            name: 'rule-with-retry',
            retryPolicy: { maxAttemptsPerTarget: 4, retryOn: ['5xx', 'timeout'] },
          }],
          total: 1,
        }),
      ),
    );
    renderRoutingList();
    await waitFor(() => {
      expect(screen.getByTestId('routing-rule-retry-badge')).toBeDefined();
    });
    const badge = screen.getByTestId('routing-rule-retry-badge');
    expect(badge.textContent).toContain('4');
    expect(badge.textContent).toContain('5xx');
    expect(badge.textContent).toContain('timeout');
  });

  it('renders no retry badge for rules with no override', async () => {
    renderRoutingList();
    await waitFor(() => {
      expect(screen.getByText(mockRoutingRule.name)).toBeDefined();
    });
    expect(screen.queryByTestId('routing-rule-retry-badge')).toBeNull();
  });
});
