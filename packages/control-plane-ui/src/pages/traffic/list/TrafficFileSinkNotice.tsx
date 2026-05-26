import { Trans, useTranslation } from 'react-i18next';

type TrafficFileSinkNoticeVariant = 'compact' | 'full';

/**
 * Explains that traffic audit uses the file sink and shows the configured path.
 * The path is rendered as React text (never via innerHTML) so server-provided values cannot inject markup.
 */
export function TrafficFileSinkNotice({
  variant,
  filePath,
}: {
  variant: TrafficFileSinkNoticeVariant;
  filePath?: string | null;
}) {
  const { t } = useTranslation();
  const displayPath = (filePath && String(filePath).trim()) || t('pages:traffic.filePathFallback');
  const i18nKey =
    variant === 'full' ? 'pages:traffic.notQueryableBodyFull' : 'pages:traffic.notQueryableBody';

  return (
    <Trans
      i18nKey={i18nKey}
      components={{
        // Server-provided path must render as React text, not interpolated HTML.
        filepath: <code>{displayPath}</code>,
      }}
    />
  );
}
