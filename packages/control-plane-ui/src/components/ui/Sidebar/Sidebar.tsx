import { useState, useRef, useEffect, useCallback, type SVGProps } from 'react';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { useAuth } from '@/auth/context/AuthContext';
import { useTheme } from '@/theme/useTheme';
import { SUPPORTED_LANGUAGES, LANGUAGE_STORAGE_KEY } from '@/i18n';
import { alertsApi } from '@/api/services/alerts/alerts';
import { buildSidebarNavSections } from '@/routes/shellRouteConfig';
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from '../DropdownMenu';
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
const ALERT_POLL_INTERVAL_MS = 60_000;

const THEME_PACK_OPTIONS = [
  { value: 'default', label: 'Default' },
  { value: 'morningstar', label: 'Morningstar' },
  { value: 'rbc', label: 'RBC Royal Bank' },
] as const;

const MODE_OPTIONS = [
  { value: 'light' as const, labelKey: 'themeLight', icon: 'sun' },
  { value: 'dark' as const, labelKey: 'themeDark', icon: 'moon' },
  { value: 'system' as const, labelKey: 'themeSystem', icon: 'monitor' },
];

type UserMenuSection = 'theme' | 'skin' | 'language';

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

/* ── SVG icon wrapper ─────────────────────────────────────────────────────── */

function Icon(props: SVGProps<SVGSVGElement>) {
  const { className, children, ...rest } = props;
  return (
    <svg
      className={clsx(styles.navIcon, className)}
      width={16}
      height={16}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
      {...rest}
    >
      {children}
    </svg>
  );
}

function MenuIcon({ name }: { name: string }) {
  switch (name) {
    case 'bell':
      return (
        <Icon className={styles.menuIcon}>
          <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
          <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        </Icon>
      );
    case 'sun':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" />
        </Icon>
      );
    case 'moon':
      return (
        <Icon className={styles.menuIcon}>
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79Z" />
        </Icon>
      );
    case 'monitor':
      return (
        <Icon className={styles.menuIcon}>
          <rect x="2" y="3" width="20" height="14" rx="2" />
          <path d="M8 21h8M12 17v4" />
        </Icon>
      );
    case 'palette':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="13.5" cy="6.5" r="2.5" />
          <circle cx="17.5" cy="10.5" r="1.5" />
          <circle cx="8.5" cy="7.5" r="1.5" />
          <circle cx="6.5" cy="12.5" r="1.5" />
          <path d="M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.93 0 1.65-.75 1.65-1.69 0-.44-.18-.84-.44-1.13-.29-.29-.44-.65-.44-1.13 0-.92.75-1.66 1.67-1.66h2c3.05 0 5.56-2.5 5.56-5.55C21.97 6.01 17.46 2 12 2Z" />
        </Icon>
      );
    case 'globe':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="12" r="10" />
          <path d="M2 12h20M12 2a15 15 0 0 1 4 10 15 15 0 0 1-4 10 15 15 0 0 1-4-10 15 15 0 0 1 4-10Z" />
        </Icon>
      );
    case 'user':
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="8" r="4" />
          <path d="M4 21c0-4 4-7 8-7s8 3 8 7" />
        </Icon>
      );
    case 'logout':
      return (
        <Icon className={styles.menuIcon}>
          <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
          <path d="M16 17l5-5-5-5M21 12H9" />
        </Icon>
      );
    default:
      return (
        <Icon className={styles.menuIcon}>
          <circle cx="12" cy="12" r="4" />
        </Icon>
      );
  }
}

type NavIconName =
  | 'activity'
  | 'bookmark'
  | 'bolt'
  | 'building'
  | 'chart'
  | 'circle-check'
  | 'clock'
  | 'cog'
  | 'cube'
  | 'deviceSquare'
  | 'dot'
  | 'file'
  | 'fileCheck'
  | 'folder'
  | 'globe'
  | 'grid'
  | 'key'
  | 'key-2'
  | 'layers'
  | 'link'
  | 'monitor'
  | 'pencil'
  | 'play'
  | 'power'
  | 'refresh'
  | 'route'
  | 'server'
  | 'shield-check'
  | 'users';

