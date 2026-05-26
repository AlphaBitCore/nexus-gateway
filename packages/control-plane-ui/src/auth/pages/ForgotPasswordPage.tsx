import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useTheme } from '@/theme/useTheme';
import { useAuth } from '../context/AuthContext';
import { SUPPORTED_LANGUAGES, LANGUAGE_STORAGE_KEY } from '../../i18n';
import { Button, Stack } from '@/components/ui';
import { clsx } from 'clsx';
import styles from './LoginPage.module.css';

export function ForgotPasswordPage() {
  const { t, i18n } = useTranslation();
  const { brand } = useTheme();
  const { status } = useAuth();
  const navigate = useNavigate();
  const currentLang = i18n.language;
  const changeLang = (code: string) => {
    void i18n.changeLanguage(code);
    localStorage.setItem(LANGUAGE_STORAGE_KEY, code);
  };

  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    if (status === 'authenticated') {
      navigate('/', { replace: true });
    }
  }, [status, navigate]);

  useEffect(() => {
    const timer = setTimeout(() => setMounted(true), 50);
    return () => clearTimeout(timer);
  }, []);

  return (
    <div className={styles.page}>
      <div className={clsx(styles.card, mounted ? styles.cardMounted : styles.cardMounting)}>
        <div className={styles.header}>
          <h1 className={styles.title}>{t('forgotPasswordPageTitle')}</h1>
          <p className={styles.subtitle}>{t('forgotPasswordPageSubtitle')}</p>
        </div>

        <Stack gap="md">
          <p className={styles.authHelpText}>{t('forgotPasswordInstructions')}</p>
          <div className={styles.authActions}>
            <Button type="button" variant="secondary" onClick={() => navigate('/login')}>
              {t('backToSignIn')}
            </Button>
          </div>
        </Stack>

        <div className={styles.langSwitcher}>
          <select
            value={currentLang}
            onChange={(e) => changeLang(e.target.value)}
            className={styles.langSelect}
            aria-label={t('language')}
          >
            {SUPPORTED_LANGUAGES.map((lang) => (
              <option key={lang.code} value={lang.code}>
                {lang.flag} {lang.label}
              </option>
            ))}
          </select>
        </div>

        {brand.tagline && <p className={styles.footer}>{brand.tagline}</p>}
      </div>
    </div>
  );
}
