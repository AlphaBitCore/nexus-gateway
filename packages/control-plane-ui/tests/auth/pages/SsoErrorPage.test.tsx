import { describe, it, expect } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import { Routes, Route } from 'react-router-dom';
import { renderWithRouter } from '@/test/test-utils';
import { SsoErrorPage } from '../../../src/auth/pages/SsoErrorPage';

function renderAt(query: string) {
  return renderWithRouter(
    <Routes>
      <Route path="/auth/sso-error" element={<SsoErrorPage />} />
      <Route path="/login" element={<div>login-stub</div>} />
    </Routes>,
    { route: `/auth/sso-error${query}` },
  );
}

describe('SsoErrorPage', () => {
  it('shows the OAuth error code from the callback redirect in the alert', () => {
    renderAt('?code=access_denied');
    expect(screen.getByRole('alert').textContent).toContain('access_denied');
  });

  it('replaces a code that is not a bare lowercase OAuth token with "unknown"', () => {
    // A crafted callback URL must not be able to reflect arbitrary text
    // onto this unauthenticated page.
    renderAt(`?code=${encodeURIComponent('<img src=x onerror=alert(1)>')}`);
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toContain('unknown');
    expect(alert.textContent).not.toContain('onerror');
  });

  it('falls back to "unknown" when the code param is missing', () => {
    renderAt('');
    expect(screen.getByRole('alert').textContent).toContain('unknown');
  });

  it('navigates back to the login page via "Sign in again"', () => {
    renderAt('?code=server_error');
    fireEvent.click(screen.getByRole('button', { name: /sign in again/i }));
    expect(screen.getByText('login-stub')).toBeInTheDocument();
  });
});
