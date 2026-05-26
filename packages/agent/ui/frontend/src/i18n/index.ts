// Dashboard i18n bootstrap.
//
// Bundles every supported locale directly into the JS chunk — unlike the
// Control Plane UI we have NO http backend available (the Wails WebView
// can read embedded assets but the network plane is firewalled). The
// `shared` namespace lives in @nexus-gateway/ui-shared so the Dashboard
// and the CP UI surface the same translations for common actions / status
// labels. Persisted user preference reads/writes localStorage; first
// boot defaults to English.
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';

import sharedEn from '@nexus-gateway/ui-shared/i18n/en/shared.json';
import sharedZh from '@nexus-gateway/ui-shared/i18n/zh/shared.json';
import sharedEs from '@nexus-gateway/ui-shared/i18n/es/shared.json';

import dashboardEn from './locales/en/dashboard.json';
import dashboardZh from './locales/zh/dashboard.json';
import dashboardEs from './locales/es/dashboard.json';

export const LANGUAGE_STORAGE_KEY = 'nexus-dashboard-language';

export const SUPPORTED_LANGUAGES = [
  { code: 'en', label: 'English' },
  { code: 'zh', label: '中文' },
  { code: 'es', label: 'Español' },
] as const;

export type LanguageCode = (typeof SUPPORTED_LANGUAGES)[number]['code'];

function initialLanguage(): LanguageCode {
  try {
    const stored = localStorage.getItem(LANGUAGE_STORAGE_KEY) as LanguageCode | null;
    if (stored && SUPPORTED_LANGUAGES.some((l) => l.code === stored)) return stored;
  } catch {
    // ignore — fall through to default
  }
  return 'en';
}

void i18n.use(initReactI18next).init({
  resources: {
    en: { shared: sharedEn, dashboard: dashboardEn },
    zh: { shared: sharedZh, dashboard: dashboardZh },
    es: { shared: sharedEs, dashboard: dashboardEs },
  },
  lng: initialLanguage(),
  fallbackLng: 'en',
  defaultNS: 'dashboard',
  ns: ['shared', 'dashboard'],
  interpolation: { escapeValue: false },
});

/** Programmatic language change (persists to localStorage). */
export function setLanguage(lng: LanguageCode): void {
  void i18n.changeLanguage(lng);
  try {
    localStorage.setItem(LANGUAGE_STORAGE_KEY, lng);
  } catch {
    // ignore — preference survives the session even if storage is blocked
  }
}

export default i18n;
