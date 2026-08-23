import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { StrategyConfigSection, type StrategyConfigSectionProps } from '@/pages/ai-gateway/routing/form/StrategyConfigSection';
import { emptyConditionalFormState } from '@/pages/ai-gateway/routing/_shared/routing-rule-config';

const providerGroups = [
  { provider: { id: 'p1', name: 'openai', displayName: 'OpenAI' }, models: [{ id: 'm1', providerModelId: 'gpt-4o', name: 'GPT-4o' }] },
] as never;

function baseProps(over: Partial<StrategyConfigSectionProps>): StrategyConfigSectionProps {
  return {
    pipelineStage: '1', strategyType: 'single', providerGroups,
    policyAllowM: [], setPolicyAllowM: vi.fn(), policyDenyM: [], setPolicyDenyM: vi.fn(),
    policyAllowP: [], setPolicyAllowP: vi.fn(), policyDenyP: [], setPolicyDenyP: vi.fn(),
    singleProvider: '', setSingleProvider: vi.fn(), singleModel: '', setSingleModel: vi.fn(),
    entries: [], updateEntry: vi.fn(), addEntry: vi.fn(), removeEntry: vi.fn(),
    conditionalUi: { mode: 'form', form: emptyConditionalFormState() }, setConditionalUi: vi.fn(),
    smartState: { routerProvider: '', routerModel: '', systemPrompt: '', temperature: '0', maxTokens: '1024', timeoutMs: '10000', defaultProvider: '', defaultModel: '' } as never,
    updateSmart: vi.fn(), showWeightColumn: false,
    ...over,
  } as StrategyConfigSectionProps;
}
function wrap(over: Partial<StrategyConfigSectionProps>) {
  return render(<I18nextProvider i18n={i18n}><StrategyConfigSection {...baseProps(over)} /></I18nextProvider>);
}

describe('StrategyConfigSection', () => {
  it('single strategy renders the provider-configuration section', () => {
    wrap({ pipelineStage: '1', strategyType: 'single' });
    expect(screen.getByText(i18n.t('pages:routing.providerConfiguration'))).toBeInTheDocument();
  });

  it('fallback strategy renders the fallback-chain title + entries table', () => {
    wrap({ pipelineStage: '1', strategyType: 'fallback' });
    expect(screen.getByText(i18n.t('pages:routing.fallbackChainTitle'))).toBeInTheDocument();
  });

  it('conditional strategy renders the conditional routing editor', () => {
    wrap({ pipelineStage: '1', strategyType: 'conditional' });
    expect(screen.getByText(i18n.t('pages:routing.conditionalRouting'))).toBeInTheDocument();
  });

  it('smart strategy renders the intelligent-routing config', () => {
    wrap({ pipelineStage: '1', strategyType: 'smart' });
    expect(screen.getByText(i18n.t('pages:routing.intelligentRoutingConfig'))).toBeInTheDocument();
  });

  it('loadbalance + ab_split strategies render without throwing', () => {
    expect(() => wrap({ pipelineStage: '1', strategyType: 'loadbalance' })).not.toThrow();
    expect(() => wrap({ pipelineStage: '1', strategyType: 'ab_split' })).not.toThrow();
  });
});
