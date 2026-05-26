import { describe, it, expect, beforeEach } from 'vitest';
import { screen } from '@testing-library/react';
import { Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { ForgotPasswordPage } from '../pages/ForgotPasswordPage';
import { renderWithRouter, server } from '@/test/test-utils';

describe('ForgotPasswordPage', () => {
  beforeEach(() => {
    server.use(http.get('/api/admin/me', () => HttpResponse.json({ error: 'unauthorized' }, { status: 401 })));
  });

  it('renders guidance and back to sign in', async () => {
    renderWithRouter(
      <Routes>
        <Route path="/forgot-password" element={<ForgotPasswordPage />} />
      </Routes>,
      { route: '/forgot-password' },
    );

    expect(await screen.findByRole('heading', { name: /forgot password/i })).toBeDefined();
    expect(screen.getByText(/does not offer self-service password reset/i)).toBeDefined();
    expect(screen.getByRole('button', { name: /back to sign in/i })).toBeDefined();
  });
});
