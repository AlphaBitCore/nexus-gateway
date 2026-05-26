import { Component, type ErrorInfo, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import styles from './ErrorBoundary.module.css';

/* ── Types ───────────────────────────────────────────────────────────────── */

export interface ErrorBoundaryProps {
  children: ReactNode;
  /** Shown when an error is caught. Defaults to a generic recovery UI. */
  fallback?: ReactNode | ((error: Error, reset: () => void) => ReactNode);
  /** Granularity level — controls the default fallback UI style. */
  level?: 'app' | 'route' | 'widget';
  /** Called when an error is caught (e.g., for logging). */
  onError?: (error: Error, info: ErrorInfo) => void;
}

interface State {
  error: Error | null;
}

/* ── Component ───────────────────────────────────────────────────────────── */

export class ErrorBoundary extends Component<ErrorBoundaryProps, State> {
  constructor(props: ErrorBoundaryProps) {
    super(props);
    this.state = { error: null };
  }

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    this.props.onError?.(error, info);
    if (import.meta.env.DEV) {
      console.error('[ErrorBoundary]', error, info.componentStack);
    }
  }

  private reset = () => {
    this.setState({ error: null });
  };

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;

    const { fallback, level = 'route' } = this.props;

    // Custom fallback
    if (typeof fallback === 'function') return fallback(error, this.reset);
    if (fallback) return fallback;

    // Default fallback by level
    return <DefaultFallback error={error} level={level} onReset={this.reset} />;
  }
}

/* ── Default Fallback UI ─────────────────────────────────────────────────── */

function DefaultFallback({
  error,
  level,
  onReset,
}: {
  error: Error;
  level: 'app' | 'route' | 'widget';
  onReset: () => void;
}) {
  const { t } = useTranslation();

  if (level === 'widget') {
    return (
      <div className={styles.widget}>
        <p className={styles.widgetText}>{t('failedToLoad')}</p>
        <button data-design-system-escape="primitive-internal" type="button" onClick={onReset} className={styles.widgetRetry}>
          {t('retry')}
        </button>
      </div>
    );
  }

  return (
    <div className={level === 'app' ? styles.app : styles.route}>
      <div className={styles.card}>
        <div className={styles.icon} aria-hidden="true">
          <svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
            <circle cx="12" cy="12" r="10" />
            <line x1="12" y1="8" x2="12" y2="12" />
            <line x1="12" y1="16" x2="12.01" y2="16" />
          </svg>
        </div>
        <h1 className={styles.title}>
          {level === 'app' ? t('applicationError') : t('pageError')}
        </h1>
        <p className={styles.message}>
          {level === 'app' ? t('applicationErrorMessage') : t('pageErrorMessage')}
        </p>
        {import.meta.env.DEV && (
          <details className={styles.details}>
            <summary className={styles.detailsSummary}>{t('errorDetails')}</summary>
            <pre className={styles.stack}>{error.message}\n{error.stack}</pre>
          </details>
        )}
        <div className={styles.actions}>
          <button data-design-system-escape="primitive-internal" type="button" onClick={onReset} className={styles.retryBtn}>
            {t('retry')}
          </button>
          {level === 'app' && (
            <button data-design-system-escape="primitive-internal"
              type="button"
              onClick={() => window.location.reload()}
              className={styles.reloadBtn}
            >
              {t('reloadPage')}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
