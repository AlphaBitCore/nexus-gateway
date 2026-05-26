/**
 * Integration test — HookDetail renders hook name, details, and tabs.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { Routes, Route } from 'react-router-dom';

import { renderWithRouter } from '@/test/test-utils';
import { HookDetail } from './HookDetail';
import { mockHook } from '@/test/msw-handlers';

function renderHookDetail() {
  return renderWithRouter(
    <Routes>
      <Route path="/compliance/hooks/:id" element={<HookDetail />} />
    </Routes>,
    { route: '/compliance/hooks/hook-1' },
  );
}

describe('HookDetail', () => {
  it('renders hook name and details', async () => {
    renderHookDetail();
    await waitFor(() => {
      expect(screen.getAllByText(mockHook.name).length).toBeGreaterThan(0);
    });
  });

  it('shows tabs (Overview, Configuration, Pipeline, Test)', async () => {
    renderHookDetail();
    await waitFor(() => {
      expect(screen.getByText(/overview/i)).toBeDefined();
      expect(screen.getAllByText(/configuration/i).length).toBeGreaterThan(0);
      expect(screen.getByText(/pipeline/i)).toBeDefined();
      expect(screen.getByText(/test/i)).toBeDefined();
    });
  });
});
