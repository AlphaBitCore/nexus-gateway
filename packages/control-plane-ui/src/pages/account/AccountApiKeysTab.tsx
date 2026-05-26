import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { personalApiKeyApi } from '@/api/services';
import type { CreatePersonalApiKeyInput } from '@/api/services/iam/personalApiKeys';
import {
  Card, Stack, Button, Badge, statusToVariant,
  DataTable, AlertDialog, Dialog, SecretDialog, Skeleton, ErrorBanner,
} from '@/components/ui';
import { useTheme } from '@/theme/useTheme';
import type { DataTableColumn } from '@/components/ui';
import type { AdminApiKey } from '@/api/types';
import styles from './AccountApiKeysTab.module.css';

export function AccountApiKeysTab() {
  const { t } = useTranslation();
  const { brand } = useTheme();

  const { data, loading, error, refetch } = useApi<{ data: AdminApiKey[] }>(
    () => personalApiKeyApi.list(),
    ['my', 'api-keys'],
  );

  const [deleting, setDeleting] = useState<AdminApiKey | null>(null);
  const [showSecret, setShowSecret] = useState<string | null>(null);
  const [showCreateDialog, setShowCreateDialog] = useState(false);
  const [createName, setCreateName] = useState('');

  const { mutate: deleteKey } = useMutation(
    (id: string) => personalApiKeyApi.delete(id),
    {
      onSuccess: () => { setDeleting(null); refetch(); },
      successMessage: t('pages:account.keyDeleted'),
    },
  );

  const { mutate: createKey, loading: creating } = useMutation(
    (data: CreatePersonalApiKeyInput) => personalApiKeyApi.create(data),
    {
      onSuccess: (result) => {
        const secret = (result as { key?: string })?.key;
        if (secret) setShowSecret(secret);
        setShowCreateDialog(false);
        setCreateName('');
        refetch();
      },
      successMessage: t('pages:account.keyCreated'),
    },
  );

  const { mutate: regenerateKey } = useMutation(
    (id: string) => personalApiKeyApi.regenerate(id),
    {
      onSuccess: (result) => {
        const secret = (result as { key?: string })?.key;
        if (secret) setShowSecret(secret);
        refetch();
      },
      successMessage: t('pages:account.keyRegenerated'),
    },
  );

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const keys = data?.data ?? [];

  const fmtDate = (s?: string | null) => {
    if (!s) return '--';
    try { return new Date(s).toLocaleString(); } catch { return s; }
  };

  const columns: DataTableColumn<AdminApiKey>[] = [
    { key: 'name', label: t('pages:account.keyName') },
    {
      key: 'keyPrefix',
      label: t('pages:account.keyPrefix'),
      render: (r) => <code>{r.keyPrefix}...</code>,
    },
    {
      key: 'enabled',
      label: t('pages:account.keyStatus'),
      render: (r) => (
        <Badge variant={statusToVariant(r.enabled ? 'enabled' : 'disabled')}>
          {r.enabled ? t('common:enabled') : t('common:disabled')}
        </Badge>
      ),
    },
    {
      key: 'expiresAt',
      label: t('pages:account.keyExpires'),
      render: (r) => r.expiresAt ? fmtDate(r.expiresAt) : t('pages:account.never'),
    },
    {
      key: 'createdAt',
      label: t('pages:account.keyCreatedAt'),
      render: (r) => fmtDate(r.createdAt),
    },
    {
      key: 'actions',
      label: t('pages:account.keyActions'),
      render: (r) => (
        <Stack direction="horizontal" gap="xs" onClick={(e) => e.stopPropagation()}>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            onClick={(e) => { e.stopPropagation(); regenerateKey(r.id); }}
          >
            {t('pages:account.keyRegenerate')}
          </Button>
          <Button
            type="button"
            variant="danger"
            size="sm"
            onClick={(e) => { e.stopPropagation(); setDeleting(r); }}
          >
            {t('common:delete')}
          </Button>
        </Stack>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <Card>
        <Stack direction="horizontal" gap="md" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div>
            <h2 style={{ fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)', marginBottom: 'var(--g-space-1)' }}>{t('pages:account.apiKeysTitle')}</h2>
            <p className={styles.descriptionText}>{t('pages:account.apiKeysDescription', { productName: brand.productName })}</p>
          </div>
          <Button type="button" onClick={() => setShowCreateDialog(true)}>
            {t('pages:account.createKey')}
          </Button>
        </Stack>
      </Card>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={columns}
          data={keys}
          emptyMessage={t('pages:account.noApiKeys')}
        />
      </Card>

      {/* Create Dialog — only name, role is inherited from user */}
      <Dialog
        open={showCreateDialog}
        onOpenChange={(open) => { if (!open) { setShowCreateDialog(false); setCreateName(''); } }}
        title={t('pages:account.createKey')}
        size="sm"
      >
        <form onSubmit={(e) => { e.preventDefault(); if (createName) createKey({ name: createName }); }}>
          <Stack gap="md">
            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-1)' }}>
              <label style={{ fontSize: 'var(--g-font-size-xs)', fontWeight: 'var(--g-font-weight-medium)', color: 'var(--color-text-secondary)' }}>
                {t('pages:account.keyName')} *
              </label>
              <input
                type="text"
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
                placeholder={t('pages:account.keyNamePlaceholder')}
                required
                autoFocus
                style={{
                  height: '36px', padding: 'var(--g-space-0) var(--g-space-3)',
                  border: '1px solid var(--color-border)', borderRadius: 'var(--g-radius-sm)',
                  fontSize: 'var(--g-font-size-base)', fontFamily: 'var(--g-font-sans)',
                  color: 'var(--color-text)', background: 'var(--color-surface)',
                }}
              />
            </div>
            <Stack direction="horizontal" gap="sm" style={{ justifyContent: 'flex-end' }}>
              <Button type="button" variant="secondary" onClick={() => { setShowCreateDialog(false); setCreateName(''); }}>
                {t('common:cancel')}
              </Button>
              <Button type="submit" disabled={creating || !createName}>
                {creating ? t('pages:account.keyCreating') : t('pages:account.createKey')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Dialog>

      {/* Delete Confirmation */}
      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:account.deleteKeyTitle')}
        description={t('pages:account.deleteKeyConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteKey(deleting.id); }}
        variant="danger"
      />

      {/* Secret Display Dialog */}
      <SecretDialog
        open={!!showSecret}
        secret={showSecret}
        title={t('pages:account.newKeySecret')}
        warning={t('pages:account.newKeySecretWarning')}
        onClose={() => setShowSecret(null)}
      />
    </Stack>
  );
}
