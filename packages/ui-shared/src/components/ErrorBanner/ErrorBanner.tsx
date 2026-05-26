import clsx from 'clsx';
import styles from './ErrorBanner.module.css';

export interface ErrorBannerProps {
  message: string;
  /** Optional secondary line (e.g. IAM evaluation reason). */
  detail?: string;
  onRetry?: () => void;
  /** Label for the retry button. Caller-supplied so the component
   *  itself stays free of i18n dependencies; CP UI passes the
   *  localized `t('retry')`, the Dashboard passes its own. */
  retryLabel?: string;
  onDismiss?: () => void;
  /** Label for the dismiss button. See retryLabel. */
  dismissLabel?: string;
  className?: string;
}

/**
 * Inline error banner. Pure presentational — no translations, no
 * data fetching. Action labels are passed as props so each app
 * owns its own i18n stack.
 */
export function ErrorBanner({
  message,
  detail,
  onRetry,
  retryLabel = 'Retry',
  onDismiss,
  dismissLabel = 'Dismiss',
  className,
}: ErrorBannerProps) {
  return (
    <div role="alert" className={clsx(styles.banner, className)}>
      <svg
        width="20"
        height="20"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        className={styles.icon}
      >
        <circle cx="12" cy="12" r="10" />
        <line x1="12" y1="8" x2="12" y2="12" />
        <line x1="12" y1="16" x2="12.01" y2="16" />
      </svg>

      <span className={styles.message}>
        {message}
        {detail ? <span className={styles.detail}>{detail}</span> : null}
      </span>

      {onRetry && (
        <button type="button" onClick={onRetry} className={styles.actionButton}>
          {retryLabel}
        </button>
      )}

      {onDismiss && (
        <button
          type="button"
          onClick={onDismiss}
          className={styles.actionButton}
          aria-label={dismissLabel}
        >
          {dismissLabel}
        </button>
      )}
    </div>
  );
}
