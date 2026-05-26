/**
 * Integration tests — ObservabilityRetention.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import ObservabilityRetention from './ObservabilityRetention';

function renderPage() {
  return renderWithRouter(<ObservabilityRetention />);
}

const allLayers = [
  'runtime_raw', 'runtime_1h', 'runtime_1d', 'runtime_1mo',
  'business_raw', 'business_1h', 'business_1d', 'business_1mo',
  'diag_warn', 'diag_error', 'diag_fatal',
];

const seedRetention = {
  retention: {
    runtime_raw: { value: 7, min: 1, max: 30 },
    runtime_1h: { value: 90, min: 30, max: 365 },
    runtime_1d: { value: 365, min: 90, max: 1095 },
    runtime_1mo: { value: 1825, min: 365, max: 3650 },
    business_raw: { value: 7, min: 1, max: 30 },
    business_1h: { value: 90, min: 30, max: 365 },
    business_1d: { value: 365, min: 90, max: 1095 },
    business_1mo: { value: 1825, min: 365, max: 3650 },
    diag_warn: { value: 30, min: 7, max: 90 },
    diag_error: { value: 180, min: 30, max: 730 },
    diag_fatal: { value: 365, min: 90, max: 1825 },
  },
};

describe('ObservabilityRetention', () => {
  it('TestRetentionPage_RendersAllElevenLayers', async () => {
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

    const input = await screen.findByLabelText('runtime_raw');
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

    const input = await screen.findByLabelText('runtime_raw');
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
      expect(body).toEqual({ runtime_raw: 14 });
    });
  });

  it('TestRetentionPage_ResetToDefaults', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () =>
        HttpResponse.json({
          retention: {
            ...seedRetention.retention,
            // Pre-existing non-default value so we can see reset overwrite it.
            runtime_raw: { value: 28, min: 1, max: 30 },
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

    await screen.findByLabelText('runtime_raw');

    await user.click(screen.getByRole('button', { name: /reset/i }));
    const dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: /reset/i }));

    await waitFor(() => {
      expect(putBody).not.toBeNull();
      const body = putBody as Record<string, number>;
      expect(body.runtime_raw).toBe(7);
      expect(body.runtime_1h).toBe(90);
      expect(body.runtime_1d).toBe(365);
      expect(body.runtime_1mo).toBe(1825);
      expect(body.business_raw).toBe(7);
      expect(body.business_1h).toBe(90);
      expect(body.business_1d).toBe(365);
      expect(body.business_1mo).toBe(1825);
      expect(body.diag_warn).toBe(30);
      expect(body.diag_error).toBe(180);
      expect(body.diag_fatal).toBe(365);
    });
  });
});
