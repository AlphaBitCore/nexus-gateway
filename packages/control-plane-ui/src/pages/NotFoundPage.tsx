import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import styles from './NotFoundPage.module.css';

export function NotFoundPage() {
  const { t } = useTranslation('common');
  return (
    <div className={styles.container}>
      <h2 className={styles.heading}>404</h2>
      <p className={styles.message}>{t('pageNotFound')}</p>
      <Link to="/" className={styles.link}>{t('goToDashboard')}</Link>
    </div>
  );
}
