import { useTranslation } from 'react-i18next';
import {
  ErrorBanner as BaseErrorBanner,
  type ErrorBannerProps as BaseErrorBannerProps,
} from '@nexus-gateway/ui-shared';

/** Duck-typed view of an ApiError 403 — this presentational component must not
 *  import the api layer (page tests fully mock `@/api/client`, and `instanceof`
 *  would also break across module instances). */
interface ForbiddenError extends Error {
  status: number;
  forbiddenDetails?: { action?: string; resource?: string; reason?: string };
}

function isForbidden(e: unknown): e is ForbiddenError {
  return e instanceof Error && (e as Partial<ForbiddenError>).status === 403;
}

export type ErrorBannerProps = Omit<BaseErrorBannerProps, 'retryLabel' | 'dismissLabel' | 'message'> & {
  message?: string;
  /**
   * The thrown error object. When it is a 403 ApiError the banner renders the
   * permission-denied state instead — a clear "you don't have permission"
   * message with the required IAM action, and NO retry button (retrying a
   * denial is pointless; the backend's iamMW answer won't change). Backend
   * enforcement stays the single source of truth — the UI never duplicates
   * action-string checks to pre-hide content (that mapping drifts).
   */
  error?: unknown;
};

/**
 * CP UI wrapper around the shared ErrorBanner. Adds the
 * react-i18next translation for the action labels — the shared
 * component itself is i18n-agnostic so the agent Dashboard's
 * (different i18n setup) can use it without colliding on namespaces.
 */
export function ErrorBanner({ error, message, onRetry, ...rest }: ErrorBannerProps) {
  const { t } = useTranslation('common');

  if (isForbidden(error)) {
    const action = error.forbiddenDetails?.action;
    return (
      <BaseErrorBanner
        {...rest}
        message={t('forbiddenSection')}
        detail={action ? t('forbiddenRequires', { action }) : undefined}
        dismissLabel={t('dismiss')}
      />
    );
  }

  const resolved =
    message ?? (error instanceof Error ? error.message : error !== undefined ? String(error) : '');
  return (
    <BaseErrorBanner
      {...rest}
      message={resolved}
      onRetry={onRetry}
      retryLabel={t('retry')}
      dismissLabel={t('dismiss')}
    />
  );
}
