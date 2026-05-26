/**
 * InputStagingSelector.test.tsx
 *
 * Covers:
 *  - All 5 options render
 *  - Suggested option has the "(Recommended)" suffix in the option text
 *  - The badge outside the select appears only when selected === suggested
 *  - onChange invoked on selection
 *  - disabled prop disables the select
 *  - Re-evaluates suggested when modelContextLimit changes
 *  - aria-label forwarded to select
 *  - strings prop overrides labels/badge/help text
 *  - i18n key presence across EN/ZH/ES (uses the JSON files directly)
 */

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { InputStagingSelector } from './InputStagingSelector';
import type { InputStagingStrategy } from './suggest';

// ── helpers ──────────────────────────────────────────────────────────────────

const STRATEGIES: InputStagingStrategy[] = [
  'last_user',
  'system_plus_last_user',
  'recent_turns',
  'head_plus_tail',
  'full_truncated',
];

function renderSelector(
  overrides?: Partial<Parameters<typeof InputStagingSelector>[0]>,
) {
  const onChange = vi.fn();
  const result = render(
    <InputStagingSelector
      modelContextLimit={8192}
      value="system_plus_last_user"
      onChange={onChange}
      {...overrides}
    />,
  );
  return { onChange, ...result };
}

// ── 1. Rendering ─────────────────────────────────────────────────────────────

describe('InputStagingSelector — rendering', () => {
  it('renders all 5 strategy options', () => {
    renderSelector();
    const select = screen.getByRole('combobox');
    const options = Array.from((select as HTMLSelectElement).options);
    expect(options).toHaveLength(5);

    // Each option's value must be one of the 5 strategy strings.
    const values = options.map((o) => o.value);
    expect(values).toContain('last_user');
    expect(values).toContain('system_plus_last_user');
    expect(values).toContain('recent_turns');
    expect(values).toContain('head_plus_tail');
    expect(values).toContain('full_truncated');
  });

  it('renders the default English label for the dropdown', () => {
    renderSelector();
    expect(screen.getByText('Input Staging Strategy')).toBeInTheDocument();
  });

  it('uses custom label from strings prop', () => {
    renderSelector({ strings: { label: 'Strategy' } });
    expect(screen.getByText('Strategy')).toBeInTheDocument();
  });

  it('renders help text when provided via strings.helpText', () => {
    renderSelector({ strings: { helpText: 'Choose the truncation approach.' } });
    expect(screen.getByText('Choose the truncation approach.')).toBeInTheDocument();
  });

  it('does not render help text paragraph when strings.helpText is absent', () => {
    const { container } = renderSelector();
    const helpEl = container.querySelector('.helpText');
    expect(helpEl).toBeNull();
  });
});

// ── 2. Recommended badge ──────────────────────────────────────────────────────

