import { useTranslation } from 'react-i18next';
import {
  PageHeader, Skeleton, ErrorBanner, Breadcrumb, Button, Stack,
  Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import { useVirtualKeyDetail } from './useVirtualKeyDetail';
import { VirtualKeyInfoTab } from './VirtualKeyInfoTab';
import { VirtualKeyQuotaTab } from './VirtualKeyQuotaTab';
import { VirtualKeyAccessLogTab } from './VirtualKeyAccessLogTab';

export function VirtualKeyDetail() {
  const { t } = useTranslation();
  const state = useVirtualKeyDetail();

  const {
    vk, loading, error, refetch,
    modelsData, projectsData, project,
    regenConfirming, setRegenConfirming, newKey, keyCopied,
    regenerateKey, regenerating, copyNewKey, dismissNewKey,
    isEditing,
    editProjectId, setEditProjectId,
    editSourceApp, setEditSourceApp,
    editEnabled, setEditEnabled,
    editRateLimitRpm, setEditRateLimitRpm,
    editSelectedModels, setEditSelectedModels,
    editExpiresAt, setEditExpiresAt,
    editNeverExpires, setEditNeverExpires,
    updating, startEditing, handleSave, cancelEditing,
    activeTab, setActiveTab,
    auditLogs,
  } = state;

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!vk) return null;

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:virtualKeys.title'), to: '/ai-gateway/virtual-keys' },
        { label: vk.name },
      ]} />

      <PageHeader
        title={vk.name}
        subtitle={t('pages:virtualKeys.title')}
        action={
          !isEditing ? (
            <Button variant="secondary" onClick={startEditing}>{t('common:edit')}</Button>
          ) : undefined
        }
      />

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value="info">{t('pages:virtualKeys.info')}</TabsTrigger>
          <TabsTrigger value="quota">{t('pages:virtualKeys.quotaLimits')}</TabsTrigger>
          <TabsTrigger value="access-log">{t('pages:virtualKeys.accessLog')}</TabsTrigger>
        </TabsList>

        <TabsContent value="info">
          <VirtualKeyInfoTab
            vk={vk}
            project={project}
            modelsData={modelsData ?? undefined}
            projectsData={projectsData ?? undefined}
            regenConfirming={regenConfirming}
            setRegenConfirming={setRegenConfirming}
            newKey={newKey}
            keyCopied={keyCopied}
            regenerateKey={regenerateKey}
            regenerating={regenerating}
            copyNewKey={copyNewKey}
            dismissNewKey={dismissNewKey}
            isEditing={isEditing}
            editProjectId={editProjectId}
            setEditProjectId={setEditProjectId}
            editSourceApp={editSourceApp}
            setEditSourceApp={setEditSourceApp}
            editEnabled={editEnabled}
            setEditEnabled={setEditEnabled}
            editRateLimitRpm={editRateLimitRpm}
            setEditRateLimitRpm={setEditRateLimitRpm}
            editSelectedModels={editSelectedModels}
            setEditSelectedModels={setEditSelectedModels}
            editExpiresAt={editExpiresAt}
            setEditExpiresAt={setEditExpiresAt}
            editNeverExpires={editNeverExpires}
            setEditNeverExpires={setEditNeverExpires}
            updating={updating}
            handleSave={handleSave}
            cancelEditing={cancelEditing}
          />
        </TabsContent>

        <TabsContent value="quota">
          <VirtualKeyQuotaTab vk={vk} />
        </TabsContent>

        <TabsContent value="access-log">
          <VirtualKeyAccessLogTab auditLogs={auditLogs} />
        </TabsContent>
      </Tabs>
    </Stack>
  );
}
