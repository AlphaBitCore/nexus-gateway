import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import styles from './LoadingSpinner.module.css';

export interface LoadingSpinnerProps {
  message?: string;
  /** @default 'md' */
  size?: 'sm' | 'md' | 'lg';
  className?: string;
}

export function LoadingSpinner({
  message,
  size = 'md',
  className,
}: LoadingSpinnerProps) {
  const { t } = useTranslation('common');
  const displayMessage = message ?? t('loading');
  return (
    <div className={clsx(styles.container, className)}>
      <div className={clsx(styles.spinner, styles[size])} />
      {displayMessage && <p className={styles.message}>{displayMessage}</p>}
    </div>
  );
}