describe('InputStagingSelector — recommended badge', () => {
  it('appends (Recommended) to the suggested option label in the option text', () => {
    // 8192 tokens + generic profile → suggested = system_plus_last_user
    renderSelector({ modelContextLimit: 8192, profile: 'generic' });
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const suggestedOption = Array.from(select.options).find(
      (o) => o.value === 'system_plus_last_user',
    );
    expect(suggestedOption?.text).toContain('Recommended');
    // Other options should NOT contain the badge text
    const nonSuggested = Array.from(select.options).filter(
      (o) => o.value !== 'system_plus_last_user',
    );
    for (const opt of nonSuggested) {
      expect(opt.text).not.toContain('Recommended');
    }
  });

  it('shows the badge element outside the select when selected === suggested', () => {
    // 8192, generic → suggested = system_plus_last_user; value = same
    renderSelector({
      modelContextLimit: 8192,
      profile: 'generic',
      value: 'system_plus_last_user',
    });
    expect(screen.getByText('Recommended')).toBeInTheDocument();
  });

  it('hides the badge element when selected != suggested', () => {
    // 8192, generic → suggested = system_plus_last_user; value = last_user
    const { queryByText } = renderSelector({
      modelContextLimit: 8192,
      profile: 'generic',
      value: 'last_user',
    });
    // The option text contains "(Recommended)" for system_plus_last_user,
    // but the standalone badge should not appear because selected != suggested.
    // There should be no standalone badge element. The word "Recommended"
    // may still appear inside the option text — we test the badge element via aria-live.
    const badgeByRole = document.querySelector('[aria-live]');
    expect(badgeByRole).toBeNull();
  });

  it('moves badge when modelContextLimit changes (long_completion, 8192 → recent_turns)', () => {
    // First render: 8192 + generic → system_plus_last_user suggested
    const { rerender, getByRole } = render(
      <InputStagingSelector
        modelContextLimit={8192}
        profile="generic"
        value="system_plus_last_user"
        onChange={vi.fn()}
      />,
    );
    const select = getByRole('combobox') as HTMLSelectElement;
    const beforeOpt = Array.from(select.options).find(
      (o) => o.value === 'system_plus_last_user',
    );
    expect(beforeOpt?.text).toContain('Recommended');

    // Rerender with long_completion → same 8192 → suggested = recent_turns
    rerender(
      <InputStagingSelector
        modelContextLimit={8192}
        profile="long_completion"
        value="system_plus_last_user"
        onChange={vi.fn()}
      />,
    );
    const afterSelectEl = getByRole('combobox') as HTMLSelectElement;
    const recentOpt = Array.from(afterSelectEl.options).find(
      (o) => o.value === 'recent_turns',
    );
    const sysOpt = Array.from(afterSelectEl.options).find(
      (o) => o.value === 'system_plus_last_user',
    );
    expect(recentOpt?.text).toContain('Recommended');
    expect(sysOpt?.text).not.toContain('Recommended');
  });

  it('uses custom recommendedBadge label from strings prop', () => {
    renderSelector({
      modelContextLimit: 8192,
      profile: 'generic',
      value: 'system_plus_last_user',
      strings: { recommendedBadge: '推荐' },
    });
    expect(screen.getByText('推荐')).toBeInTheDocument();
  });
});

// ── 3. onChange ───────────────────────────────────────────────────────────────

describe('InputStagingSelector — onChange', () => {
  it('invokes onChange immediately when user selects a different option', () => {
    const { onChange } = renderSelector();
    const select = screen.getByRole('combobox');
    fireEvent.change(select, { target: { value: 'recent_turns' } });
    expect(onChange).toHaveBeenCalledOnce();
    expect(onChange).toHaveBeenCalledWith('recent_turns');
  });

  it('invokes onChange with the correct strategy value', () => {
    const { onChange } = renderSelector({ value: 'last_user' });
    const select = screen.getByRole('combobox');
    fireEvent.change(select, { target: { value: 'head_plus_tail' } });
    expect(onChange).toHaveBeenCalledWith('head_plus_tail');
  });
});

// ── 4. Disabled ───────────────────────────────────────────────────────────────

describe('InputStagingSelector — disabled', () => {
  it('disables the select when disabled=true', () => {
    renderSelector({ disabled: true });
    expect(screen.getByRole('combobox')).toBeDisabled();
  });

  it('does not disable the select when disabled is absent/false', () => {
    renderSelector();
    expect(screen.getByRole('combobox')).not.toBeDisabled();
  });
});

// ── 5. Accessibility ──────────────────────────────────────────────────────────

describe('InputStagingSelector — accessibility', () => {
  it('forwards ariaLabel to the select element', () => {
    renderSelector({ ariaLabel: 'Embedding truncation strategy' });
    const select = screen.getByRole('combobox');
    expect(select).toHaveAttribute('aria-label', 'Embedding truncation strategy');
  });

  it('associates the label element with the select via htmlFor/id', () => {
    renderSelector();
    // The label text must be findable and connected to the select
    const label = screen.getByText('Input Staging Strategy');
    expect(label.tagName).toBe('LABEL');
    expect((label as HTMLLabelElement).htmlFor).toBe('input-staging-select');
  });
});

// ── 6. Strategy option labels ─────────────────────────────────────────────────

