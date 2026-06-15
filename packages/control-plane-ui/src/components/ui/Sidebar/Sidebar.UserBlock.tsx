import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
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
import { MenuIcon } from './Sidebar.icons';
import { SidebarAlertMenuItem } from './Sidebar.AlertMenuItem';
import styles from './Sidebar.module.css';

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

export function SidebarUserBlock({ collapsed }: { collapsed: boolean }) {
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
          side="right"
          align="end"
          sideOffset={12}
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
