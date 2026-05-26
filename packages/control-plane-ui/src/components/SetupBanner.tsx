import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { useTheme } from '@/theme/useTheme';
import { checkAllSetupComplete } from '../pages/setup/SetupWizardPage';
import styles from './SetupBanner.module.css';

/**
 * SetupBanner — shows at the top of the dashboard while the system is not
 * fully configured. Uses live data detection (orgs, providers, credentials,
 * projects, VKs, routing rules) instead of the old setup-state API.
 */
export function SetupBanner() {
  const { t } = useTranslation();
  const { brand } = useTheme();
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    let cancelled = false;
    checkAllSetupComplete().then((done) => {
      if (!cancelled) setVisible(!done);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!visible) return null;

  return (
    <div role="status" className={styles.banner}>
      <strong>{t('pages:setup.bannerTitle', { productName: brand.productName })}</strong>
      <span>
        {t(
          'pages:setup.bannerBody',
          'Finish the setup wizard to connect a provider, define routing, and issue your first virtual key.',
        )}
      </span>
      <span className={styles.spacer} />
      <Link to="/setup" className={styles.link}>
        {t('pages:setup.bannerOpen', 'Open wizard')}
      </Link>
      <button
        type="button"
        onClick={() => setVisible(false)}
        className={styles.dismissBtn}
      >
        {t('pages:setup.bannerDismiss', 'Dismiss')}
      </button>
    </div>
  );
}
