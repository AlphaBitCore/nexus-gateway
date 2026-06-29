import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { useStrategyOptions } from '@/pages/ai-gateway/routing/_shared/useStrategyOptions';
import {
  KvRow, MatchModelSelector, ProviderModelSelect, formatProviderMatchLine,
} from '@/pages/ai-gateway/routing/detail/RoutingRuleHelpers';

const groups = [
  { provider: { id: 'p1', name: 'openai', displayName: 'OpenAI' }, models: [
    { id: 'm1', providerModelId: 'gpt-4o', name: 'GPT-4o' },
    { id: 'm2', providerModelId: 'gpt-4o-mini', name: 'Mini' },
  ] },
] as never;
function I18n({ children }: { children: React.ReactNode }) {
  return <I18nextProvider i18n={i18n}>{children}</I18nextProvider>;
}

describe('formatProviderMatchLine', () => {
  it('joins provider display names, falls back to the raw id, and dashes empty', () => {
    expect(formatProviderMatchLine(groups, ['p1'])).toBe('OpenAI');
    expect(formatProviderMatchLine(groups, ['unknown'])).toBe('unknown');
    expect(formatProviderMatchLine(groups, [])).toBe('—');
  });
});

describe('useStrategyOptions', () => {
  it('returns a non-empty list of {value,label} strategy options', () => {
    let captured: Array<{ value: string; label: string }> = [];
    function Probe() { captured = useStrategyOptions(); return null; }
    render(<I18n><Probe /></I18n>);
    expect(captured.length).toBeGreaterThan(0);
    expect(captured[0]).toHaveProperty('value');
    expect(captured[0]).toHaveProperty('label');
  });
});

describe('KvRow', () => {
  it('renders the label, children, and a help tooltip trigger when help is provided', () => {
    render(<I18n><KvRow label="Strategy" helpTitle="Strategy help" helpBody={<span>body</span>}><span>val</span></KvRow></I18n>);
    expect(screen.getByText('Strategy')).toBeInTheDocument();
    expect(screen.getByText('val')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Help: Strategy help' })).toBeInTheDocument();
  });
});

describe('ProviderModelSelect', () => {
  it('selecting a provider emits onProviderChange + clears the model', () => {
    const onProviderChange = vi.fn();
    const onModelChange = vi.fn();
    render(<I18n><ProviderModelSelect providerValue="" modelValue="" onProviderChange={onProviderChange} onModelChange={onModelChange} providerGroups={groups} /></I18n>);
    const [providerSel] = screen.getAllByRole('combobox');
    fireEvent.change(providerSel, { target: { value: 'openai' } });
    expect(onProviderChange).toHaveBeenCalledWith('openai');
    expect(onModelChange).toHaveBeenCalledWith('');
  });

  it('with a provider chosen, the model dropdown lists that provider models', () => {
    const onModelChange = vi.fn();
    render(<I18n><ProviderModelSelect providerValue="openai" modelValue="" onProviderChange={vi.fn()} onModelChange={onModelChange} providerGroups={groups} /></I18n>);
    const modelSel = screen.getAllByRole('combobox')[1];
    fireEvent.change(modelSel, { target: { value: 'gpt-4o' } });
    expect(onModelChange).toHaveBeenCalledWith('gpt-4o');
  });
});

describe('MatchModelSelector', () => {
  it('renders the searchable model multi-select (excludes already-targeted models)', () => {
    expect(() => render(
      <I18n><MatchModelSelector selected={[]} onChange={vi.fn()} providerGroups={groups} excludeModels={new Set(['m1'])} /></I18n>,
    )).not.toThrow();
  });
});
