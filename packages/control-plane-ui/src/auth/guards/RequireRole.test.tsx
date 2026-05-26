import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { RequireRole } from '../guards/RequireRole';

const mockStatus = vi.fn().mockReturnValue('authenticated');
const mockPermissions = vi.fn().mockReturnValue([]);
vi.mock('../context/AuthContext', () => ({
  useAuth: () => ({
    status: mockStatus(),
    permissions: mockPermissions(),
    roles: [],
    login: vi.fn(),
    logout: vi.fn(),
    completeLogin: vi.fn(),
    refreshSession: vi.fn(),
  }),
}));

function renderInRouter(ui: React.ReactElement) {
  return render(
    <MemoryRouter>
      <I18nextProvider i18n={i18n}>{ui}</I18nextProvider>
    </MemoryRouter>,
  );
}

describe('RequireRole', () => {
  it('renders children when the required IAM action is in permissions', () => {
    mockStatus.mockReturnValue('authenticated');
    mockPermissions.mockReturnValue(['admin:provider.create']);
    renderInRouter(
      <RequireRole allowedActions={['admin:provider.create']}>
        <div>protected-content</div>
      </RequireRole>,
    );
    expect(screen.getByText('protected-content')).toBeDefined();
  });

  it('shows access denied when authenticated but required action is not in permissions', () => {
    mockStatus.mockReturnValue('authenticated');
    mockPermissions.mockReturnValue(['admin:provider.read']);
    renderInRouter(
      <RequireRole allowedActions={['admin:provider.create']}>
        <div>protected-content</div>
      </RequireRole>,
    );
    expect(screen.queryByText('protected-content')).toBeNull();
    expect(screen.getByRole('heading')).toBeDefined();
  });

  it('shows loading state while session is resolving', () => {
    mockStatus.mockReturnValue('loading');
    mockPermissions.mockReturnValue([]);
    const { container } = renderInRouter(
      <RequireRole allowedActions={['admin:provider.create']}>
        <div>protected-content</div>
      </RequireRole>,
    );
    expect(screen.queryByText('protected-content')).toBeNull();
    expect(container.querySelector('[role="status"]')).toBeDefined();
  });

  it('shows access denied when permissions is empty', () => {
    mockStatus.mockReturnValue('authenticated');
    mockPermissions.mockReturnValue([]);
    renderInRouter(
      <RequireRole allowedActions={['admin:provider.create']}>
        <div>protected-content</div>
      </RequireRole>,
    );
    expect(screen.queryByText('protected-content')).toBeNull();
  });
});
