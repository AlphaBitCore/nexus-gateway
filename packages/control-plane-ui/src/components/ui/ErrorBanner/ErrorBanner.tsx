import { useTranslation } from 'react-i18next';
import {
  ErrorBanner as BaseErrorBanner,
  type ErrorBannerProps as BaseErrorBannerProps,
} from '@nexus-gateway/ui-shared';

export type ErrorBannerProps = Omit<BaseErrorBannerProps, 'retryLabel' | 'dismissLabel'>;

/**
 * CP UI wrapper around the shared ErrorBanner. Adds the
 * react-i18next translation for the action labels — the shared
 * component itself is i18n-agnostic so the agent Dashboard's
 * (different i18n setup) can use it without colliding on namespaces.
 */
export function ErrorBanner(props: ErrorBannerProps) {
  const { t } = useTranslation('common');
  return (
    <BaseErrorBanner
      {...props}
      retryLabel={t('retry')}
      dismissLabel={t('dismiss')}
    />
  );
}
