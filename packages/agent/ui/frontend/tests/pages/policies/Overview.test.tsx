import { describe, it, expect, vi } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { PoliciesOverview } from '@/pages/policies/Overview';

// Mutable so each arm can hand the page a different sync state; the page reads
// it through useAppliedConfig.
let data: Record<string, unknown> = {};

vi.mock('@/pages/policies/useAppliedConfig', () => ({
  useAppliedConfig: () => ({ data, isLoading: false }),
  useRefreshPolicies: () => ({ refreshing: false, error: null, trigger: vi.fn(), clearError: vi.fn() }),
}));

function withSync(desiredVersion: number, reportedVersion: number) {
  return {
    sync: {
      desiredVersion,
      reportedVersion,
      inSync: desiredVersion === reportedVersion,
      lastReportedAt: '2026-08-27T10:00:00Z',
    },
    killSwitch: { engaged: false, reason: null },
    diag: undefined,
    interceptionDomains: [],
    hooks: [],
    exemptions: [],
    rulePacks: [],
  };
}

function wrap() {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><PoliciesOverview /></MemoryRouter>
    </I18nextProvider>,
  );
}

describe('PoliciesOverview — out-of-sync banner', () => {
  // The agent is a user-facing product surface, so it must speak the product
  // vocabulary (node / config sync / target / applied), never the internal
  // Thing-model words. This page shipped "Drifted from admin", "desired" and
  // "Last reported" in en while zh and es had already been written correctly,
  // and the terminology guard could not see it: its locale walk was pinned to
  // the control-plane-ui bundle.
  it('reports an out-of-sync agent in product vocabulary, not Thing-model words', async () => {
    data = withSync(7, 5);
    await i18n.changeLanguage('en');
    wrap();

    const body = document.body.textContent ?? '';
    expect(body).toContain('Out of sync');
    expect(body).not.toMatch(/\bDrifted\b/);
    expect(body).not.toMatch(/\bdesired\b/i);
    expect(body).not.toMatch(/\bLast reported\b/);
    cleanup();
  });

  // `{{s}}` was never a plural rule. The hero computed `s: behind === 1 ? '' :
  // 's'` while the sync card hardcoded `s: 's'`, so the card read "1 versions
  // behind"; and zh, which has no plural -s, rendered the token literally as
  // "落后 3 个版本s". Both now go through i18next's count-driven plurals.
  it('says "1 version behind", singular, in BOTH the hero and the sync card', async () => {
    data = withSync(6, 5);
    await i18n.changeLanguage('en');
    wrap();

    const body = document.body.textContent ?? '';
    expect(body).toContain('1 version behind');
    expect(body).not.toContain('1 versions behind');
    cleanup();
  });

  it('says "2 versions behind", plural, once more than one behind', async () => {
    data = withSync(7, 5);
    await i18n.changeLanguage('en');
    wrap();

    expect(document.body.textContent ?? '').toContain('2 versions behind');
    cleanup();
  });

  it('never renders a bare English plural -s into the zh bundle', async () => {
    data = withSync(8, 5);
    await i18n.changeLanguage('zh');
    wrap();

    const body = document.body.textContent ?? '';
    expect(body).toContain('落后 3 个版本');
    // The literal token artefact this replaced.
    expect(body).not.toContain('版本s');
    cleanup();
    await i18n.changeLanguage('en');
  });
});
