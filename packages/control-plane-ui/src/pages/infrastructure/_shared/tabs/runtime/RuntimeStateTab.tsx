import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { nodeRuntimeApi, type NodeRuntimeResponse } from '@/api/services/infrastructure/nodes/nodeRuntime';
import { Stack, Card, Button, Skeleton, ErrorBanner, Badge } from '@/components/ui';
import styles from '../../../nodes/InfraNodeDetailPage.module.css';

interface RuntimeStateTabProps {
  thingId: string;
}

export function RuntimeStateTab({ thingId }: RuntimeStateTabProps) {
  const { t } = useTranslation();
  const { data, loading, error, refetch } = useApi<NodeRuntimeResponse>(
    () => nodeRuntimeApi.get(thingId),
    ['admin', 'nodes', 'runtime', thingId],
    { skip: !thingId },
  );

  if (loading && !data) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  const { snapshot, meta: bridgeMeta } = data;
  const sourceNames = Object.keys(snapshot?.sources ?? {}).sort();
  const lastSeenLabel = bridgeMeta.last_seen_at
    ? new Date(bridgeMeta.last_seen_at).toLocaleString()
    : '—';

  return (
    <Card>
      <Stack gap="md">
        <Stack direction="horizontal" gap="md" align="center">
          <h2 className={styles.sectionTitle}>{t('pages:infrastructure.runtime.title')}</h2>
          <Button size="sm" variant="secondary" onClick={refetch}>
            {t('pages:infrastructure.runtime.refresh')}
          </Button>
        </Stack>

        <dl className={styles.infoGrid}>
          <div className={styles.infoRow}>
            <dt className={styles.infoLabel}>{t('pages:infrastructure.runtime.service')}</dt>
            <dd className={styles.infoValue}>
              <code className={styles.mono}>{snapshot?.meta?.service ?? '—'}</code>
            </dd>
          </div>
          <div className={styles.infoRow}>
            <dt className={styles.infoLabel}>{t('pages:infrastructure.runtime.processStartedAt')}</dt>
            <dd className={styles.infoValue}>
              {snapshot?.meta?.process_started_at
                ? new Date(snapshot.meta.process_started_at).toLocaleString()
                : '—'}
            </dd>
          </div>
          <div className={styles.infoRow}>
            <dt className={styles.infoLabel}>{t('pages:infrastructure.runtime.snapshotTakenAt')}</dt>
            <dd className={styles.infoValue}>
              {snapshot?.snapshot_taken_at
                ? new Date(snapshot.snapshot_taken_at).toLocaleString()
                : '—'}
            </dd>
          </div>
          <div className={styles.infoRow}>
            <dt className={styles.infoLabel}>{t('pages:infrastructure.runtime.hubDesiredVer')}</dt>
            <dd className={styles.infoValue}>{bridgeMeta.desired_ver}</dd>
          </div>
          <div className={styles.infoRow}>
            <dt className={styles.infoLabel}>{t('pages:infrastructure.runtime.hubReportedVer')}</dt>
            <dd className={styles.infoValue}>{bridgeMeta.reported_ver}</dd>
          </div>
          <div className={styles.infoRow}>
            <dt className={styles.infoLabel}>{t('pages:infrastructure.runtime.lastSeenAt')}</dt>
            <dd className={styles.infoValue}>{lastSeenLabel}</dd>
          </div>
        </dl>

        {sourceNames.length === 0 ? (
          <p className={styles.emptyText}>{t('pages:infrastructure.runtime.empty')}</p>
        ) : (
          <Stack gap="sm">
            {sourceNames.map((name) => {
              const result = snapshot.sources[name];
              return (
                <Card key={name}>
                  <Stack gap="sm">
                    <Stack direction="horizontal" gap="sm" align="center">
                      <code>{name}</code>
                      {result.ok ? (
                        <Badge variant="success">{t('pages:infrastructure.runtime.statusOk')}</Badge>
                      ) : (
                        <Badge variant="danger">{t('pages:infrastructure.runtime.statusError')}</Badge>
                      )}
                    </Stack>
                    {result.ok ? (
                      <pre className={styles.jsonPre}>{JSON.stringify(result.value, null, 2)}</pre>
                    ) : (
                      <p>{result.error ?? t('pages:infrastructure.runtime.unknownError')}</p>
                    )}
                  </Stack>
                </Card>
              );
            })}
          </Stack>
        )}
      </Stack>
    </Card>
  );
}
