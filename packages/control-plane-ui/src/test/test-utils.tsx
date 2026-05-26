/**
 * Test utilities — render wrapper with all providers.
 *
 * Usage:
 *   import { renderWithProviders } from '@/test/test-utils';
 *   const { getByText } = renderWithProviders(<MyComponent />);
 */
import { type ReactNode } from 'react';
import { render, type RenderOptions } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import { AuthProvider } from '../auth/context/AuthContext';
import { ToastProvider } from '../context/ToastContext';
import { ThemeProvider } from '../theme/ThemeProvider';
import i18n from '../i18n';

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: 0,
        gcTime: 0,
      },
      mutations: {
        retry: false,
      },
    },
  });
}

function AllProviders({ children }: { children: ReactNode }) {
  const queryClient = createTestQueryClient();
  return (
    <QueryClientProvider client={queryClient}>
      <I18nextProvider i18n={i18n}>
        <ThemeProvider>
          <AuthProvider>
            <ToastProvider>
              {children}
            </ToastProvider>
          </AuthProvider>
        </ThemeProvider>
      </I18nextProvider>
    </QueryClientProvider>
  );
}

/**
 * Render with all required providers (QueryClient, i18n, Auth, Toast).
 * Does NOT include Router — wrap your component in <MemoryRouter> if needed.
 */
export function renderWithProviders(
  ui: React.ReactElement,
  options?: Omit<RenderOptions, 'wrapper'>,
) {
  return render(ui, { wrapper: AllProviders, ...options });
}

/**
 * Render with all providers + MemoryRouter.
 */
export function renderWithRouter(
  ui: React.ReactElement,
  { route = '/', ...options }: Omit<RenderOptions, 'wrapper'> & { route?: string } = {},
) {
  return renderWithProviders(
    <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>,
    options,
  );
}

export { server } from './msw-server';
export { http, HttpResponse } from 'msw';
