import { useState, useEffect, useCallback, useRef } from 'react';
import { Outlet } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { SetupBanner } from '../../SetupBanner';
import { cn } from '@/lib/utils';
import { Sidebar } from '../Sidebar';
import { Header } from '../Header';

/* ── Breakpoints ─────────────────────────────────────────────────────────── */

const BP_MOBILE = 768;
const BP_TABLET = 1024;

type Viewport = 'mobile' | 'tablet' | 'desktop';

function getViewport(width: number): Viewport {
  if (width < BP_MOBILE) return 'mobile';
  if (width < BP_TABLET) return 'tablet';
  return 'desktop';
}

/* ── Shell component (layout aligned with prime-console App + sidebar) ─── */

export function Shell() {
  const { t } = useTranslation();
  const [viewport, setViewport] = useState<Viewport>(() =>
    typeof window !== 'undefined' ? getViewport(window.innerWidth) : 'desktop',
  );
  const [collapsed, setCollapsed] = useState(false);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);

  const mainScrollRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    const onResize = () => setViewport(getViewport(window.innerWidth));
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);

  useEffect(() => {
    if (viewport !== 'mobile') setMobileMenuOpen(false);
  }, [viewport]);

  const isMobile = viewport === 'mobile';
  const effectiveCollapsed = viewport === 'tablet' ? true : collapsed;

  const toggleCollapsed = useCallback(() => setCollapsed((v) => !v), []);
  const toggleMobileMenu = useCallback(() => setMobileMenuOpen((v) => !v), []);
  const closeMobileMenu = useCallback(() => setMobileMenuOpen(false), []);

  return (
    <div className="flex h-dvh max-h-dvh w-full overflow-hidden bg-background text-foreground">
      <a
        href="#main-content"
        className={cn(
          'absolute left-4 z-[100] -top-full rounded-md border-2 border-primary bg-background px-4 py-2',
          'text-sm font-semibold text-foreground no-underline transition-[top] focus:top-2 focus:outline focus:outline-2 focus:outline-offset-2 focus:outline-primary',
        )}
      >
        {t('common:skipToMainContent')}
      </a>

      {isMobile && mobileMenuOpen && (
        <div
          className="fixed inset-0 z-40 bg-[var(--color-overlay)] transition-opacity duration-200"
          onClick={closeMobileMenu}
          aria-hidden="true"
        />
      )}

      <div
        data-testid="shell-nav"
        className={cn(
          'shrink-0',
          isMobile && 'fixed left-0 top-0 z-50 h-dvh -translate-x-[var(--sidebar-width)] transition-transform duration-200',
          isMobile && mobileMenuOpen && 'translate-x-0',
        )}
      >
        <Sidebar
          collapsed={isMobile ? false : effectiveCollapsed}
          onToggle={toggleCollapsed}
          onNavigate={isMobile ? closeMobileMenu : undefined}
        />
      </div>

      <div
        className={cn(
          'flex min-h-0 min-w-0 flex-1 flex-col bg-[var(--color-bg)] text-[var(--color-text)] transition-[margin] duration-200',
          !isMobile && 'ml-[var(--sidebar-width)]',
          !isMobile && effectiveCollapsed && 'ml-[var(--sidebar-width-icon)]',
          isMobile && 'ml-0',
        )}
      >
        {isMobile && (
          <Header
            isMobile={isMobile}
            onMenuToggle={toggleMobileMenu}
            scrollContainerRef={mainScrollRef}
          />
        )}

        <main
          id="main-content"
          ref={mainScrollRef}
          className={cn(
            'page-shell flex min-h-0 flex-1 flex-col overflow-y-auto scroll-smooth',
          )}
        >
          <div
            className={cn(
              'w-full px-8 py-6 pb-8',
              'md:px-8 md:pb-8',
              'max-md:px-4 max-md:pb-5 max-[768px]:px-3 max-[768px]:pb-5',
            )}
          >
            <SetupBanner />
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
