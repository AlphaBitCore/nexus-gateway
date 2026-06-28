/**
 * Top-level i18n language picker.
 *
 * A small dropdown that lets the operator switch the Dashboard UI language
 * between the three supported locales (en / zh / es). Mounted in the Shell
 * alongside the GlobalTimeRangeBanner. Persists the choice to localStorage
 * and calls i18next.changeLanguage so every i18n key re-renders immediately.
 */

import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import styles from './LanguagePicker.module.css';

const LANGUAGES: Array<{ code: string; label: string }> = [
  { code: 'en', label: 'English' },
  { code: 'zh', label: '中文' },
  { code: 'es', label: 'Español' },
];

export function LanguagePicker() {
  const { t, i18n } = useTranslation();

  const handleChange = useCallback(
    (e: React.ChangeEvent<HTMLSelectElement>) => {
      const lang = e.target.value;
      void i18n.changeLanguage(lang);
      try {
        localStorage.setItem('nexusLang', lang);
      } catch {
        // private mode — skip
      }
    },
    [i18n],
  );

  return (
    <select
      value={i18n.language?.split('-')[0] ?? 'en'}
      onChange={handleChange}
      aria-label={t('common:language')}
      className={styles.select}
    >
      {LANGUAGES.map((l) => (
        <option key={l.code} value={l.code}>
          {l.label}
        </option>
      ))}
    </select>
  );
}
