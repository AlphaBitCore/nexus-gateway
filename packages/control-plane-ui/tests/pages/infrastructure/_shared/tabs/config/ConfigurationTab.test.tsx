/**
 * ConfigurationTab unit tests — covers the merged 4-column view introduced by
 * ConfigurationTab tests including the override row styling, blacklist guard, killswitch
 * bypass banner, and the resync / clear-override mutations.
 *
 * We stub `hubApi` directly (rather than via MSW) because the component fetches
 * through the typed service and the test surface is small enough that spies on
 * the four functions used (`getAppliedConfig`, `resyncNodeAll`, `clearOverride`,
 * `setOverride`) keeps assertions explicit.
 */
import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '@/test/test-utils';
import {
  hubApi,
  type AppliedConfigResponse,
  type ResyncResponse,
  type ThingOverride,
} from '@/api/services/infrastructure/nodes/hub';
import { ConfigurationTab } from '../../../../../../src/pages/infrastructure/_shared/tabs/config/ConfigurationTab';

type GetAppliedConfigFn = (nodeId: string) => Promise<AppliedConfigResponse>;
type ResyncNodeAllFn = (nodeId: string, body?: { configKey?: string }) => Promise<ResyncResponse>;
type ClearOverrideFn = (nodeId: string, configKey: string) => Promise<void>;

function makeOverride(partial: Partial<ThingOverride> & { configKey: string; state: unknown }): ThingOverride {
  return {
    templateVerAtSet: 1,
    currentTemplateVer: 1,
    stale: false,
    setBy: 'alice@nexus.ai',
    setAt: '2026-04-20T10:00:00Z',
    emergencyOverride: false,
    ...partial,
  };
}

function buildResponse(overrides: Partial<AppliedConfigResponse> = {}): AppliedConfigResponse {
  return {
    nodeId: 'node-gw-1',
    nodeType: 'ai_gateway',
    targetVersion: 12,
    appliedVersion: 12,
    configs: {},
    ...overrides,
  };
}

