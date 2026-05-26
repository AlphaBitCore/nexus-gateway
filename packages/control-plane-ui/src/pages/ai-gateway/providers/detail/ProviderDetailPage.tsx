import { useTranslation } from 'react-i18next';
import {
  PageHeader, AlertDialog, Skeleton, ErrorBanner,
  Breadcrumb, Button, Stack,
} from '@/components/ui';
import { useProviderDetail } from './useProviderDetail';
import type { Tab } from './useProviderDetail';
import { ProviderInfoTab } from './ProviderInfoTab';
import { ProviderCredentialsTab } from './ProviderCredentialsTab';
import { ProviderModelsTab } from './ProviderModelsTab';
import { ProviderUsageTab } from './ProviderUsageTab';
import { ProviderHealthTab } from './ProviderHealthTab';
import { ProviderCacheTab } from './ProviderCacheTab';
import styles from './ProviderDetail.module.css';

export function ProviderDetailPage() {
  const { t } = useTranslation();
  const detail = useProviderDetail();

  const {
    provider, loading, error, refetch,
    activeTab, setActiveTab,
    canUpdate, canDelete,
    toggleEnabled, toggleLoading,
    deleting, setDeleting,
    deleteProvider,
    credentials, models,
    deletingModel, setDeletingModel, deleteModel,
    deletingCred, setDeletingCred, deleteCredential,
  } = detail;

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!provider) return null;

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:providers.title'), to: '/ai-gateway/providers' },
        { label: provider.name },
      ]} />

      {/* Header */}
      <PageHeader
        title={provider.name}
        subtitle={provider.displayName || provider.description || undefined}
        action={
          <Stack direction="horizontal" gap="sm" className={styles.alignCenter}>
            {canUpdate && activeTab === 'info' && !detail.isEditing && (
              <Button variant="secondary" onClick={detail.startEditing}>{t('common:edit')}</Button>
            )}
            {canUpdate && (
              <Button
                variant="secondary"
                onClick={() => toggleEnabled(!provider.enabled)}
                disabled={toggleLoading}
              >
                {provider.enabled ? t('pages:providers.disable') : t('pages:providers.enable')}
              </Button>
            )}
            {canDelete && (
              <Button variant="danger" onClick={() => setDeleting(true)}>{t('common:delete')}</Button>
            )}
          </Stack>
        }
      />

      {/* Tabs */}
      <div className={styles.tabBar}>
        {(['info', 'credentials', 'models', 'usage', 'health', 'cache'] as Tab[]).map(tab => (
          <button
            key={tab}
            onClick={() => setActiveTab(tab)}
            className={activeTab === tab ? styles.tabActive : styles.tab}
          >
            {tab === 'info' && t('pages:providers.information')}
            {tab === 'credentials' && `${t('pages:providers.credentials')} (${credentials.length})`}
            {tab === 'models' && `${t('pages:providers.models')} (${models.length})`}
            {tab === 'usage' && t('pages:providers.usage')}
            {tab === 'health' && t('pages:providers.health')}
            {tab === 'cache' && t('pages:providers.cache')}
          </button>
        ))}
      </div>

      {/* Tab content */}
      {activeTab === 'info' && <ProviderInfoTab detail={detail} />}
      {activeTab === 'credentials' && <ProviderCredentialsTab detail={detail} />}
      {activeTab === 'models' && <ProviderModelsTab detail={detail} />}
      {activeTab === 'usage' && <ProviderUsageTab detail={detail} />}
      {activeTab === 'health' && <ProviderHealthTab detail={detail} />}
      {activeTab === 'cache' && <ProviderCacheTab providerID={provider.id} adapterType={provider.adapterType} />}

      {/* Dialogs */}
      <AlertDialog
        open={deleting}
        onOpenChange={(open) => { if (!open) setDeleting(false); }}
        title={t('pages:providers.deleteProvider')}
        description={t('pages:providers.deleteProviderConfirm', { name: provider.displayName || provider.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => deleteProvider(undefined as never)}
        variant="danger"
      />
      <AlertDialog
        open={!!deletingModel}
        onOpenChange={(open) => { if (!open) setDeletingModel(null); }}
        title={t('pages:providers.deleteModel')}
        description={t('pages:providers.deleteModelConfirm', { name: deletingModel?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deletingModel) deleteModel(deletingModel.id); }}
        variant="danger"
      />
      <AlertDialog
        open={!!deletingCred}
        onOpenChange={(open) => { if (!open) setDeletingCred(null); }}
        title={t('pages:providers.deleteCredential')}
        description={t('pages:providers.deleteCredentialConfirm', { name: deletingCred?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deletingCred) deleteCredential(deletingCred.id); }}
        variant="danger"
      />
    </Stack>
  );
}
