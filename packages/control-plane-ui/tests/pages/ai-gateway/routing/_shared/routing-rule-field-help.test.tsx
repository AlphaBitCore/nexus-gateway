import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { renderHook } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import {
  useRoutingFieldHelp,
  useStrategyConfigHelp,
  strategyConfigHelpBody,
  ROUTING_RULE_FIELD_HELP,
  RoutingStrategyTypesHelp,
} from '../../../../../src/pages/ai-gateway/routing/_shared/routing-rule-field-help';

const wrapper = ({ children }: { children: React.ReactNode }) => (
  <I18nextProvider i18n={i18n}>{children}</I18nextProvider>
);

const STRATEGIES = ['single', 'fallback', 'loadbalance', 'conditional', 'ab_split', 'smart', 'latency'] as const;

describe('routing-rule-field-help', () => {
  it('static ROUTING_RULE_FIELD_HELP + strategyConfigHelpBody cover every strategy', () => {
    expect(ROUTING_RULE_FIELD_HELP.primaryWinnerCallout.length).toBeGreaterThan(0);
    for (const s of STRATEGIES) {
      expect(typeof strategyConfigHelpBody[s]).toBe('string');
      expect(strategyConfigHelpBody[s].length).toBeGreaterThan(0);
    }
  });

  it('useRoutingFieldHelp returns the i18n help bundle', () => {
    const { result } = renderHook(() => useRoutingFieldHelp(), { wrapper });
    expect(result.current).toHaveProperty('strategyType');
    expect(result.current).toHaveProperty('matchConditions');
  });

  it('useStrategyConfigHelp maps every strategy key', () => {
    const { result } = renderHook(() => useStrategyConfigHelp(), { wrapper });
    for (const s of STRATEGIES) {
      expect(result.current[s]).toBeDefined();
    }
  });

  it('RoutingStrategyTypesHelp renders the help trigger button', () => {
    render(<RoutingStrategyTypesHelp />, { wrapper });
    expect(screen.getByRole('button')).toBeInTheDocument();
  });
});