function SidebarIconGlyph({ name }: { name: NavIconName }) {
  switch (name) {
    case 'grid':
      return (
        <Icon>
          <rect x="3" y="3" width="7" height="7" rx="1" />
          <rect x="14" y="3" width="7" height="7" rx="1" />
          <rect x="3" y="14" width="7" height="7" rx="1" />
          <rect x="14" y="14" width="7" height="7" rx="1" />
        </Icon>
      );
    case 'activity':
      return (
        <Icon>
          <path d="M3 12l4-4 4 6 4-2 6 4" />
        </Icon>
      );
    case 'chart':
      return (
        <Icon>
          <path d="M3 3v18h18" />
          <path d="M8 17V9" />
          <path d="M13 17v-5" />
          <path d="M18 17V5" />
        </Icon>
      );
    case 'clock':
      return (
        <Icon>
          <circle cx="12" cy="12" r="9" />
          <path d="M12 7v5l3 2" />
        </Icon>
      );
    case 'layers':
      return (
        <Icon>
          <path d="M3 7l9-4 9 4-9 4-9-4z" />
          <path d="M3 12l9 4 9-4" />
          <path d="M3 17l9 4 9-4" />
        </Icon>
      );
    case 'cog':
      return (
        <Icon>
          <circle cx="12" cy="12" r="3" />
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
        </Icon>
      );
    case 'key':
      return (
        <Icon>
          <circle cx="8" cy="15" r="4" />
          <path d="M11 12l9-9 3 3-3 3 2 2-3 3-2-2-2 2-3-3" />
        </Icon>
      );
    case 'key-2':
      return (
        <Icon>
          <path d="M21 2l-2 2-3-3-7 7 5 5 7-7-3-3z" />
          <path d="M3 21h18" />
        </Icon>
      );
    case 'shield-check':
      return (
        <Icon>
          <path d="M12 2l8 4v6c0 5-3.5 9-8 10-4.5-1-8-5-8-10V6z" />
          <path d="M9 12l2 2 4-4" />
        </Icon>
      );
    case 'route':
      return (
        <Icon>
          <path d="M9 17h6" />
          <path d="M3 7h18" />
          <path d="M5 7v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V7" />
          <path d="M12 11v6" />
        </Icon>
      );
    case 'circle-check':
      return (
        <Icon>
          <circle cx="12" cy="12" r="10" />
          <path d="M9 12l2 2 4-4" />
        </Icon>
      );
    case 'bookmark':
      return (
        <Icon>
          <path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z" />
        </Icon>
      );
    case 'file':
      return (
        <Icon>
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
          <polyline points="14 2 14 8 20 8" />
        </Icon>
      );
    case 'bolt':
      return (
        <Icon>
          <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />
        </Icon>
      );
    case 'globe':
      return (
        <Icon>
          <circle cx="12" cy="12" r="10" />
          <line x1="2" y1="12" x2="22" y2="12" />
          <path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z" />
        </Icon>
      );
    case 'dot':
      return (
        <Icon>
          <circle cx="12" cy="12" r="5" />
        </Icon>
      );
    case 'link':
      return (
        <Icon>
          <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
          <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
        </Icon>
      );
    case 'cube':
      return (
        <Icon>
          <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
          <polyline points="3.27 6.96 12 12.01 20.73 6.96" />
          <line x1="12" y1="22.08" x2="12" y2="12" />
        </Icon>
      );
    case 'deviceSquare':
      return (
        <Icon>
          <rect x="3" y="3" width="18" height="18" rx="2" />
          <rect x="8" y="8" width="8" height="8" rx="1" />
        </Icon>
      );
    case 'server':
      return (
        <Icon>
          <rect x="2" y="3" width="20" height="8" rx="2" />
          <rect x="2" y="13" width="20" height="8" rx="2" />
          <line x1="6" y1="7" x2="6.01" y2="7" />
          <line x1="6" y1="17" x2="6.01" y2="17" />
        </Icon>
      );
    case 'refresh':
      return (
        <Icon>
          <polyline points="23 4 23 10 17 10" />
          <polyline points="1 20 1 14 7 14" />
          <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10" />
          <path d="M20.49 15a9 9 0 0 1-14.85 3.36L1 14" />
        </Icon>
      );
    case 'pencil':
      return (
        <Icon>
          <path d="M12 20h9" />
          <path d="M16.5 3.5a2.121 2.121 0 1 1 3 3L7 19l-4 1 1-4z" />
        </Icon>
      );
    case 'power':
      return (
        <Icon>
          <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
          <line x1="12" y1="2" x2="12" y2="12" />
        </Icon>
      );
    case 'folder':
      return (
        <Icon>
          <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" />
        </Icon>
      );
    case 'play':
      return (
        <Icon>
          <polygon points="6 3 20 12 6 21 6 3" />
        </Icon>
      );
    case 'fileCheck':
      return (
        <Icon>
          <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
          <polyline points="14 2 14 8 20 8" />
          <polyline points="9 14 11 16 15 12" />
        </Icon>
      );
    case 'users':
      return (
        <Icon>
          <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
          <circle cx="9" cy="7" r="4" />
          <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
          <path d="M16 3.13a4 4 0 0 1 0 7.75" />
        </Icon>
      );
    case 'building':
      return (
        <Icon>
          <rect x="4" y="3" width="16" height="18" rx="1" />
          <path d="M9 8h2M9 12h2M9 16h2M14 8h2M14 12h2M14 16h2" />
        </Icon>
      );
    case 'monitor':
      return (
        <Icon>
          <rect x="2" y="3" width="20" height="14" rx="2" />
          <line x1="8" y1="21" x2="16" y2="21" />
          <line x1="12" y1="17" x2="12" y2="21" />
        </Icon>
      );
  }
  return null;
}

