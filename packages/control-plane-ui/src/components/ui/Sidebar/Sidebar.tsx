import { useState } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { useAuth } from '@/auth/context/AuthContext';
import { useTheme } from '@/theme/useTheme';
import { buildSidebarNavSections } from '@/routes/shellRouteConfig';
import { NavIconForPath } from './Sidebar.icons';
import { CollapsibleList } from './Sidebar.CollapsibleList';
import { SidebarUserBlock } from './Sidebar.UserBlock';
import styles from './Sidebar.module.css';

/* ── Data model (aligned with shellRouteConfig nav metadata) ───────────────── */

interface NavItem {
  /** i18n key resolved via t('nav:...') at render time */
  labelKey: string;
  path: string;
  allowedActions?: string[];
  /** Regex sources whose match against the URL keeps this item active. */
  relatedPaths?: string[];
}

interface NavSection {
  /** i18n key resolved via t('nav:...') at render time */
  titleKey: string;
  collapsible: boolean;
  defaultOpen?: boolean;
  items: NavItem[];
}

const NAV_SECTIONS: NavSection[] = buildSidebarNavSections();
const ALL_ITEM_PATHS = NAV_SECTIONS.flatMap((s) => s.items.map((item) => item.path));

const STORAGE_KEY = 'nexus_sidebar_collapsed';

/* ── Helpers ──────────────────────────────────────────────────────────────── */

function itemMatchesRoute(pathname: string, item: NavItem): boolean {
  const itemPath = item.path;
  if (itemPath === '/') {
    return pathname === '/' || pathname === '';
  }
  if (pathname === itemPath) return true;
  if (pathname.startsWith(`${itemPath}/`)) {
    // Prefix match only when no more-specific nav item also covers this path.
    // Prevents /analytics from staying active when /analytics/cache-roi is current.
    const moreSpecific = ALL_ITEM_PATHS.some(
      (p) =>
        p !== itemPath &&
        p.startsWith(`${itemPath}/`) &&
        (pathname === p || pathname.startsWith(`${p}/`)),
    );
    if (!moreSpecific) return true;
  }
  // Drill-down pages may live outside the item's URL tree (e.g.
  // proxy-rollout → /infrastructure/nodes/:id/setup). relatedPaths keeps
  // those drilldowns highlighted on the landing entry.
  if (item.relatedPaths && item.relatedPaths.length > 0) {
    for (const src of item.relatedPaths) {
      try {
        if (new RegExp(src).test(pathname)) return true;
      } catch {
        // Bad regex in config — ignore so the sidebar still renders.
      }
    }
  }
  return false;
}

/* ── Single nav link ──────────────────────────────────────────────────────── */

function SidebarNavLink({ item, onNavigate }: { item: NavItem; onNavigate?: () => void }) {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const isActive = itemMatchesRoute(pathname, item);
  return (
    <NavLink
      to={item.path}
      onClick={onNavigate}
      className={clsx(styles.navLink, isActive && styles.navLinkActive)}
    >
      <NavIconForPath path={item.path} />
      <span className={styles.navLinkLabel}>{t(`nav:${item.labelKey}`)}</span>
    </NavLink>
  );
}

/* ── Sidebar props ────────────────────────────────────────────────────────── */

export interface SidebarProps {
  /** When true the sidebar is visually collapsed (width 0). Managed by the Shell. */
  collapsed?: boolean;
  /** Called when the sidebar requests a toggle (e.g. internal hamburger in a future iteration). */
  onToggle?: () => void;
  /** Called after a nav link is clicked (useful for closing mobile drawer). */
  onNavigate?: () => void;
}

/* ── Sidebar component ────────────────────────────────────────────────────── */

