import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { useApi } from '@/hooks/useApi';
import {
  cacheApi,
  type CacheOverrideRow,
  type CacheOverridesList,
} from '@/api/services/system/cache';
import { Badge, Card, ErrorBanner, Skeleton, Stack } from '@/components/ui';
import styles from './SettingsCacheTab.module.css';

export function OverridesPanel() {
  const { t } = useTranslation();
  const { data, loading, error, refetch } = useApi<CacheOverridesList>(
    () => cacheApi.listOverrides(),
    ['admin', 'cache', 'overrides'],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const items = data?.items ?? [];

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.overridesTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.overridesSubtitle')}</p>

        {items.length === 0 ? (
          <p className={styles.empty}>{t('pages:settings.promptCache.overridesEmpty')}</p>
        ) : (
          <table className={styles.overridesTable}>
            <thead>
              <tr>
                <th>{t('pages:settings.promptCache.overrideProvider')}</th>
                <th>{t('pages:settings.promptCache.overrideAdapter')}</th>
                <th>{t('pages:settings.promptCache.overrideFields')}</th>
                <th>{t('pages:settings.promptCache.overrideActions')}</th>
              </tr>
            </thead>
            <tbody>
              {items.map(row => <OverrideRowView key={row.provider_id} row={row} />)}
            </tbody>
          </table>
        )}
      </Stack>
    </Card>
  );
}

function OverrideRowView({ row }: { row: CacheOverrideRow }) {
  const { t } = useTranslation();
  return (
    <tr>
      <td><strong>{row.provider_name}</strong> <code>{row.provider_id.slice(0, 8)}…</code></td>
      <td><Badge variant="default">{row.adapter_type}</Badge></td>
      <td>
        <ul className={styles.diffList}>
          {row.overridden_keys.map(key => {
            const d = row.diff[key];
            return (
              <li key={key}>
                <code>{key}</code>: <span className={styles.diffInherited}>{String(d.inherited)}</span> → <strong>{String(d.override)}</strong>
              </li>
            );
          })}
        </ul>
      </td>
      <td>
        <Link to={`/ai-gateway/providers/${row.provider_id}`}>{t('pages:settings.promptCache.openProvider')}</Link>
      </td>
    </tr>
  );
}
