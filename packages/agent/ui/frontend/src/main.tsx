import React from 'react';
import ReactDOM from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { HashRouter } from 'react-router-dom';

// Tailwind v4 + shadcn + prime-aligned tokens.
import './styles/tailwind-app.css';
import '@nexus-gateway/ui-shared/styles/global.css';
import '@nexus-gateway/ui-shared/styles/light.css';
import '@nexus-gateway/ui-shared/styles/dark.css';
import '@nexus-gateway/ui-shared/styles/animations.css';
import '@nexus-gateway/ui-shared/styles/utilities.css';

import './i18n';
import { App } from './app/App';
import { ThemeProvider } from './theme/ThemeProvider';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The agent socket is local and fast; refetching on focus is
      // cheap and keeps the Dashboard reactive when the user
      // switches windows. A 2-second staleTime mirrors the menu
      // bar's poll rate so the two surfaces don't fight.
      staleTime: 2_000,
      refetchOnWindowFocus: true,
      retry: 1,
    },
  },
});

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    {/* QueryClient wraps ThemeProvider so the provider can subscribe to
        the agent's GET_APPLIED_CONFIG via useQuery — when the Hub pushes
        a fleet-wide themeId through the agent_settings shadow, the
        Dashboard hot-switches to that theme without an operator action. */}
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <HashRouter>
          <App />
        </HashRouter>
      </ThemeProvider>
    </QueryClientProvider>
  </React.StrictMode>,
);
