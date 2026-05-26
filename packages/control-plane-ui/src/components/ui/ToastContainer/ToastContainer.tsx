import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import styles from './ToastContainer.module.css';

export interface ToastItem {
  id: number;
  message: string;
  type: 'success' | 'error' | 'warning' | 'info';
}

const DISMISS_MS: Record<string, number> = {
  success: 3000,
  info: 4000,
  warning: 5000,
  error: 5000,
};

const TYPE_ICON: Record<string, string> = {
  success: 'M20 6L9 17l-5-5',
  error:   'M18 6L6 18M6 6l12 12',
  warning: 'M12 9v4m0 4h.01M12 2l10 18H2L12 2z',
  info:    'M12 16v-4m0-4h.01M22 12a10 10 0 11-20 0 10 10 0 0120 0z',
};

function SingleToast({ toast, onDismiss }: { toast: ToastItem; onDismiss: (id: number) => void }) {
  const { t } = useTranslation();
  const [exiting, setExiting] = useState(false);
  const [progress, setProgress] = useState(100);
  const icon = TYPE_ICON[toast.type] ?? TYPE_ICON.info;

  const handleDismiss = useCallback(() => {
    setExiting(true);
    setTimeout(() => onDismiss(toast.id), 250);
  }, [onDismiss, toast.id]);

  useEffect(() => {
    const start = Date.now();
    const interval = setInterval(() => {
      const elapsed = Date.now() - start;
      const duration = DISMISS_MS[toast.type] ?? 4000;
      const remaining = Math.max(0, 100 - (elapsed / duration) * 100);
      setProgress(remaining);
      if (remaining <= 0) {
        clearInterval(interval);
        handleDismiss();
      }
    }, 50);
    return () => clearInterval(interval);
  }, [handleDismiss]);

  return (
    <div
      role="alert"
      data-type={toast.type}
      className={styles.toast}
      style={{
        opacity: exiting ? 0 : 1,
        transform: exiting ? 'translateY(-12px)' : 'translateY(0)',
        transition: 'opacity var(--g-transition-slow), transform var(--g-transition-slow)',
        animation: exiting ? 'none' : undefined,
      }}
    >
      <div className={styles.accent} />
      <div className={styles.iconWrap}>
        <svg
          width="18" height="18" viewBox="0 0 24 24" fill="none"
          stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"
        >
          <path d={icon} />
        </svg>
      </div>
      <div className={styles.message}>{toast.message}</div>
      <button data-design-system-escape="primitive-internal" onClick={handleDismiss} className={styles.dismissBtn} aria-label={t('common:dismiss')}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M18 6L6 18M6 6l12 12" />
        </svg>
      </button>
      <div className={styles.progress} style={{ width: `${progress}%` }} />
    </div>
  );
}

export function ToastContainer({ toasts, onDismiss }: { toasts: ToastItem[]; onDismiss: (id: number) => void }) {
  const { t } = useTranslation();
  // Always render the region so aria-live is registered before toasts appear
  return (
    <div className={styles.viewport} role="region" aria-live="polite" aria-label={t('common:notifications')}>
      {toasts.map((toast) => (
        <SingleToast key={toast.id} toast={toast} onDismiss={onDismiss} />
      ))}
    </div>
  );
}
