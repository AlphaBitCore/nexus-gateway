import { useState, useEffect, type RefObject } from 'react';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { useAuth } from '@/auth/context/AuthContext';
import { useTheme } from '@/theme/useTheme';
import { SUPPORTED_LANGUAGES, LANGUAGE_STORAGE_KEY } from '@/i18n';
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from '../DropdownMenu';
import { AlertBell } from '../../alerts/AlertBell';
import styles from './Header.module.css';

/* ── Theme pack options (one per /themes/*.json) ──────────────────────────── */

const THEME_PACK_OPTIONS = [
  { value: 'default', label: 'Default' },
  { value: 'morningstar', label: 'Morningstar' },
  { value: 'rbc', label: 'RBC Royal Bank' },
] as const;

/* ── Mode labels (light / dark / system) ──────────────────────────────────── */

const MODE_OPTIONS = [
  { value: 'light' as const, labelKey: 'themeLight', icon: '\u2600' },   // sun
  { value: 'dark' as const, labelKey: 'themeDark', icon: '\u263E' },     // moon
  { value: 'system' as const, labelKey: 'themeSystem', icon: '\uD83D\uDCBB' }, // laptop
];

function resolvedIcon(resolvedMode: 'light' | 'dark', mode: 'light' | 'dark' | 'system'): string {
  if (mode === 'system') return '\uD83D\uDCBB';
  return resolvedMode === 'dark' ? '\u263E' : '\u2600';
}

/* ── Props ────────────────────────────────────────────────────────────────── */

export interface HeaderProps {
  isMobile?: boolean;
  onMenuToggle?: () => void;
  /** When set, header "scrolled" styling follows this element's scrollTop. */
  scrollContainerRef?: RefObject<HTMLElement | null>;
}

/* ── Header component ─────────────────────────────────────────────────────── */

export function Header({ isMobile, onMenuToggle, scrollContainerRef }: HeaderProps) {
  const { keyName, logout } = useAuth();
  const { mode, resolvedMode, setMode, brand, themeId, setThemeId } = useTheme();
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();

  const changeLanguage = (code: string) => {
    void i18n.changeLanguage(code);
    localStorage.setItem(LANGUAGE_STORAGE_KEY, code);
  };
  const currentLang = i18n.language;
  const [scrolled, setScrolled] = useState(false);

  /* Track scroll position for frosted-glass effect */
  useEffect(() => {
    const el = scrollContainerRef?.current ?? null;
    const handler = () => {
      const y = el ? el.scrollTop : typeof window !== 'undefined' ? window.scrollY : 0;
      setScrolled(y > 10);
    };
    handler();
    if (el) {
      el.addEventListener('scroll', handler, { passive: true });
      return () => el.removeEventListener('scroll', handler);
    }
    if (typeof window !== 'undefined') {
      window.addEventListener('scroll', handler, { passive: true });
      return () => window.removeEventListener('scroll', handler);
    }
    return undefined;
  }, [scrollContainerRef]);

  return (
    <header className={clsx(styles.header, scrolled && styles.headerScrolled)}>
      {/* Left side */}
      <div className={styles.left}>
        {isMobile && (
          <>
            <button data-design-system-escape="primitive-internal"
              className={styles.hamburger}
              onClick={onMenuToggle}
              aria-label={t('common:toggleNav')}
            >
              <span className={styles.hamburgerBar} />
              <span className={styles.hamburgerBar} />
              <span className={styles.hamburgerBar} />
            </button>
            <div className={styles.separator} />
          </>
        )}
        <span className={styles.brandName}>{brand.productName}</span>
      </div>

      {/* Right side */}
      <div className={styles.right}>
        {/* Theme toggle dropdown */}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button data-design-system-escape="primitive-internal"
              className={styles.themeToggle}
              aria-label={`Theme: ${mode}. Click to change.`}
            >
              {resolvedIcon(resolvedMode, mode)}
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {MODE_OPTIONS.map((opt) => (
              <DropdownMenuItem
                key={opt.value}
                onSelect={() => setMode(opt.value)}
              >
                {opt.icon}&nbsp;&nbsp;{t(`common:${opt.labelKey}`)}
                {mode === opt.value ? ' \u2713' : ''}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>

        {/* Theme pack selector dropdown */}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button data-design-system-escape="primitive-internal"
              className={styles.themeToggle}
              aria-label={`Theme: ${themeId}. Click to change.`}
            >
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <circle cx="13.5" cy="6.5" r="2.5" />
                <circle cx="17.5" cy="10.5" r="1.5" />
                <circle cx="8.5" cy="7.5" r="1.5" />
                <circle cx="6.5" cy="12.5" r="1.5" />
                <path d="M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.926 0 1.648-.746 1.648-1.688 0-.437-.18-.835-.437-1.125-.29-.289-.438-.652-.438-1.125a1.64 1.64 0 0 1 1.668-1.668h1.996c3.051 0 5.555-2.503 5.555-5.554C21.965 6.012 17.461 2 12 2z" />
              </svg>
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {THEME_PACK_OPTIONS.map((opt) => (
              <DropdownMenuItem
                key={opt.value}
                onSelect={() => setThemeId(opt.value)}
              >
                {opt.label}
                {themeId === opt.value ? ' \u2713' : ''}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>

        {/* Language selector dropdown */}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button data-design-system-escape="primitive-internal"
              className={styles.themeToggle}
              aria-label={`Language: ${currentLang}. Click to change.`}
            >
              {SUPPORTED_LANGUAGES.find(l => l.code === currentLang)?.flag ?? '🌐'}
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {SUPPORTED_LANGUAGES.map((lang) => (
              <DropdownMenuItem
                key={lang.code}
                onSelect={() => changeLanguage(lang.code)}
              >
                {lang.flag}&nbsp;&nbsp;{lang.label}
                {currentLang === lang.code ? ' \u2713' : ''}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>

        {/* Alert bell */}
        <AlertBell />

        <div className={styles.separator} />

        {/* User menu */}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button data-design-system-escape="primitive-internal" className={styles.userMenuTrigger} aria-label={t('common:userMenu')}>
              <span className={styles.avatar}>
                {(keyName ?? '?')[0].toUpperCase()}
              </span>
              <span className={styles.userName}>{keyName}</span>
              <svg className={styles.chevron} width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M6 9l6 6 6-6" />
              </svg>
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => navigate('/account')}>
              {t('common:myAccount')}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={logout}>
              {t('common:signOut')}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  );
}