function navIconNameForPath(path: string): NavIconName {
  switch (path) {
    case '/':
      return 'grid';
    case '/traffic':
    case '/status':
      return 'activity';
    case '/analytics':
      return 'chart';
    case '/quota-usage':
    case '/infrastructure/jobs':
      return 'clock';
    case '/cache-roi':
    case '/iam/roles':
      return 'layers';
    case '/ai-gateway/providers':
      return 'cog';
    case '/ai-gateway/credentials':
      return 'key';
    case '/ai-gateway/credential-reliability':
    case '/compliance/exemptions':
      return 'shield-check';
    case '/ai-gateway/routing':
      return 'route';
    case '/ai-gateway/virtual-keys':
      return 'key-2';
    case '/ai-gateway/quota-policies':
      return 'circle-check';
    case '/ai-gateway/quota-overrides':
      return 'bookmark';
    case '/ai-gateway/cache':
      return 'file';
    case '/iam/policies':
      return 'fileCheck';
    case '/ai-gateway/passthrough':
      return 'bolt';
    case '/compliance/hooks':
      return 'link';
    case '/compliance/rule-packs':
      return 'cube';
    case '/devices':
      return 'monitor';
    case '/devices/groups':
    case '/iam/users':
      return 'users';
    case '/devices/device-defaults':
      return 'deviceSquare';
    case '/infrastructure/nodes':
      return 'server';
    case '/infrastructure/config-sync':
      return 'refresh';
    case '/infrastructure/overrides':
      return 'pencil';
    case '/infrastructure/observability-config':
    case '/infrastructure/kill-switch':
      return 'power';
    case '/iam/organizations':
      return 'building';
    case '/iam/projects':
      return 'folder';
    case '/iam/simulator':
      return 'play';
    case '/iam/identity-providers':
      return 'globe';
    default:
      return 'dot';
  }
}

function NavIconForPath({ path }: { path: string }) {
  return <SidebarIconGlyph name={navIconNameForPath(path)} />;
}

/* ── Collapsible list (animated max-height) ───────────────────────────────── */