describe('ConfigurationTab', () => {
  let getSpy: MockInstance<GetAppliedConfigFn>;
  let resyncSpy: MockInstance<ResyncNodeAllFn>;
  let clearSpy: MockInstance<ClearOverrideFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(hubApi, 'getAppliedConfig') as unknown as MockInstance<GetAppliedConfigFn>;
    resyncSpy = vi.spyOn(hubApi, 'resyncNodeAll') as unknown as MockInstance<ResyncNodeAllFn>;
    clearSpy = vi.spyOn(hubApi, 'clearOverride') as unknown as MockInstance<ClearOverrideFn>;
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders 3 templated keys with no overrides — Force resync + Override actions', async () => {
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          routing: {
            targetConfig: { version: 3 },
            targetVersion: 12,
            appliedConfig: { version: 3 },
            appliedVersion: 12,
            inSync: true,
            templateState: { version: 3 },
            templateVer: 1,
          },
          hooks: {
            targetConfig: { list: [] },
            targetVersion: 12,
            appliedConfig: { list: [] },
            appliedVersion: 12,
            inSync: true,
            templateState: { list: [] },
            templateVer: 1,
          },
          killswitch: {
            targetConfig: { engaged: false },
            targetVersion: 12,
            appliedConfig: { engaged: false },
            appliedVersion: 12,
            inSync: true,
            templateState: { engaged: false },
            templateVer: 1,
          },
        },
      }),
    );

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => {
      expect(screen.getByText('routing')).toBeInTheDocument();
      expect(screen.getByText('hooks')).toBeInTheDocument();
      expect(screen.getByText('killswitch')).toBeInTheDocument();
    });

    // 3 + Override row buttons (one per non-blacklisted, non-overridden key) +
    // 1 toolbar "+ Add override" trigger = 4 buttons total.
    const overrideButtons = screen.getAllByRole('button', { name: /\+ Add override/i });
    expect(overrideButtons.length).toBe(4);

    // Each row shows a "Force resync" button (the in-sync wording).
    const resyncButtons = screen.getAllByRole('button', { name: /^Force resync$/i });
    expect(resyncButtons).toHaveLength(3);

    // No override / stale badges anywhere.
    expect(screen.queryAllByText(/^override$/i)).toHaveLength(0);
    expect(screen.queryAllByText(/^stale$/i)).toHaveLength(0);
  });

  it('renders an active stale override row with override + stale badges and Edit/Clear actions', async () => {
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          routing: {
            targetConfig: { version: 9 },
            targetVersion: 14,
            appliedConfig: { version: 9 },
            appliedVersion: 14,
            inSync: true,
            templateState: { version: 3 },
            templateVer: 5,
            override: makeOverride({
              configKey: 'routing',
              state: { version: 9 },
              templateVerAtSet: 3,
              currentTemplateVer: 5,
              stale: true,
            }),
          },
        },
      }),
    );

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => {
      expect(screen.getByText('routing')).toBeInTheDocument();
    });

    // Badges.
    expect(screen.getByText(/^override$/i)).toBeInTheDocument();
    expect(screen.getByText(/^stale$/i)).toBeInTheDocument();

    // Actions: Edit + Clear, no "+ Add override" on this row.
    expect(screen.getByRole('button', { name: /^Edit$/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Clear$/ })).toBeInTheDocument();
  });

  it('greys out blacklist key (`credentials`) and disables + Override but keeps Force resync enabled', async () => {
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          credentials: {
            targetConfig: { provider: 'openai' },
            targetVersion: 12,
            appliedConfig: { provider: 'openai' },
            appliedVersion: 12,
            inSync: true,
            templateState: { provider: 'openai' },
            templateVer: 1,
          },
        },
      }),
    );

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => {
      expect(screen.getByText('credentials')).toBeInTheDocument();
    });

    expect(screen.getByText(/^global-only$/i)).toBeInTheDocument();

    // Both the toolbar trigger and the per-row "+ Add override" button render.
    // The per-row button must be disabled; the toolbar dropdown trigger is
    // also disabled because the only key is non-overridable.
    const addButtons = screen.getAllByRole('button', { name: /\+ Add override/i });
    expect(addButtons.length).toBeGreaterThan(0);
    addButtons.forEach((b) => expect(b).toBeDisabled());

    // Force resync still enabled.
    const resyncButtons = screen.getAllByRole('button', { name: /^Force resync$/ });
    expect(resyncButtons).toHaveLength(1);
    expect(resyncButtons[0]).toBeEnabled();
  });

  it('clicks "Force resync all" and calls resyncNodeAll with no configKey', async () => {
    const user = userEvent.setup();
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          routing: {
            targetConfig: { version: 3 },
            targetVersion: 12,
            appliedConfig: { version: 3 },
            appliedVersion: 12,
            inSync: true,
            templateState: { version: 3 },
            templateVer: 1,
          },
        },
      }),
    );
    resyncSpy.mockResolvedValue({ ok: true, nodeId: 'node-gw-1', keyCount: 1 });

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => expect(screen.getByText('routing')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: /Force resync all/i }));

    await waitFor(() => {
      expect(resyncSpy).toHaveBeenCalledWith('node-gw-1');
    });
  });

  it('clicks per-row "Force resync" on an override row and passes the configKey', async () => {
    const user = userEvent.setup();
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          routing: {
            targetConfig: { version: 9 },
            targetVersion: 14,
            appliedConfig: { version: 9 },
            appliedVersion: 14,
            inSync: true,
            templateState: { version: 3 },
            templateVer: 5,
            override: makeOverride({
              configKey: 'routing',
              state: { version: 9 },
              templateVerAtSet: 5,
              currentTemplateVer: 5,
            }),
          },
        },
      }),
    );
    resyncSpy.mockResolvedValue({ ok: true, nodeId: 'node-gw-1', configKey: 'routing' });

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => expect(screen.getByText('routing')).toBeInTheDocument());

    // Per-row "Force resync" sits inside the row's actions cell, alongside
    // Edit/Clear. The toolbar button is "Force resync all", so we exclude it.
    const rowResync = screen.getByRole('button', { name: /^Force resync$/ });
    await user.click(rowResync);

    await waitFor(() => {
      expect(resyncSpy).toHaveBeenCalledWith('node-gw-1', { configKey: 'routing' });
    });
  });

  it('clicks "Clear" on an override row and calls clearOverride + refetches', async () => {
    const user = userEvent.setup();
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          routing: {
            targetConfig: { version: 9 },
            targetVersion: 14,
            appliedConfig: { version: 9 },
            appliedVersion: 14,
            inSync: true,
            templateState: { version: 3 },
            templateVer: 5,
            override: makeOverride({
              configKey: 'routing',
              state: { version: 9 },
              templateVerAtSet: 5,
              currentTemplateVer: 5,
            }),
          },
        },
      }),
    );
    clearSpy.mockResolvedValue(undefined);

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => expect(screen.getByText('routing')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: /^Clear$/ }));

    await waitFor(() => {
      expect(clearSpy).toHaveBeenCalledWith('node-gw-1', 'routing');
    });

    // After clearing, the component refetches — the get spy is called again.
    await waitFor(() => {
      expect(getSpy.mock.calls.length).toBeGreaterThanOrEqual(2);
    });
  });

  it('renders the killswitch bypass banner when an override on `killswitch` sets engaged=false', async () => {
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          killswitch: {
            targetConfig: { engaged: false },
            targetVersion: 14,
            appliedConfig: { engaged: false },
            appliedVersion: 14,
            inSync: true,
            templateState: { engaged: true },
            templateVer: 5,
            override: makeOverride({
              configKey: 'killswitch',
              state: { engaged: false },
              templateVerAtSet: 5,
              currentTemplateVer: 5,
              setBy: 'alice@nexus.ai',
              setAt: '2026-04-20T10:00:00Z',
              emergencyOverride: true,
            }),
          },
        },
      }),
    );

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="compliance_proxy" />);

    await waitFor(() => expect(screen.getByText('killswitch')).toBeInTheDocument());

    const banner = screen.getByRole('alert');
    expect(banner).toBeInTheDocument();
    expect(within(banner).getByText(/bypasses an active killswitch/i)).toBeInTheDocument();
    expect(within(banner).getByText(/alice@nexus\.ai/)).toBeInTheDocument();
  });

  it('opens the OverrideEditorDrawer with the right configKey when "+ Add override" dropdown item is clicked', async () => {
    const user = userEvent.setup();
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          // Single non-blacklisted, non-overridden key — the toolbar
          // dropdown should list exactly this one entry.
          routing: {
            targetConfig: { version: 3 },
            targetVersion: 12,
            appliedConfig: { version: 3 },
            appliedVersion: 12,
            inSync: true,
            templateState: { version: 3 },
            templateVer: 1,
          },
        },
      }),
    );

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="ai_gateway" />);

    await waitFor(() => expect(screen.getByText('routing')).toBeInTheDocument());

    // No drawer mounted before the user opens it.
    expect(screen.queryByRole('dialog')).toBeNull();

    // Toolbar "+ Add override ▾" dropdown trigger sits adjacent to the
    // "Force resync all" button. The per-row "+ Add override" button on
    // the row is also a valid entry into the editor; we drive the toolbar
    // path here because it exercises the dropdown menu as well.
    const triggers = screen.getAllByRole('button', { name: /\+ Add override/i });
    // First trigger is the toolbar dropdown ("+ Add override ▾"), per the
    // render order — toolbar comes before the table rows.
    await user.click(triggers[0]);

    // Dropdown item carries the configKey as its visible label.
    const dropdownItem = await screen.findByRole('menuitem', { name: /routing/ });
    await user.click(dropdownItem);

    // Drawer mounts with the dialog role + a title that interpolates the
    // configKey + thingId via `pages:infrastructure.editor.title`.
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toBeInTheDocument();
    expect(within(dialog).getByText(/routing/)).toBeInTheDocument();
    expect(within(dialog).getByText(/node-gw-1/)).toBeInTheDocument();
  });

  it('does not render the killswitch banner when override.engaged is truthy', async () => {
    getSpy.mockResolvedValue(
      buildResponse({
        configs: {
          killswitch: {
            targetConfig: { engaged: true },
            targetVersion: 14,
            appliedConfig: { engaged: true },
            appliedVersion: 14,
            inSync: true,
            templateState: { engaged: false },
            templateVer: 5,
            override: makeOverride({
              configKey: 'killswitch',
              state: { engaged: true },
              templateVerAtSet: 5,
              currentTemplateVer: 5,
            }),
          },
        },
      }),
    );

    renderWithProviders(<ConfigurationTab thingId="node-gw-1" thingType="compliance_proxy" />);

    await waitFor(() => expect(screen.getByText('killswitch')).toBeInTheDocument());

    expect(screen.queryByRole('alert')).toBeNull();
  });
});