export function Sidebar({ collapsed = false, onToggle, onNavigate }: SidebarProps) {
  const { permissions } = useAuth();
  const { brand } = useTheme();
  const { t } = useTranslation();
  const location = useLocation();

  /* Section collapse state (persisted per-section, independent of sidebar collapsed prop) */
  const [sectionCollapsed, setSectionCollapsed] = useState<Record<string, boolean>>(() => {
    try {
      return JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '{}');
    } catch {
      return {};
    }
  });

  const toggleSection = (titleKey: string) => {
    const next = { ...sectionCollapsed, [titleKey]: !sectionCollapsed[titleKey] };
    setSectionCollapsed(next);
    localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
  };

  const isSectionOpen = (section: NavSection, visibleItems: NavItem[]): boolean => {
    if (!section.collapsible) return true;
    if (sectionCollapsed[section.titleKey] !== undefined) return !sectionCollapsed[section.titleKey];
    const hasActive = visibleItems.some((it) => itemMatchesRoute(location.pathname, it));
    if (hasActive) return true;
    return section.defaultOpen !== false;
  };

  const brandInitial = brand.productName.charAt(0).toUpperCase();
  const sidebarToggleLabel = t(collapsed ? 'nav:expandSidebar' : 'nav:collapseSidebar');
  const logoLockup = brand.logoMark && brand.logoFull && brand.logoTagline
    ? { mark: brand.logoMark, wordmark: brand.logoFull, tagline: brand.logoTagline }
    : null;

  return (
    <nav
      aria-label={t('common:mainNavigation')}
      className={clsx(
        styles.sidebar,
        collapsed && styles.collapsed,
        'anim-sidebar border-r border-sidebar-border bg-sidebar text-sidebar-foreground',
      )}
    >
      {/* Brand header */}
      <header className={clsx(styles.header, logoLockup && styles.logoLockupHeader)}>
        <div className={clsx(styles.brandRow, logoLockup && styles.logoLockupRow)}>
          {logoLockup ? (
            <>
              <div className={clsx(styles.brandMark, styles.logoLockupMark)} aria-hidden>
                <img src={logoLockup.mark} alt="" />
              </div>
              <div className={clsx(styles.brandText, styles.logoLockupText)}>
                <img
                  src={logoLockup.wordmark}
                  alt={brand.productName}
                  className={styles.logoLockupWordmark}
                />
                <img
                  src={logoLockup.tagline}
                  alt=""
                  className={styles.logoLockupTagline}
                  aria-hidden="true"
                />
              </div>
            </>
          ) : (
            <>
              <div className={styles.brandMark} aria-hidden>
                {brand.logoMark ? (
                  <img src={brand.logoMark} alt="" />
                ) : (
                  brandInitial
                )}
              </div>
              <div className={styles.brandText}>
                <div className={styles.brandName}>{brand.productName}</div>
                <div className={styles.brandSubtitle}>{t('nav:adminConsole')}</div>
              </div>
            </>
          )}
        </div>
      </header>
      {onToggle && (
        <button data-design-system-escape="primitive-internal"
          type="button"
          className={styles.collapseToggle}
          onClick={onToggle}
          aria-label={sidebarToggleLabel}
          title={sidebarToggleLabel}
        >
          <svg
            width="12" height="12" viewBox="0 0 24 24" fill="none"
            stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"
            className={clsx(styles.collapseIcon, collapsed && styles.collapseIconFlipped)}
            aria-hidden="true"
          >
            <path d="M15 18l-6-6 6-6" />
          </svg>
        </button>
      )}

      <div className={styles.divider} />

      {/* Navigation sections */}
      <div className={styles.body}>
        {NAV_SECTIONS.map((section, sectionIndex) => {
          const visibleItems = section.items.filter((item) => {
            if (!item.allowedActions) return true;
            return permissions.length > 0 && item.allowedActions.some(a => permissions.includes(a));
          });
          if (visibleItems.length === 0) return null;

          const open = isSectionOpen(section, visibleItems);

          return (
            <div key={section.titleKey} className={styles.section}>
              {section.collapsible ? (
                <button data-design-system-escape="primitive-internal"
                  type="button"
                  className={clsx(styles.sectionToggle, open && styles.sectionToggleOpen)}
                  onClick={() => toggleSection(section.titleKey)}
                  aria-expanded={open}
                >
                  <span>{t(`nav:${section.titleKey}`)}</span>
                  <svg
                    className={clsx(styles.chevron, open && styles.chevronOpen)}
                    width="14"
                    height="14"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    aria-hidden
                  >
                    <path d="M6 9l6 6 6-6" />
                  </svg>
                </button>
              ) : (
                <div
                  className={clsx(
                    styles.sectionLabel,
                    sectionIndex === 0 && styles.sectionLabelFirst,
                  )}
                >
                  {t(`nav:${section.titleKey}`)}
                </div>
              )}

              <CollapsibleList open={open}>
                <ul className={styles.navList}>
                  {visibleItems.map((item) => (
                    <li key={item.path}>
                      <SidebarNavLink item={item} onNavigate={onNavigate} />
                    </li>
                  ))}
                </ul>
              </CollapsibleList>
            </div>
          );
        })}
      </div>

      <SidebarUserBlock collapsed={collapsed} />
    </nav>
  );
}
