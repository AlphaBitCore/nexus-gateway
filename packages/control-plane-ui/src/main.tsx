import React from 'react';
import ReactDOM from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { App } from './App';
import { ToastProvider } from './context/ToastContext';
import { ThemeProvider } from './theme/ThemeProvider';
import { ErrorBoundary } from '@/components/ui';
import { initGlobalErrorReporting, reportReactError } from './lib/errorReporting';

// i18n (must init before React renders)
import './i18n';

// Tailwind v4 + shadcn + prime-aligned tokens (see ui-shared prime-shadcn-tokens.css).
import './styles/tailwind-app.css';
// Legacy spacing/palette + semantic aliases (—color-* / —g-*) for existing CSS Modules.
import '@nexus-gateway/ui-shared/styles/global.css';
import '@nexus-gateway/ui-shared/styles/light.css';
import '@nexus-gateway/ui-shared/styles/dark.css';
import '@nexus-gateway/ui-shared/styles/animations.css';
import '@nexus-gateway/ui-shared/styles/utilities.css';

initGlobalErrorReporting();

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,       // 30s — data considered fresh
      retry: 1,                // retry once on failure
      refetchOnWindowFocus: true,
    },
  },
});

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ErrorBoundary level="app" onError={reportReactError}>
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <ToastProvider>
            <App />
          </ToastProvider>
        </ThemeProvider>
      </QueryClientProvider>
    </ErrorBoundary>
  </React.StrictMode>,
);

// Report Core Web Vitals
import('./lib/vitals').then(({ reportWebVitals }) => reportWebVitals());
