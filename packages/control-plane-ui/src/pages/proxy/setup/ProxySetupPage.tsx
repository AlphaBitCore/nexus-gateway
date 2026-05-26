import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { hubApi, type Node } from '@/api/services/infrastructure/nodes/hub';
import { PageHeader, Stack, Card, Button, LoadingSpinner, ErrorBanner, Badge, Tooltip } from '@/components/ui';
import styles from './ProxySetupPage.module.css';

export default function ProxySetupPage() {
  const { t } = useTranslation('pages');
  const navigate = useNavigate();

  const { data, loading, error } = useApi(
    () => hubApi.listNodes({ type: 'compliance-proxy', pageSize: 200 }),
    ['admin', 'proxy-setup', 'nodes'],
  );

  const nodes: Node[] = data?.nodes ?? [];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.proxySetupTitle')}
        subtitle={t('infrastructure.proxySetupDescription')}
      />

      {loading && <LoadingSpinner />}
      {error && <ErrorBanner message={error.message} />}

      {!loading && !error && nodes.length === 0 && (
        <Card>
          <p className={styles.emptyMessage}>{t('infrastructure.noProxyNodes')}</p>
        </Card>
      )}

      {nodes.map((node) => {
        const online = node.status === 'online';
        return (
          <Card key={node.id}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 'var(--g-space-3)' }}>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-0-5)' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)' }}>
                  <span style={{ fontWeight: 'var(--g-font-weight-semibold)' }}>{node.name}</span>
                  <Badge variant={online ? 'success' : 'default'}>
                    {node.status}
                  </Badge>
                </div>
                <div className={styles.nodeId}>{node.id}</div>
              </div>

              <Tooltip content={!online ? t('infrastructure.offlineSetupDisabled') : undefined}>
                {/* span wrapper ensures pointer events reach the Tooltip trigger
                    even when the Button is disabled (disabled elements don't fire
                    mouse events in browsers). */}
                <span style={{ display: 'inline-block', cursor: online ? undefined : 'not-allowed' }}>
                  <Button
                    variant="primary"
                    size="sm"
                    disabled={!online}
                    style={{ pointerEvents: online ? undefined : 'none' }}
                    onClick={() => navigate(`/infrastructure/nodes/${node.id}/setup`)}
                  >
                    {t('infrastructure.configureSetup')}
                  </Button>
                </span>
              </Tooltip>
            </div>
          </Card>
        );
      })}
    </Stack>
  );
}
