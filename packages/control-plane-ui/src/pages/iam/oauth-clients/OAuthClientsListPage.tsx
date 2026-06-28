import { useState, useCallback, useRef } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { oauthClientApi, type OAuthClient } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable, Card, Skeleton, ErrorBanner, Button, Stack,
  RowActions, RowActionIconButton, OpenActionIcon, DeleteActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { DeleteClientConfirmDialog } from './components/DeleteClientConfirmDialog';
import styles from './OAuthClientsListPage.module.css';

/**
 * OAuthClientsListPage — admin list of registered OAuth clients
 * (third-party applications that authenticate to /oauth/token).
 *
 * Row click navigates to the detail page; the kebab carries Delete
 * only — Edit and Rotate live on the detail page where the full
 * context (active refresh tokens, last rotation, etc.) is available.
 */
export function OAuthClientsListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const canCreate = usePermission('oauth-client:create');
  const canDelete = usePermission('oauth-client:delete');
  const { data, loading, error, refetch } = useApi<{ data: OAuthClient[] }>(
    () => oauthClientApi.list(),
    ['admin', 'oauth-clients', 'list'],
  );

  // Delete flow needs the activeRefreshTokenCount so the admin sees the
  // cascade blast radius. The list endpoint omits it (kept cheap), so we
  // fetch the single client on demand when the kebab is clicked.
  const [deleting, setDeleting] = useState<OAuthClient | null>(null);
  const [deletingTokenCount, setDeletingTokenCount] = useState<number | null>(null);

  // Pin the in-flight request to the client whose dialog the user opened.
  // A naked setState would let a late getOne(A) overwrite the count after the
  // admin already cancelled A and opened a fresh dialog for B.
  const openForRef = useRef<string | null>(null);

  const openDelete = useCallback(async (client: OAuthClient) => {
    setDeleting(client);
    setDeletingTokenCount(null);
    openForRef.current = client.id;
    try {
      const resp = await oauthClientApi.getOne(client.id);
      if (openForRef.current !== client.id) return; // stale response — admin moved on
      setDeletingTokenCount(resp.data.activeRefreshTokenCount ?? 0);
    } catch {
      if (openForRef.current !== client.id) return;
      // Surfacing 0 is safe — the dialog still requires type-to-confirm and
      // the server-side cascade is authoritative regardless of the count.
      setDeletingTokenCount(0);
    }
  }, []);

  const closeDelete = useCallback(() => {
    openForRef.current = null;
    setDeleting(null);
    setDeletingTokenCount(null);
  }, []);

  const { mutate: deleteClient, loading: deletingInFlight } = useMutation(
    (id: string) => oauthClientApi.remove(id),
    {
      invalidateQueries: [['admin', 'oauth-clients']],
      onSuccess: () => { closeDelete(); },
      successMessage: t('pages:iam.oauthClients.toastDeleted'),
    },
  );

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const rows = data?.data ?? [];

  const columns: DataTableColumn<OAuthClient>[] = [
    {
      key: 'id',
      label: t('pages:iam.oauthClients.columnId'),
      render: (r) => <span className={styles.idCell}>{r.id}</span>,
    },
    {
      key: 'name',
      label: t('pages:iam.oauthClients.columnName'),
    },
    {
      key: 'type',
      label: t('pages:iam.oauthClients.columnType'),
      render: (r) => (
        <span className={r.type === 'public' ? styles.typeBadgePublic : styles.typeBadgeConfidential}>
          {r.type === 'public'
            ? t('pages:iam.oauthClients.typePublic')
            : t('pages:iam.oauthClients.typeConfidential')}
        </span>
      ),
    },
    {
      key: 'redirectUris',
      label: t('pages:iam.oauthClients.columnRedirectUris'),
      render: (r) => (
        <span className={styles.countCell} title={r.redirectUris.join('\n')}>
          {r.redirectUris.length}
        </span>
      ),
    },
    {
      key: 'allowedScopes',
      label: t('pages:iam.oauthClients.columnAllowedScopes'),
      render: (r) => (
        <span className={styles.countCell} title={r.allowedScopes.join(', ')}>
          {r.allowedScopes.length}
        </span>
      ),
    },
    {
      key: 'createdAt',
      label: t('pages:iam.oauthClients.columnCreated'),
      render: (r) => new Date(r.createdAt).toLocaleDateString(),
    },
    {
      key: 'actions',
      label: t('common:actions', 'Actions'),
      render: (r) => (
        <RowActions>
          <RowActionIconButton
            label={t('common:view', 'View')}
            onAction={() => navigate(`/iam/oauth-clients/${r.id}`)}
          >
            <OpenActionIcon />
          </RowActionIconButton>
          {canDelete && (
            <RowActionIconButton
              label={t('common:delete')}
              tone="danger"
              onAction={() => openDelete(r)}
            >
              <DeleteActionIcon />
            </RowActionIconButton>
          )}
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:iam.oauthClients.pageTitle')}
        subtitle={t('pages:iam.oauthClients.pageDescription')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/iam/oauth-clients/new')}>
              {t('pages:iam.oauthClients.createButton')}
            </Button>
          ) : undefined
        }
      />

      <Card padding="none" className={styles.tableCard}>
        <DataTable
          hideSearch
          frameless
          onRowClick={(row) => navigate(`/iam/oauth-clients/${row.id}`)}
          columns={columns}
          data={rows}
          emptyMessage={t('pages:iam.oauthClients.emptyState')}
        />
      </Card>

      {deleting && (
        <DeleteClientConfirmDialog
          open
          clientId={deleting.id}
          activeRefreshTokenCount={deletingTokenCount ?? 0}
          loading={deletingInFlight || deletingTokenCount === null}
          onCancel={closeDelete}
          onConfirm={() => deleteClient(deleting.id)}
        />
      )}
    </Stack>
  );
}
