import type { ReactNode } from 'react';
import { Route } from 'react-router-dom';
import { RequireRole } from '@/auth/guards/RequireRole';
import { SHELL_ROUTES } from './shellRouteConfig';

/**
 * Returns Route elements for the authenticated Shell layout.
 * Must be spread inside <Routes> as {shellRoutes()} — not <ShellRoutes />.
 * React Router requires direct <Route> children, not custom components.
 *
 * Authentication is enforced at the Shell layout level (a single
 * <RequireAuth> wraps the entire authenticated subtree), so per-route
 * auth gating is gone — `allowedActions` is the only per-route gate
 * and runs the IAM permission check via RequireRole.
 */
export function shellRoutes(): ReactNode[] {
  return SHELL_ROUTES.map((r) => {
    const Page = r.LazyPage;
    let element: ReactNode = <Page />;
    if (r.allowedActions?.length) {
      element = <RequireRole allowedActions={r.allowedActions}>{element}</RequireRole>;
    }
    const key = r.index ? '__index' : r.path ?? '';
    if (r.index) {
      return <Route key={key} index element={element} />;
    }
    return <Route key={key} path={r.path} element={element} />;
  });
}
