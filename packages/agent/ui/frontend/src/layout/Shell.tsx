import type { ReactNode } from 'react';
import { NavLink } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { cn } from '@nexus-gateway/ui-shared';

/**
 * Window-level layout: left sidebar + scrollable main (prime-console shell pattern).
 */
export function Shell({ children }: { children: ReactNode }) {
  const { t } = useTranslation();

  const linkClass = ({ isActive }: { isActive: boolean }) =>
    cn(
      'block rounded-md px-3 py-2 text-sm font-medium transition-colors no-underline',
      isActive
        ? 'bg-sidebar-accent text-sidebar-accent-foreground'
        : 'text-sidebar-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
    );

  return (
    <div className="flex h-dvh w-full overflow-hidden bg-background text-foreground">
      <aside className="flex w-[var(--sidebar-width)] shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground">
        <div className="border-b border-sidebar-border px-4 py-4 font-sans text-sm font-semibold tracking-normal text-sidebar-foreground">
          Nexus Agent
        </div>
        <nav className="flex flex-1 flex-col gap-0.5 overflow-y-auto p-2">
          <NavLink to="/overview" className={linkClass}>{t('nav.overview')}</NavLink>
          <NavLink to="/activity" className={linkClass}>{t('nav.activity')}</NavLink>
          <NavLink to="/traffic" className={linkClass}>{t('nav.traffic')}</NavLink>
          <NavLink to="/policies" className={linkClass}>{t('nav.policies')}</NavLink>
          <NavLink to="/stats" className={linkClass}>{t('nav.stats')}</NavLink>
          <NavLink to="/diagnostics" className={linkClass}>{t('nav.diagnostics')}</NavLink>
          <NavLink to="/settings" className={linkClass}>{t('nav.settings')}</NavLink>
        </nav>
      </aside>
      <main className="page-shell min-h-0 min-w-0 flex-1 overflow-y-auto">
        <div className="page-shell min-h-full p-6">{children}</div>
      </main>
    </div>
  );
}
