import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { ErrorBanner } from '@/components/ui/ErrorBanner';
import { ApiError } from '@/api/client';

function renderBanner(props: React.ComponentProps<typeof ErrorBanner>) {
  return render(
    <I18nextProvider i18n={i18n}>
      <ErrorBanner {...props} />
    </I18nextProvider>,
  );
}

describe('ErrorBanner (CP wrapper)', () => {
  it('renders a plain message with a retry button for ordinary errors', () => {
    let retried = false;
    renderBanner({ message: 'boom', onRetry: () => (retried = true) });
    expect(screen.getByRole('alert')).toHaveTextContent('boom');
    screen.getByRole('button', { name: 'Retry' }).click();
    expect(retried).toBe(true);
  });

  it('derives the message from a thrown Error when no message prop is given', () => {
    renderBanner({ error: new Error('fetch exploded') });
    expect(screen.getByRole('alert')).toHaveTextContent('fetch exploded');
  });

  it('renders the permission-denied state without a retry button for a 403 ApiError', () => {
    const denied = new ApiError(403, 'IAM_ACCESS_DENIED', 'Access denied: admin:settings.read', 'authorization_error', {
      action: 'admin:settings.read',
      resource: 'nrn:nexus:cp:*:settings/x',
      reason: 'NoMatchingStatement',
    });
    renderBanner({ error: denied, onRetry: () => {} });
    const alert = screen.getByRole('alert');
    // Clear no-permission copy + the IAM action the admin would need to request.
    expect(alert).toHaveTextContent("You don't have permission to view this.");
    expect(alert).toHaveTextContent('admin:settings.read');
    // Retrying a denial is pointless — the button must NOT render even though
    // the caller passed onRetry.
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });

  it('keeps the retry button for a non-403 ApiError', () => {
    const err = new ApiError(500, 'INTERNAL', 'server exploded');
    renderBanner({ error: err, onRetry: () => {} });
    expect(screen.getByRole('alert')).toHaveTextContent('server exploded');
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });
});
