import { useState, useMemo } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useApi } from '@/hooks/useApi';
import { credentialApi, providerApi } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, Badge, statusToVariant, AlertDialog, Breadcrumb, Skeleton, ErrorBanner,
  FormField, Input, Switch, Tooltip, Button, Stack, Card,
  Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import type { Credential, Provider } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';
import { formatDateTime } from '@/lib/format';
import { ReliabilityPanel } from './ReliabilityPanel';
import styles from './CredentialDetail.module.css';

function rotationBadgeClass(state: string, s: Record<string, string>): string {
  switch (state) {
    case 'pending_rotation': return s.rotationStatePendingRotation;
    case 'validating': return s.rotationStateValidating;
    case 'rotated': return s.rotationStateRotated;
    case 'completed': return s.rotationStateCompleted;
    case 'failed': return s.rotationStateFailed;
    default: return s.rotationStateNone;
  }
}

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
      onSuccess: () => { setIsEditing(false); },
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
            {canUpdate && !isEditing && (
              <Button variant="secondary" onClick={startEditing}>{t('common:edit')}</Button>
            )}
            {canUpdate && !isEditing && (
              <Button
                variant="secondary"
                onClick={() => updateCred({ enabled: !credential.enabled })}
              >{credential.enabled ? t('pages:credentials.disable') : t('pages:credentials.enable')}</Button>
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
          <Card>
            {isEditing ? (
              <Stack gap="md">
                <FormField label={t('pages:credentials.name')} required>
                  <Input name="editName" value={editName} onChange={(e) => setEditName(e.target.value)} required />
                </FormField>
                <FormField
                  label={t('pages:credentials.newApiKeyLabel')}
                  helpText={t('pages:credentials.newApiKeyHelpText')}
                >
                  <Input name="editApiKey" value={editApiKey} onChange={(e) => setEditApiKey(e.target.value)} type="password" placeholder={t('pages:credentials.placeholderApiKeyHint')} />
                </FormField>
                <Stack direction="horizontal" gap="sm" align="center">
                  <Switch checked={editEnabled} onCheckedChange={setEditEnabled} />
                  <Tooltip content={t('pages:credentials.enabledTooltip')}>
                    <span>{t('pages:credentials.enabledLabel')}</span>
                  </Tooltip>
                </Stack>
                <FormField label={t('pages:credentials.selectionWeightLabel')} helpText={t('pages:credentials.selectionWeightHelp')}>
                  <Input
                    name="editWeight"
                    type="number"
                    min={0}
                    max={10000}
                    value={String(editWeight)}
                    onChange={(e) => setEditWeight(Number(e.target.value))}
                  />
                </FormField>
                <FormField label={t('pages:credentials.poolStatusLabel')} helpText={t('pages:credentials.poolStatusHelp')}>
                  <select
                    value={editStatus}
                    onChange={(e) => setEditStatus(e.target.value)}
                  >
                    <option value="active">{t('pages:credentials.poolStatus_active')}</option>
                    <option value="retiring">{t('pages:credentials.poolStatus_retiring')}</option>
                  </select>
                </FormField>
                <FormField label={t('pages:providers.credExpiresAtLabel')} helpText={t('pages:providers.credExpiresAtHelp')}>
                  <Input
                    name="editExpiresAt"
                    type="date"
                    value={editExpiresAt}
                    onChange={(e) => setEditExpiresAt(e.target.value)}
                  />
                </FormField>
                <Stack direction="horizontal" gap="sm" justify="end">
                  <Button variant="secondary" onClick={() => setIsEditing(false)}>{t('common:cancel')}</Button>
                  <Button onClick={handleSave} disabled={updating || !editName} loading={updating}>
                    {t('common:save')}
                  </Button>
                </Stack>
              </Stack>
            ) : (
              <div className={styles.kvGrid}>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.name')}</span>
                    <Tooltip content={t('pages:credentials.nameTooltip')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.kvValueBold}>{credential.name}</div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.provider')}</span>
                    <Tooltip content={t('pages:credentials.providerTooltip')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.kvValue}>
                    {provider ? (
                      <Link to={`/ai-gateway/providers/${provider.id}`} className={styles.link}>{provider.displayName || provider.name}</Link>
                    ) : credential.providerId}
                  </div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.status')}</span>
                    <Tooltip content={t('pages:credentials.statusTooltip')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.badgeOffset}><Badge variant={statusToVariant(credential.enabled ? 'enabled' : 'disabled')}>{credential.enabled ? t('common:enabled') : t('common:disabled')}</Badge></div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.rotationState')}</span>
                    <Tooltip content={t('pages:credentials.rotationStateTooltip')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.badgeOffset}><span className={clsx(styles.rotationBadge, rotationBadgeClass(rotationState, styles))}>{rotationState.replace(/_/g, ' ')}</span></div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.storedSecret')}</span>
                    <Tooltip content={t('pages:credentials.storedSecretTooltip')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.kvValueMono}>{t('pages:credentials.notDisplayed')}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.expires')}</div>
                  <div className={styles.kvValue}>
                    {credential.expiresAt ? (
                      <Stack direction="horizontal" gap="xs" align="center">
                        <span>{formatDateTime(credential.expiresAt)}</span>
                        {new Date(credential.expiresAt) < new Date() ? (
                          <Badge variant="danger">{t('pages:credentials.expiresOverdue')}</Badge>
                        ) : rotationState === 'pending_rotation' ? (
                          <Badge variant="warning">{t('pages:credentials.expiringSoon')}</Badge>
                        ) : null}
                      </Stack>
                    ) : '—'}
                  </div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.created')}</div>
                  <div className={styles.kvValue}>{formatDateTime(credential.createdAt)}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.lastUpdated')}</div>
                  <div className={styles.kvValue}>{formatDateTime(credential.updatedAt)}</div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.lastRotated')}</span>
                    <Tooltip content={t('pages:credentials.lastRotatedTooltip')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.kvValue}>{formatDateTime(credential.lastRotatedAt)}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.lastUsedLabel')}</div>
                  <div className={styles.kvValue}>{formatDateTime(credential.lastUsedAt)}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.lastSuccess')}</div>
                  <div className={styles.kvValue}>{formatDateTime(credential.lastSuccessAt)}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.lastFailure')}</div>
                  <div className={styles.kvValue}>
                    {credential.lastFailureAt ? <span className={styles.dangerText}>{formatDateTime(credential.lastFailureAt)}</span> : '--'}
                    {credential.lastFailureReason && (
                      <div className={styles.failureDetail}>{credential.lastFailureReason}</div>
                    )}
                  </div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:credentials.totalUsageCount')}</div>
                  <div className={styles.kvValueMono}>{credential.totalUsageCount.toLocaleString()}</div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.selectionWeightLabel')}</span>
                    <Tooltip content={t('pages:credentials.selectionWeightHelp')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.kvValueMono}>{credential.selectionWeight ?? 100}</div>
                </div>
                <div>
                  <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:credentials.poolStatusLabel')}</span>
                    <Tooltip content={t('pages:credentials.poolStatusHelp')}>
                      <span className={styles.helpIcon}>?</span>
                    </Tooltip>
                  </Stack>
                  <div className={styles.badgeOffset}>
                    <Badge variant={
                      credential.status === 'retiring' ? 'warning' :
                      credential.status === 'retired' ? 'default' : 'success'
                    }>
                      {t(`pages:credentials.poolStatus_${credential.status ?? 'active'}`)}
                    </Badge>
                  </div>
                </div>
                {credential.retireAt && (
                  <div>
                    <div className={styles.kvLabel}>{t('pages:credentials.retireAt')}</div>
                    <div className={styles.kvValue}>{formatDateTime(credential.retireAt)}</div>
                  </div>
                )}
                <div>
                  <span className={styles.kvLabel}>{t('pages:credentials.reliability')}</span>
                  <div className={styles.badgeOffset}>
                    <span className={styles.mutedText}>{t('pages:credentials.seeReliabilityTab')}</span>
                  </div>
                </div>
              </div>
            )}
          </Card>
        </TabsContent>

        <TabsContent value="reliability">
          <ReliabilityPanel credentialId={id!} canEdit={canUpdate} seed={credential} />
        </TabsContent>

        <TabsContent value="history">
          <Card>
            <h2 className={styles.widgetTitle}>{t('pages:credentials.rotationHistory')}</h2>
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
