import { Suspense } from 'react';
import { BrowserRouter, Routes, Route } from 'react-router-dom';
import { AuthProvider } from './auth/context/AuthContext';
import { RequireAuth } from './auth/guards/RequireAuth';
import { TimeRangeProvider } from './context/TimeRangeContext';
import { Shell, ErrorBoundary } from '@/components/ui';
import { TopLoader } from '@/components/ui/TopLoader';
import { reportReactError } from './lib/errorReporting';
import { LazyCallbackPage, LazyForgotPasswordPage, LazyLoginPage, LazyNotFoundPage, LazySsoErrorPage } from './routes/lazyPages';
import { shellRoutes } from './routes/ShellRoutes';

export function App() {
  return (
    <BrowserRouter>
      <AuthProvider>
        <TimeRangeProvider>
        <ErrorBoundary level="route" onError={reportReactError}>
          <Suspense fallback={<TopLoader />}>
            <Routes>
              <Route path="/login" element={<LazyLoginPage />} />
              <Route path="/auth/callback" element={<LazyCallbackPage />} />
              <Route path="/auth/sso-error" element={<LazySsoErrorPage />} />
              <Route path="/forgot-password" element={<LazyForgotPasswordPage />} />
              <Route element={<RequireAuth><Shell /></RequireAuth>}>
                {shellRoutes()}
              </Route>
              <Route path="*" element={<LazyNotFoundPage />} />
            </Routes>
          </Suspense>
        </ErrorBoundary>
        </TimeRangeProvider>
      </AuthProvider>
    </BrowserRouter>
  );
}
