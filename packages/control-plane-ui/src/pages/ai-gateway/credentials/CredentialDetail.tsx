import { useState, useMemo } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { credentialApi, providerApi } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, AlertDialog, Breadcrumb, Skeleton, ErrorBanner,
  Button, Stack, Card,
  Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import type { Credential, Provider } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';
import { formatDateTime } from '@/lib/format';
import { ReliabilityPanel } from './ReliabilityPanel';
import { CredentialInfoTab } from './CredentialDetail.InfoTab';
import styles from './CredentialDetail.module.css';

export function CredentialDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [isEditing, setIsEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);

  // Edit state
  const [editName, setEditName] = useState('');
  const [editEnabled, setEditEnabled] = useState(true);
  const [editApiKey, setEditApiKey] = useState('');
  const [editWeight, setEditWeight] = useState(100);
  const [editStatus, setEditStatus] = useState('active');
  const [editExpiresAt, setEditExpiresAt] = useState('');

  const canUpdate = usePermission('credential:update');
  const canDelete = usePermission('credential:delete');

  const { data: credential, loading, error, refetch } = useApi<Credential>(
    () => credentialApi.get(id!),
    ['admin', 'credentials', 'detail', id],
  );

  const { data: providersData } = useApi<{ data: Provider[] }>(
    () => providerApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'providers', 'list', 'credential-detail'],
  );

  const provider = useMemo(() => {
    if (!credential || !providersData?.data) return null;
    return providersData.data.find(p => p.id === credential.providerId) ?? null;
  }, [credential, providersData]);

  const { mutate: updateCred, loading: updating } = useMutation(
    (data: Record<string, unknown>) => credentialApi.update(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'credentials']],
      onSuccess: () => { setIsEditing(false); refetch(); },
      successMessage: t('pages:credentials.credentialUpdated'),
    },
  );

  // Circuit reset moved to the Reliability tab (ReliabilityPanel) — its
  // own useMutation lives in that component to avoid duplicating UI state
  // between the two tabs.

  const { mutate: deleteCred } = useMutation(
    () => credentialApi.delete(id!),
    {
      invalidateQueries: [['api', 'admin', 'credentials']],
      onSuccess: () => navigate('/ai-gateway/credentials'),
      successMessage: t('pages:credentials.credentialDeleted'),
    },
  );

  const startEditing = () => {
    if (!credential) return;
    setEditName(credential.name);
    setEditEnabled(credential.enabled);
    setEditApiKey('');
    setEditWeight(credential.selectionWeight ?? 100);
    setEditStatus(credential.status ?? 'active');
    setEditExpiresAt(credential.expiresAt ? credential.expiresAt.slice(0, 10) : '');
    setIsEditing(true);
  };

  const handleSave = () => {
    const payload: Record<string, unknown> = {
      name: editName,
      enabled: editEnabled,
      selectionWeight: editWeight,
      status: editStatus,
      expiresAt: editExpiresAt ? `${editExpiresAt}T00:00:00Z` : null,
    };
    if (editApiKey) payload.apiKey = editApiKey;
    updateCred(payload);
  };

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!credential) return null;

  const rotationState = credential.rotationState ?? 'none';

  // Build rotation timeline
  const timeline: { label: string; date: string | undefined; danger?: boolean }[] = [];
  if (credential.createdAt) timeline.push({ label: t('pages:credentials.created'), date: credential.createdAt });
  if (credential.lastRotatedAt) timeline.push({ label: t('pages:credentials.lastRotated'), date: credential.lastRotatedAt });
  if (credential.lastSuccessAt) timeline.push({ label: t('pages:credentials.lastSuccess'), date: credential.lastSuccessAt });
  if (credential.lastFailureAt) timeline.push({ label: t('pages:credentials.lastFailure'), date: credential.lastFailureAt, danger: true });
  timeline.sort((a, b) => new Date(b.date ?? 0).getTime() - new Date(a.date ?? 0).getTime());

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:credentials.title'), to: '/ai-gateway/credentials' },
        { label: credential.name },
      ]} />

      <PageHeader
        title={credential.name}
        subtitle={provider ? t('pages:credentials.providerSubtitleLabel', { name: provider.displayName || provider.name }) : undefined}
        action={
          <Stack direction="horizontal" gap="sm">
            {canUpdate && (
              <Button
                variant="secondary"
                onClick={() => updateCred({ enabled: !credential.enabled })}
                className={styles.credentialStatusButton}
              >{credential.enabled ? t('common:enabled') : t('common:disabled')}</Button>
            )}
            {canDelete && (
              <Button variant="danger" onClick={() => setDeleting(true)}>{t('common:delete')}</Button>
            )}
          </Stack>
        }
      />

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">{t('pages:credentials.information')}</TabsTrigger>
          <TabsTrigger value="reliability">{t('pages:credentials.reliability')}</TabsTrigger>
          <TabsTrigger value="history">{t('pages:credentials.rotationHistory')}</TabsTrigger>
        </TabsList>

        <TabsContent value="info">
          <CredentialInfoTab
            credential={credential}
            provider={provider}
            rotationState={rotationState}
            canUpdate={canUpdate}
            isEditing={isEditing}
            startEditing={startEditing}
            handleSave={handleSave}
            updating={updating}
            setIsEditing={setIsEditing}
            editName={editName}
            setEditName={setEditName}
            editEnabled={editEnabled}
            setEditEnabled={setEditEnabled}
            editApiKey={editApiKey}
            setEditApiKey={setEditApiKey}
            editWeight={editWeight}
            setEditWeight={setEditWeight}
            editStatus={editStatus}
            setEditStatus={setEditStatus}
            editExpiresAt={editExpiresAt}
            setEditExpiresAt={setEditExpiresAt}
          />
        </TabsContent>

        <TabsContent value="reliability">
          <ReliabilityPanel credentialId={id!} canEdit={canUpdate} seed={credential} />
        </TabsContent>

        <TabsContent value="history">
          <h2 className={styles.historyTitle}>{t('pages:credentials.rotationHistory')}</h2>
          <Card>
            {timeline.length === 0 ? (
              <div className={styles.emptyMessage}>
                {t('pages:credentials.noRotationHistory')}
              </div>
            ) : (
              <div>
                {timeline.map((item, idx) => (
                  <div key={idx} className={styles.timelineItem}>
                    <div className={item.danger ? styles.timelineDotDanger : styles.timelineDot} />
                    <div>
                      <div className={styles.timelineLabel}>{item.label}</div>
                      <div className={styles.timelineDate}>{formatDateTime(item.date)}</div>
                      {item.danger && credential.lastFailureReason && (
                        <div className={styles.timelineFailureDetail}>{credential.lastFailureReason}</div>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </Card>
        </TabsContent>
      </Tabs>

      <AlertDialog
        open={deleting}
        onOpenChange={(open) => { if (!open) setDeleting(false); }}
        title={t('pages:credentials.deleteCredential')}
        description={t('pages:credentials.deleteConfirm', { name: credential.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => deleteCred(undefined as never)}
        variant="danger"
      />
    </Stack>
  );
}
