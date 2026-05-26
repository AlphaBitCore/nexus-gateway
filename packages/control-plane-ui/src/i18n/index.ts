import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import HttpBackend from 'i18next-http-backend';

// English bundled for instant first render (no network wait)
import commonEn from './locales/en/common.json';
import navEn from './locales/en/nav.json';
import pagesEn from './locales/en/pages.json';
// `shared` namespace lives in @nexus-gateway/ui-shared so both this
// admin app and the agent Dashboard (Wails) pull the same strings.
import sharedEn from '@nexus-gateway/ui-shared/i18n/en/shared.json';

const STORAGE_KEY = 'nexus-language';

/**
 * i18n configuration.
 *
 * - English: bundled (zero latency on first render)
 * - Other languages: loaded on demand from /locales/{lng}/{ns}.json via HTTP backend
 * - Fallback: English if a key is missing in the selected language
 */
void i18n
  .use(HttpBackend)
  .use(initReactI18next)
  .init({
    // Bundled English — available immediately, no network request
    resources: {
      en: { common: commonEn, nav: navEn, pages: pagesEn, shared: sharedEn },
    },
    // Only load non-English languages from the backend
    partialBundledLanguages: true,
    backend: {
      loadPath: '/locales/{{lng}}/{{ns}}.json',
    },
    lng: (() => { try { return localStorage?.getItem(STORAGE_KEY) || 'en'; } catch { return 'en'; } })(),
    fallbackLng: 'en',
    defaultNS: 'common',
    ns: ['common', 'nav', 'pages', 'shared'],
    interpolation: {
      escapeValue: false,
    },
    // Allow <code> in <Trans> strings (defaults only include br, strong, i, p).
    react: {
      transKeepBasicHtmlNodesFor: ['br', 'strong', 'i', 'p', 'code'],
    },
  });

export const SUPPORTED_LANGUAGES = [
  { code: 'en', label: 'English', flag: '🇺🇸' },
  { code: 'zh', label: '中文', flag: '🇨🇳' },
  { code: 'es', label: 'Español', flag: '🇪🇸' },
] as const;

export const LANGUAGE_STORAGE_KEY = STORAGE_KEY;

export default i18n;
