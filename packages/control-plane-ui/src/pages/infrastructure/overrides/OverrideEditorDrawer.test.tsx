/**
 * OverrideEditorDrawer unit tests.
 *
 * Covers the drawer's pre-fill logic per mode, JSON validity gating, the
 * "Reset to template" action, save-success wiring, the reason length guard,
 * the TTL preset → expiresAt conversion, and the close-on-Escape /
 * close-on-backdrop behaviours. We stub `hubApi.setOverride` so the test
 * runs without network.
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
  type MockInstance,
} from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { hubApi, type ThingOverride } from '@/api/services/infrastructure/nodes/hub';
import { OverrideEditorDrawer } from './OverrideEditorDrawer';

type SetOverrideFn = (
  thingId: string,
  configKey: string,
  body: { state: Record<string, unknown>; reason?: string; expiresAt?: string },
) => Promise<ThingOverride>;

const TEMPLATE_STATE = { engaged: false, mode: 'soft' };
const TEMPLATE_VER = 5;

function makeOverride(partial: Partial<ThingOverride> & { state: unknown }): ThingOverride {
  return {
    configKey: 'killswitch',
    templateVerAtSet: TEMPLATE_VER,
    currentTemplateVer: TEMPLATE_VER,
    stale: false,
    setBy: 'alice@nexus.ai',
    setAt: '2026-04-20T10:00:00Z',
    emergencyOverride: false,
    ...partial,
  };
}

function renderDrawer(overrides: Partial<React.ComponentProps<typeof OverrideEditorDrawer>> = {}) {
  const onClose = vi.fn();
  const onSaved = vi.fn();
  const props: React.ComponentProps<typeof OverrideEditorDrawer> = {
    open: true,
    thingId: 'node-gw-1',
    thingType: 'ai_gateway',
    configKey: 'killswitch',
    mode: 'add',
    templateState: TEMPLATE_STATE,
    templateVer: TEMPLATE_VER,
    onClose,
    onSaved,
    ...overrides,
  };
  const utils = renderWithProviders(<OverrideEditorDrawer {...props} />);
  return { ...utils, onClose, onSaved };
}

describe('OverrideEditorDrawer', () => {
  let setSpy: MockInstance<SetOverrideFn>;

  beforeEach(() => {
    setSpy = vi.spyOn(hubApi, 'setOverride') as unknown as MockInstance<SetOverrideFn>;
    setSpy.mockResolvedValue(makeOverride({ state: { engaged: true } }));
  });
  afterEach(() => vi.restoreAllMocks());

  it('add mode: editor pre-fills with the template JSON', () => {
    renderDrawer({ mode: 'add' });
    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe(JSON.stringify(TEMPLATE_STATE, null, 2));
  });

  it('edit mode: editor pre-fills with the existing override state', () => {
    const existing = makeOverride({ state: { engaged: true, mode: 'hard' } });
    renderDrawer({ mode: 'edit', existingOverride: existing });
    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe(JSON.stringify(existing.state, null, 2));
  });

  it('invalid JSON disables Save and surfaces an inline error', async () => {
    const user = userEvent.setup();
    renderDrawer();
    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    await user.clear(ta);
    // userEvent.type treats `{` as a key-chord prefix; use paste for literals.
    await user.click(ta);
    await user.paste('{not valid json');

    const saveBtn = screen.getByTestId('override-editor-save');
    expect(saveBtn).toBeDisabled();
    expect(screen.getByTestId('override-editor-json-error')).toBeInTheDocument();
  });

  it('top-level non-object JSON fails the object-only guard', async () => {
    const user = userEvent.setup();
    renderDrawer();
    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    await user.clear(ta);
    await user.click(ta);
    await user.paste('[1,2,3]');

    expect(screen.getByTestId('override-editor-save')).toBeDisabled();
    expect(screen.getByTestId('override-editor-json-error')).toHaveTextContent(/object/i);
  });

  it('Reset to template restores the editor content to the template JSON', async () => {
    const user = userEvent.setup();
    renderDrawer({ mode: 'add' });
    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    await user.clear(ta);
    await user.click(ta);
    await user.paste('{"engaged":true}');
    expect(ta.value).toBe('{"engaged":true}');

    await user.click(screen.getByTestId('override-editor-reset'));
    expect(ta.value).toBe(JSON.stringify(TEMPLATE_STATE, null, 2));
  });

  it('saves with valid JSON + reason and calls onSaved + onClose', async () => {
    const user = userEvent.setup();
    const { onClose, onSaved } = renderDrawer();
    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    await user.clear(ta);
    await user.click(ta);
    await user.paste('{"engaged":true}');

    const reason = screen.getByTestId('override-editor-reason') as HTMLInputElement;
    await user.type(reason, 'Customer escalation #42');

    await user.click(screen.getByTestId('override-editor-save'));

    await waitFor(() => {
      expect(setSpy).toHaveBeenCalledTimes(1);
    });
    const [thingId, configKey, body] = setSpy.mock.calls[0];
    expect(thingId).toBe('node-gw-1');
    expect(configKey).toBe('killswitch');
    expect(body.state).toEqual({ engaged: true });
    expect(body.reason).toBe('Customer escalation #42');
    expect(body.expiresAt).toBeUndefined();

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('reason longer than 500 chars disables Save', async () => {
    const user = userEvent.setup();
    renderDrawer();
    const reason = screen.getByTestId('override-editor-reason') as HTMLInputElement;

    // userEvent.type is slow on long strings; use fireEvent-style direct write
    // by selecting + paste-like behaviour. The maxLength on the input is 501
    // so we can overshoot by 1 to trigger the guard message.
    await user.click(reason);
    const longReason = 'x'.repeat(501);
    // userEvent.paste matches the spec's clipboard path and bypasses maxLength
    await user.paste(longReason);

    expect(reason.value.length).toBeGreaterThan(500);
    expect(screen.getByTestId('override-editor-save')).toBeDisabled();
  });

  it('TTL preset selection is sent through as expiresAt on save', async () => {
    const user = userEvent.setup();
    renderDrawer();

    const ttl = screen.getByTestId('override-editor-ttl') as HTMLSelectElement;
    await user.selectOptions(ttl, 'h4');

    await user.click(screen.getByTestId('override-editor-save'));

    await waitFor(() => expect(setSpy).toHaveBeenCalledTimes(1));
    const [, , body] = setSpy.mock.calls[0];
    expect(body.expiresAt).toBeDefined();
    const ms = new Date(body.expiresAt as string).getTime() - Date.now();
    // 4h = 14_400_000ms; allow a generous fudge for test-run latency.
    expect(ms).toBeGreaterThan(4 * 60 * 60 * 1000 - 60_000);
    expect(ms).toBeLessThan(4 * 60 * 60 * 1000 + 60_000);
  });

  it('clicking the backdrop calls onClose', async () => {
    const user = userEvent.setup();
    const { onClose, container } = renderDrawer();
    // The backdrop is the first div with role=presentation in the DOM.
    const backdrop = container.querySelector('div[role="presentation"]');
    expect(backdrop).not.toBeNull();
    await user.click(backdrop as HTMLElement);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('Escape key closes the drawer', async () => {
    const user = userEvent.setup();
    const { onClose } = renderDrawer();
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('keeps the drawer open and surfaces the server error when setOverride rejects', async () => {
    const user = userEvent.setup();
    // Override the suite-wide success mock with a rejection carrying the
    // server message we expect to render verbatim. CP+Hub now forward 4xx
    // bodies through (S2 commit c58b1b60), so the message reaching the
    // browser layer is the canonical operator-facing text.
    const serverMsg = 'override.state must contain `engaged` boolean';
    setSpy.mockReset();
    setSpy.mockRejectedValue(new Error(serverMsg));

    const { onClose, onSaved } = renderDrawer();

    const ta = screen.getByTestId('override-editor-textarea') as HTMLTextAreaElement;
    await user.clear(ta);
    await user.click(ta);
    await user.paste('{"engaged":true}');

    await user.click(screen.getByTestId('override-editor-save'));

    // Spy was called exactly once.
    await waitFor(() => expect(setSpy).toHaveBeenCalledTimes(1));

    // Server error surfaces inline with the exact server text.
    const errorEl = await screen.findByTestId('override-editor-server-error');
    expect(errorEl).toHaveTextContent(serverMsg);

    // Drawer still mounted (not unmounted on error).
    expect(screen.getByRole('dialog')).toBeInTheDocument();

    // Neither onSaved nor onClose were invoked — the failed save is
    // strictly a no-op for the parent.
    expect(onSaved).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();

    // Save button is re-enabled so the user can retry without remounting.
    expect(screen.getByTestId('override-editor-save')).toBeEnabled();
  });
});
