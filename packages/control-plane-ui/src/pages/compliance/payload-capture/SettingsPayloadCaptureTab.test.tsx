/**
 * SettingsPayloadCaptureTab — renders the current config,
 * forces a confirmation modal when the admin turns a store-flag from
 * OFF to ON, and posts the merged config to the admin API on Save.
 */
import { describe, it, expect, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders, server, http, HttpResponse } from '@/test/test-utils';
import { SettingsPayloadCaptureTab } from './SettingsPayloadCaptureTab';

function renderTab() {
  return renderWithProviders(<SettingsPayloadCaptureTab />);
}

// getSwitchesByOrder returns the two capture switches in document order:
// [0] = request body, [1] = response body. The shared Switch wrapper does
// not forward arbitrary props (so aria-label never lands on the role=switch
// element); relying on order matches how sighted users perceive the tab
// top-to-bottom without over-specifying the DOM.
function getSwitchesByOrder(): HTMLElement[] {
  return screen.getAllByRole('switch');
}

describe('SettingsPayloadCaptureTab', () => {
  it('renders the current config from the API', async () => {
    server.use(
      http.get('/api/admin/settings/payload-capture', () =>
        HttpResponse.json({
          storeRequestBody: true,
          storeResponseBody: false,
          maxInlineBodyBytes: 131072,
          maxRequestBytes: 25 * 1024 * 1024,
          maxResponseBytes: 30 * 1024 * 1024,
        }),
      ),
    );

    renderTab();

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /payload capture/i })).toBeDefined();
    });

    const [requestSwitch, responseSwitch] = getSwitchesByOrder();
    expect(requestSwitch.getAttribute('aria-checked')).toBe('true');
    expect(responseSwitch.getAttribute('aria-checked')).toBe('false');

    // Three byte-cap inputs in document order: audit truncation cap,
    // inbound request network cap, upstream response network cap.
    const [auditInput, requestInput, responseInput] = screen.getAllByRole('spinbutton');
    expect((auditInput as HTMLInputElement).value).toBe('131072');
    expect((requestInput as HTMLInputElement).value).toBe(String(25 * 1024 * 1024));
    expect((responseInput as HTMLInputElement).value).toBe(String(30 * 1024 * 1024));

    // Streaming capture is now supported; the note advertises
    // coverage of both stream and non-stream responses.
    expect(
      screen.getByText(/response body capture covers both streaming \(SSE\) and non-streaming responses/i),
    ).toBeDefined();
  });

  it('opens a confirmation modal when enabling request capture and only flips after confirm', async () => {
    server.use(
      http.get('/api/admin/settings/payload-capture', () =>
        HttpResponse.json({
          storeRequestBody: false,
          storeResponseBody: false,
          maxInlineBodyBytes: 65536,
          maxRequestBytes: 10 * 1024 * 1024,
          maxResponseBytes: 10 * 1024 * 1024,
        }),
      ),
    );

    const user = userEvent.setup();
    renderTab();

    await screen.findByRole('heading', { name: /payload capture/i });
    const [requestSwitch] = getSwitchesByOrder();
    expect(requestSwitch.getAttribute('aria-checked')).toBe('false');

    await user.click(requestSwitch);

    // Switch should NOT flip until the user confirms.
    expect(requestSwitch.getAttribute('aria-checked')).toBe('false');

    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText(/enable payload storage/i)).toBeDefined();
    const confirmButton = within(dialog).getByRole('button', { name: /yes, enable capture/i });
    await user.click(confirmButton);

    await waitFor(() => {
      expect(requestSwitch.getAttribute('aria-checked')).toBe('true');
    });
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('aborts the flip when the user cancels the confirmation', async () => {
    server.use(
      http.get('/api/admin/settings/payload-capture', () =>
        HttpResponse.json({
          storeRequestBody: false,
          storeResponseBody: false,
          maxInlineBodyBytes: 65536,
          maxRequestBytes: 10 * 1024 * 1024,
          maxResponseBytes: 10 * 1024 * 1024,
        }),
      ),
    );
    const user = userEvent.setup();
    renderTab();

    await screen.findByRole('heading', { name: /payload capture/i });
    const [, responseSwitch] = getSwitchesByOrder();
    await user.click(responseSwitch);

    const dialog = await screen.findByRole('alertdialog');
    const cancel = within(dialog).getByRole('button', { name: /cancel/i });
    await user.click(cancel);

    await waitFor(() => {
      expect(screen.queryByRole('alertdialog')).toBeNull();
    });
    expect(responseSwitch.getAttribute('aria-checked')).toBe('false');
  });

  it('saves the merged config and refetches on success', async () => {
    const putSpy = vi.fn();
    server.use(
      http.get('/api/admin/settings/payload-capture', () =>
        HttpResponse.json({
          storeRequestBody: false,
          storeResponseBody: false,
          maxInlineBodyBytes: 65536,
          maxRequestBytes: 10 * 1024 * 1024,
          maxResponseBytes: 10 * 1024 * 1024,
        }),
      ),
      http.put('/api/admin/settings/payload-capture', async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        putSpy(body);
        return HttpResponse.json({
          storeRequestBody: Boolean(body.storeRequestBody),
          storeResponseBody: Boolean(body.storeResponseBody),
          maxInlineBodyBytes: Number(body.maxInlineBodyBytes) || 65536,
          maxRequestBytes: Number(body.maxRequestBytes) || 10 * 1024 * 1024,
          maxResponseBytes: Number(body.maxResponseBytes) || 10 * 1024 * 1024,
        });
      }),
    );

    const user = userEvent.setup();
    renderTab();

    await screen.findByRole('heading', { name: /payload capture/i });

    // Bump the audit cap (first spinbutton) and save without toggling
    // any flag — no modal.
    const [auditInput] = screen.getAllByRole('spinbutton');
    await user.clear(auditInput);
    await user.type(auditInput, '32768');

    const saveBtn = screen.getByRole('button', { name: /save/i });
    await user.click(saveBtn);

    await waitFor(() => {
      expect(putSpy).toHaveBeenCalledTimes(1);
    });
    const payload = putSpy.mock.calls[0][0] as Record<string, unknown>;
    expect(payload.maxInlineBodyBytes).toBe(32768);
    expect(payload.storeRequestBody).toBe(false);
    expect(payload.storeResponseBody).toBe(false);
    // The two network caps must round-trip the loaded values unchanged
    // even when only the audit cap was edited — the form must not
    // accidentally zero them.
    expect(payload.maxRequestBytes).toBe(10 * 1024 * 1024);
    expect(payload.maxResponseBytes).toBe(10 * 1024 * 1024);
  });
});