function CollapsibleList({ open, children }: { open: boolean; children: React.ReactNode }) {
  const innerRef = useRef<HTMLDivElement>(null);
  const [maxHeight, setMaxHeight] = useState<string>(open ? 'none' : '0px');

  useEffect(() => {
    if (open) {
      const h = innerRef.current?.scrollHeight ?? 0;
      setMaxHeight(`${h}px`);
      const timer = setTimeout(() => setMaxHeight('none'), 220);
      return () => clearTimeout(timer);
    }
    if (innerRef.current) {
      setMaxHeight(`${innerRef.current.scrollHeight}px`);

      innerRef.current.offsetHeight;
    }
    requestAnimationFrame(() => setMaxHeight('0px'));
  }, [open]);

  return (
    <div
      ref={innerRef}
      className={clsx(styles.collapsibleWrapper, maxHeight === 'none' && styles.collapsibleNone)}
      style={{ maxHeight }}
    >
      {children}
    </div>
  );
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
                  className={styles.sectionToggle}
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

function SidebarAlertMenuItem() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [count, setCount] = useState(0);

  const fetchCount = useCallback(async () => {
    try {
      const res = await alertsApi.list({ state: ['firing'], limit: 1 });
      setCount(res.total ?? 0);
    } catch {
      setCount(0);
    }
  }, []);

  useEffect(() => {
    void fetchCount();
    const timer = setInterval(() => void fetchCount(), ALERT_POLL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [fetchCount]);

  return (
    <DropdownMenuItem className={styles.userMenuItem} onSelect={() => navigate('/alerts')}>
      <MenuIcon name="bell" />
      <span className={styles.menuItemLabel}>{t('common:notifications')}</span>
      {count > 0 && (
        <span className={styles.menuBadge}>{count > 99 ? '99+' : count}</span>
      )}
    </DropdownMenuItem>
  );
}

function SidebarUserBlock({ collapsed }: { collapsed: boolean }) {
  const { keyName, email, logout } = useAuth();
  const { mode, setMode, themeId, setThemeId } = useTheme();
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const [openSection, setOpenSection] = useState<UserMenuSection | null>(null);

  const displayName = email || keyName || t('common:userMenu');
  const initial = displayName.trim().charAt(0).toUpperCase() || '?';
  const currentLang = i18n.language.split('-')[0];
  const currentModeLabel = t(`common:${MODE_OPTIONS.find((opt) => opt.value === mode)?.labelKey ?? 'themeSystem'}`);
  const currentThemeLabel = THEME_PACK_OPTIONS.find((opt) => opt.value === themeId)?.label ?? THEME_PACK_OPTIONS[0].label;
  const currentLanguageLabel = SUPPORTED_LANGUAGES.find((lang) => lang.code === currentLang)?.label ?? SUPPORTED_LANGUAGES[0].label;

  const changeLanguage = (code: string) => {
    void i18n.changeLanguage(code);
    localStorage.setItem(LANGUAGE_STORAGE_KEY, code);
  };

  const toggleMenuSection = (section: UserMenuSection) => {
    setOpenSection((current) => current === section ? null : section);
  };

  return (
    <footer className={styles.footer}>
      <DropdownMenu onOpenChange={(open) => {
        if (!open) setOpenSection(null);
      }}>
        <DropdownMenuTrigger asChild>
          <button
            data-design-system-escape="primitive-internal"
            type="button"
            className={styles.userBlockTrigger}
            aria-label={t('common:userMenu')}
          >
            <span className={styles.userAvatar}>
              {initial}
              <span className={styles.userStatusDot} />
            </span>
            {!collapsed && (
              <>
                <span className={styles.userNameBlock}>
                  <span className={styles.userNameMain}>{displayName}</span>
                  <span className={styles.userNameSub}>{t('nav:adminConsole')}</span>
                </span>
                <svg
                  className={styles.userChevron}
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
              </>
            )}
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          className={styles.userMenuContent}
          side={collapsed ? 'right' : 'top'}
          align={collapsed ? 'end' : 'start'}
          sideOffset={collapsed ? 8 : 6}
        >
          <SidebarAlertMenuItem />
          <DropdownMenuSeparator className={styles.userMenuSeparator} />

          <div className={clsx(styles.menuAccordionSection, openSection === 'theme' && styles.menuAccordionSectionOpen)}>
            <button
              data-design-system-escape="primitive-internal"
              type="button"
              className={styles.menuSectionTrigger}
              aria-expanded={openSection === 'theme'}
              onClick={() => toggleMenuSection('theme')}
            >
              <MenuIcon name="monitor" />
              <span className={styles.menuItemLabel}>{t('common:theme')}</span>
              <span className={styles.menuSectionValue}>{currentModeLabel}</span>
              <svg className={styles.menuSectionChevron} width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                <path d="M6 9l6 6 6-6" />
              </svg>
            </button>
            {openSection === 'theme' && (
              <div className={styles.menuSubmenu}>
                {MODE_OPTIONS.map((opt) => (
                  <button
                    data-design-system-escape="primitive-internal"
                    key={opt.value}
                    type="button"
                    role="menuitemradio"
                    aria-checked={mode === opt.value}
                    className={clsx(styles.menuOptionItem, mode === opt.value && styles.userMenuItemActive)}
                    onClick={() => setMode(opt.value)}
                  >
                    <MenuIcon name={opt.icon} />
                    <span className={styles.menuItemLabel}>{t(`common:${opt.labelKey}`)}</span>
                    {mode === opt.value && <span className={styles.menuCheck}>✓</span>}
                  </button>
                ))}
              </div>
            )}
          </div>

          <div className={clsx(styles.menuAccordionSection, openSection === 'skin' && styles.menuAccordionSectionOpen)}>
            <button
              data-design-system-escape="primitive-internal"
              type="button"
              className={styles.menuSectionTrigger}
              aria-expanded={openSection === 'skin'}
              onClick={() => toggleMenuSection('skin')}
            >
              <MenuIcon name="palette" />
              <span className={styles.menuItemLabel}>{t('common:skin')}</span>
              <span className={styles.menuSectionValue}>{currentThemeLabel}</span>
              <svg className={styles.menuSectionChevron} width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                <path d="M6 9l6 6 6-6" />
              </svg>
            </button>
            {openSection === 'skin' && (
              <div className={styles.menuSubmenu}>
                {THEME_PACK_OPTIONS.map((opt) => (
                  <button
                    data-design-system-escape="primitive-internal"
                    key={opt.value}
                    type="button"
                    role="menuitemradio"
                    aria-checked={themeId === opt.value}
                    className={clsx(styles.menuOptionItem, themeId === opt.value && styles.userMenuItemActive)}
                    onClick={() => setThemeId(opt.value)}
                  >
                    <MenuIcon name="palette" />
                    <span className={styles.menuItemLabel}>{opt.label}</span>
                    {themeId === opt.value && <span className={styles.menuCheck}>✓</span>}
                  </button>
                ))}
              </div>
            )}
          </div>

          <div className={clsx(styles.menuAccordionSection, openSection === 'language' && styles.menuAccordionSectionOpen)}>
            <button
              data-design-system-escape="primitive-internal"
              type="button"
              className={styles.menuSectionTrigger}
              aria-expanded={openSection === 'language'}
              onClick={() => toggleMenuSection('language')}
            >
              <MenuIcon name="globe" />
              <span className={styles.menuItemLabel}>{t('common:language')}</span>
              <span className={styles.menuSectionValue}>{currentLanguageLabel}</span>
              <svg className={styles.menuSectionChevron} width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                <path d="M6 9l6 6 6-6" />
              </svg>
            </button>
            {openSection === 'language' && (
              <div className={styles.menuSubmenu}>
                {SUPPORTED_LANGUAGES.map((lang) => (
                  <button
                    data-design-system-escape="primitive-internal"
                    key={lang.code}
                    type="button"
                    role="menuitemradio"
                    aria-checked={currentLang === lang.code}
                    className={clsx(styles.menuOptionItem, currentLang === lang.code && styles.userMenuItemActive)}
                    onClick={() => changeLanguage(lang.code)}
                  >
                    <MenuIcon name="globe" />
                    <span className={styles.menuItemLabel}>
                      {lang.flag} {lang.label}
                    </span>
                    {currentLang === lang.code && <span className={styles.menuCheck}>✓</span>}
                  </button>
                ))}
              </div>
            )}
          </div>

          <DropdownMenuSeparator className={styles.userMenuSeparator} />
          <DropdownMenuItem className={styles.userMenuItem} onSelect={() => navigate('/account')}>
            <MenuIcon name="user" />
            <span className={styles.menuItemLabel}>{t('common:myAccount')}</span>
          </DropdownMenuItem>
          <DropdownMenuItem className={clsx(styles.userMenuItem, styles.logoutItem)} onSelect={logout}>
            <MenuIcon name="logout" />
            <span className={styles.menuItemLabel}>{t('common:signOut')}</span>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </footer>
  );
}
