/**
 * Integration tests — ObservabilityRetention.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import ObservabilityRetention from '../../../../src/pages/infrastructure/observability-retention/ObservabilityRetention';

function renderPage() {
  return renderWithRouter(<ObservabilityRetention />);
}

const allLayers = [
  'runtime_5m', 'runtime_1h', 'runtime_1d', 'runtime_1mo',
  'business_5m', 'business_1h', 'business_1d', 'business_1mo',
  'diag_info', 'diag_warn', 'diag_error', 'diag_fatal',
];

const seedRetention = {
  retention: {
    runtime_5m: { value: 7, min: 1, max: 30 },
    runtime_1h: { value: 90, min: 30, max: 365 },
    runtime_1d: { value: 365, min: 90, max: 1095 },
    runtime_1mo: { value: 1825, min: 365, max: 3650 },
    business_5m: { value: 7, min: 1, max: 30 },
    business_1h: { value: 90, min: 30, max: 365 },
    business_1d: { value: 365, min: 90, max: 1095 },
    business_1mo: { value: 1825, min: 365, max: 3650 },
    diag_info: { value: 14, min: 1, max: 90 },
    diag_warn: { value: 30, min: 7, max: 90 },
    diag_error: { value: 180, min: 30, max: 730 },
    diag_fatal: { value: 365, min: 90, max: 1825 },
  },
};

describe('ObservabilityRetention', () => {
  it('TestRetentionPage_RendersAllTwelveLayers', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () => HttpResponse.json(seedRetention)),
    );

    renderPage();

    await waitFor(() => {
      for (const layer of allLayers) {
        expect(screen.getByLabelText(layer)).toBeDefined();
      }
    });
  });

  it('TestRetentionPage_ValidatesRange', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () => HttpResponse.json(seedRetention)),
    );

    const user = userEvent.setup();
    renderPage();

    const input = await screen.findByLabelText('runtime_5m');
    await user.clear(input);
    await user.type(input, '999'); // out of range (max 30)

    await waitFor(() => {
      // Inline error message surfaces (role=alert).
      expect(screen.getByText(/must be between 1 and 30 days/i)).toBeDefined();
    });

    const save = screen.getByRole('button', { name: /^save$/i });
    expect((save as HTMLButtonElement).disabled).toBe(true);
  });

  it('TestRetentionPage_SavesOnlyChangedLayers', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () => HttpResponse.json(seedRetention)),
    );

    let putBody: unknown = null;
    server.use(
      http.put('/api/admin/observability/retention', async ({ request }) => {
        putBody = await request.json();
        return HttpResponse.json({ ok: true, updated: 1 });
      }),
    );

    const user = userEvent.setup();
    renderPage();

    const input = await screen.findByLabelText('runtime_5m');
    await user.clear(input);
    await user.type(input, '14');

    const save = screen.getByRole('button', { name: /^save$/i });
    await waitFor(() => {
      expect((save as HTMLButtonElement).disabled).toBe(false);
    });
    await user.click(save);

    await waitFor(() => {
      expect(putBody).not.toBeNull();
      const body = putBody as Record<string, number>;
      // Only the diverged key is present.
      expect(body).toEqual({ runtime_5m: 14 });
    });
  });

  it('TestRetentionPage_ResetToDefaults', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () =>
        HttpResponse.json({
          retention: {
            ...seedRetention.retention,
            // Pre-existing non-default value so we can see reset overwrite it.
            runtime_5m: { value: 28, min: 1, max: 30 },
          },
        }),
      ),
    );

    let putBody: unknown = null;
    server.use(
      http.put('/api/admin/observability/retention', async ({ request }) => {
        putBody = await request.json();
        return HttpResponse.json({ ok: true, updated: 11 });
      }),
    );

    const user = userEvent.setup();
    renderPage();

    await screen.findByLabelText('runtime_5m');

    await user.click(screen.getByRole('button', { name: /reset/i }));
    const dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: /reset/i }));

    await waitFor(() => {
      expect(putBody).not.toBeNull();
      const body = putBody as Record<string, number>;
      expect(body.runtime_5m).toBe(7);
      expect(body.runtime_1h).toBe(90);
      expect(body.runtime_1d).toBe(365);
      expect(body.runtime_1mo).toBe(1825);
      expect(body.business_5m).toBe(7);
      expect(body.business_1h).toBe(90);
      expect(body.business_1d).toBe(365);
      expect(body.business_1mo).toBe(1825);
      expect(body.diag_info).toBe(14);
      expect(body.diag_warn).toBe(30);
      expect(body.diag_error).toBe(180);
      expect(body.diag_fatal).toBe(365);
    });
  });

  // ── A layer the server has NO value for ────────────────────────────────
  //
  // The page filled the box with the spec default and treated that default as
  // the layer's current value. Two consequences, and the second is what makes
  // the page unusable rather than merely misleading:
  //
  //   1. The operator reads a retention window that is not in force. This is
  //      the surface whose entire job is saying how long data is kept.
  //   2. "Changed" compared the box against that same default, so the layer
  //      counted as unchanged and Save was DISABLED — on exactly the layers
  //      that had never been configured. The number was visible and could not
  //      be committed.

  it('TestRetentionPage_UnconfiguredLayer_IsNotPresentedAsInForce', async () => {
    const partial = { retention: { ...seedRetention.retention } };
    delete (partial.retention as Record<string, unknown>).diag_warn;
    server.use(
      http.get('/api/admin/observability/retention', () => HttpResponse.json(partial)),
    );

    renderPage();

    const input = await screen.findByLabelText('diag_warn');
    // The box still shows the default so the row is usable...
    expect((input as HTMLInputElement).value).toBe('30');
    // ...but it is marked as NOT a setting in force.
    await waitFor(() => {
      expect(input.getAttribute('data-unconfigured')).toBe('true');
    });
    // And a configured sibling is not marked.
    expect(screen.getByLabelText('diag_error').getAttribute('data-unconfigured')).toBeNull();
  });

  it('TestRetentionPage_UnconfiguredLayer_CanBeSavedWithoutEditingIt', async () => {
    const partial = { retention: { ...seedRetention.retention } };
    delete (partial.retention as Record<string, unknown>).diag_warn;

    let putBody: unknown = null;
    server.use(
      http.get('/api/admin/observability/retention', () => HttpResponse.json(partial)),
      http.put('/api/admin/observability/retention', async ({ request }) => {
        putBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );

    const user = userEvent.setup();
    renderPage();

    await screen.findByLabelText('diag_warn');

    // Save must be enabled with NO edit at all — persisting the default is a
    // real change from "nothing stored".
    const save = await screen.findByRole('button', { name: /^save$/i });
    await waitFor(() => {
      expect((save as HTMLButtonElement).disabled).toBe(false);
    });

    await user.click(save);
    await waitFor(() => {
      expect(putBody).not.toBeNull();
    });
    expect(JSON.stringify(putBody)).toContain('diag_warn');
  });

  // The sibling: when every layer IS configured and nothing is edited, Save
  // stays disabled. Without this, "always enable Save" would satisfy the arm
  // above while removing the dirty-tracking the page relies on.
  it('TestRetentionPage_AllConfiguredAndUntouched_SaveStaysDisabled', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () => HttpResponse.json(seedRetention)),
    );

    renderPage();
    await screen.findByLabelText('diag_warn');

    const save = await screen.findByRole('button', { name: /^save$/i });
    await waitFor(() => {
      expect((save as HTMLButtonElement).disabled).toBe(true);
    });
  });
});
