import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { personalVKApi } from '@/api/services/ai-gateway/personalVirtualKeys';
import { useMutation } from '@/hooks/useMutation';
import { useTheme } from '@/theme/useTheme';
import {
  DataTable, AlertDialog, Badge, statusToVariant,
  Skeleton, ErrorBanner, Button, Stack, Card, SecretDialog,
  RowActions, RowActionIconButton, RowDeleteAction,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { VirtualKey } from '@/api/types';
import styles from './PersonalVKList.module.css';

function RegenerateActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M21 12a9 9 0 0 1-15.3 6.36" />
      <path d="M3 12A9 9 0 0 1 18.3 5.64" />
      <path d="M18 3v4h-4" />
      <path d="M6 21v-4h4" />
    </svg>
  );
}

export function PersonalVKList() {
  const { t } = useTranslation();
  const { brand } = useTheme();
  const navigate = useNavigate();

  const { data, loading, error, refetch } = useApi<{ data: VirtualKey[]; total: number }>(
    () => personalVKApi.list(),
    ['user', 'virtual-keys'],
  );

  const [deleting, setDeleting] = useState<VirtualKey | null>(null);
  const [regeneratedSecret, setRegeneratedSecret] = useState<string | null>(null);

  const { mutate: deleteVk } = useMutation(
    (id: string) => personalVKApi.delete(id),
    {
      onSuccess: () => { setDeleting(null); refetch(); },
      successMessage: t('pages:personalVks.deleted'),
    },
  );

  const { mutate: regenerateVk } = useMutation(
    (id: string) => personalVKApi.regenerate(id),
    {
      onSuccess: (result) => {
        const secret = (result as { key?: string })?.key;
        if (secret) setRegeneratedSecret(secret);
        refetch();
      },
      successMessage: t('pages:personalVks.regenerated'),
    },
  );

  const rows = data?.data ?? [];

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<VirtualKey>[] = [
    { key: 'name', label: t('pages:personalVks.name') },
    {
      key: 'enabled',
      label: t('pages:personalVks.status'),
      render: (r) => (
        <Badge variant={statusToVariant(r.enabled ? 'enabled' : 'disabled')}>
          {r.enabled ? t('common:enabled') : t('common:disabled')}
        </Badge>
      ),
    },
    {
      key: 'rateLimitRpm',
      label: t('pages:personalVks.rpm'),
      render: (r) => r.rateLimitRpm ?? '-',
    },
    {
      key: 'createdAt',
      label: t('pages:personalVks.createdAt'),
      render: (r) => new Date(r.createdAt).toLocaleDateString(),
    },
    {
      key: 'actions',
      label: t('pages:personalVks.actions'),
      render: (r) => (
        <RowActions>
          <RowActionIconButton label={t('pages:personalVks.regenerate')} onAction={() => regenerateVk(r.id)}>
            <RegenerateActionIcon />
          </RowActionIconButton>
          <RowDeleteAction label={t('common:delete')} onAction={() => setDeleting(r)} />
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <Card>
        <Stack direction="horizontal" gap="md" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div>
            <h2 style={{ fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)', marginBottom: 'var(--g-space-1)' }}>{t('pages:personalVks.title')}</h2>
            <p className={styles.descriptionText}>{t('pages:personalVks.description', { productName: brand.productName })}</p>
          </div>
          <Button className={styles.createVkButton} onClick={() => navigate('/settings/personal-vks/new')}>
            {t('pages:personalVks.createVk')}
          </Button>
        </Stack>
      </Card>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={columns}
          data={rows}
          emptyMessage={t('pages:personalVks.noVksConfigured')}
        />
      </Card>

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:personalVks.deleteVk')}
        description={t('pages:personalVks.deleteConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteVk(deleting.id); }}
        variant="danger"
      />

      <SecretDialog
        open={!!regeneratedSecret}
        secret={regeneratedSecret}
        title={t('pages:personalVks.newSecret')}
        warning={t('pages:personalVks.newSecretWarning')}
        onClose={() => setRegeneratedSecret(null)}
      />
    </Stack>
  );
}