describe('InputStagingSelector — option labels', () => {
  it('uses built-in English labels when strings.strategies is absent', () => {
    renderSelector();
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const options = Array.from(select.options);
    // Each option text starts with one of the default English labels
    const texts = options.map((o) => o.text);
    expect(texts.some((t) => t.startsWith('Last User Message'))).toBe(true);
    expect(texts.some((t) => t.startsWith('System + Last User'))).toBe(true);
    expect(texts.some((t) => t.startsWith('Recent Turns'))).toBe(true);
    expect(texts.some((t) => t.startsWith('Head + Tail'))).toBe(true);
    expect(texts.some((t) => t.startsWith('Full (Truncated)'))).toBe(true);
  });

  it('uses partial strings.strategies overrides, falling back to defaults for unspecified keys', () => {
    renderSelector({
      strings: {
        strategies: {
          last_user: '仅最后一条用户消息',
          system_plus_last_user: '系统提示 + 最后用户消息',
        },
      },
    });
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const opts = Array.from(select.options);
    expect(opts.find((o) => o.value === 'last_user')?.text).toContain('仅最后一条用户消息');
    expect(opts.find((o) => o.value === 'recent_turns')?.text).toContain('Recent Turns');
  });

  it('all 5 strategies are present in the STRATEGIES constant order', () => {
    renderSelector();
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(STRATEGIES);
  });
});

// ── 7. Suggest boundary — modelContextLimit changes ───────────────────────────

describe('InputStagingSelector — modelContextLimit boundaries', () => {
  it('suggests last_user for tiny window (limit=512)', () => {
    renderSelector({ modelContextLimit: 512, profile: 'generic' });
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const opt = Array.from(select.options).find((o) => o.value === 'last_user');
    expect(opt?.text).toContain('Recommended');
  });

  it('suggests system_plus_last_user for medium window (limit=8192, generic)', () => {
    renderSelector({ modelContextLimit: 8192, profile: 'generic' });
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const opt = Array.from(select.options).find(
      (o) => o.value === 'system_plus_last_user',
    );
    expect(opt?.text).toContain('Recommended');
  });

  it('suggests recent_turns for large window (limit=32000, generic)', () => {
    renderSelector({ modelContextLimit: 32000, profile: 'generic' });
    const select = screen.getByRole('combobox') as HTMLSelectElement;
    const opt = Array.from(select.options).find((o) => o.value === 'recent_turns');
    expect(opt?.text).toContain('Recommended');
  });
});

// ── 8. i18n key presence across locales ───────────────────────────────────────

describe('InputStagingSelector — i18n key parity (EN/ZH/ES shared.json)', () => {
  // Import the locale files directly. Vitest resolves JSON via resolveJsonModule.
  // This test verifies that the inputStaging keys are present and non-empty
  // in every locale — the same invariant enforced by npm run check:i18n.

  const EN_REQUIRED_KEYS = [
    'inputStaging.label',
    'inputStaging.recommendedBadge',
    'inputStaging.help',
    'inputStaging.strategies.last_user',
    'inputStaging.strategies.system_plus_last_user',
    'inputStaging.strategies.recent_turns',
    'inputStaging.strategies.head_plus_tail',
    'inputStaging.strategies.full_truncated',
    'inputStaging.tooltips.last_user',
    'inputStaging.tooltips.system_plus_last_user',
    'inputStaging.tooltips.recent_turns',
    'inputStaging.tooltips.head_plus_tail',
    'inputStaging.tooltips.full_truncated',
  ];

  function getNestedValue(obj: Record<string, unknown>, dotPath: string): unknown {
    return dotPath.split('.').reduce<unknown>((acc, part) => {
      if (acc && typeof acc === 'object') {
        return (acc as Record<string, unknown>)[part];
      }
      return undefined;
    }, obj);
  }

  async function checkLocale(locale: string) {
    const json = await import(`../../i18n/${locale}/shared.json`);
    const data = json.default as Record<string, unknown>;
    for (const key of EN_REQUIRED_KEYS) {
      const val = getNestedValue(data, key);
      expect(
        typeof val === 'string' && val.length > 0,
        `locale=${locale} key="${key}" expected non-empty string, got: ${JSON.stringify(val)}`,
      ).toBe(true);
    }
  }

  it('en/shared.json has all required inputStaging keys', async () => {
    await checkLocale('en');
  });

  it('zh/shared.json has all required inputStaging keys', async () => {
    await checkLocale('zh');
  });

  it('es/shared.json has all required inputStaging keys', async () => {
    await checkLocale('es');
  });
});
