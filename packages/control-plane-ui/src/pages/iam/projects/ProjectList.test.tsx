/**
 * Integration test — ProjectList renders project data and supports interactions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter } from '@/test/test-utils';
import { ProjectList } from './ProjectList';
import { mockProject } from '@/test/msw-handlers';

function renderProjectList() {
  return renderWithRouter(<ProjectList />);
}

describe('ProjectList', () => {
  it('renders page heading', async () => {
    renderProjectList();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('displays project data in table', async () => {
    renderProjectList();
    await waitFor(() => {
      expect(screen.getByText(mockProject.name)).toBeDefined();
    });
  });
});
