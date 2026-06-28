/**
 * InterceptionDomainDetailPage — domain summary card, allowlist note, and a
 * nested InterceptionPath sub-table with Add / Edit / Delete dialogs. All
 * actions invalidate the `['admin', 'interception-domains', 'detail', id]`
 * query so reloads reflect the write without extra orchestration.
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import styles from './InterceptionDomainDetailPage.module.css';

import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { interceptionDomainApi } from '@/api/services';
import type {
  InterceptionDomain,
  InterceptionDomainUpdatePayload,
  InterceptionPath,
  InterceptionPathCreatePayload,
  InterceptionPathUpdatePayload,
} from '@/api/services';
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  DataTable,
  ErrorBanner,
  EditActionIcon,
  Skeleton,
  Stack,
  Switch,
  RowActions,
  RowActionIconButton,
  RowDeleteAction,
  statusToVariant,
  type DataTableColumn,
} from '@/components/ui';
import { InterceptionDomainForm } from './InterceptionDomainForm';
import { InterceptionPathForm } from './InterceptionPathForm';

function SummaryField({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className={styles.summaryField}>
      <span className={styles.summaryFieldLabel}>{label}</span>
      <span className={styles.summaryFieldValue}>{value}</span>
    </div>
  );
}

export function InterceptionDomainDetailPage() {
  const { t } = useTranslation();
  const { id = '' } = useParams<{ id: string }>();

  const { data, loading, error, refetch } = useApi<InterceptionDomain>(
    () => interceptionDomainApi.get(id),
    ['admin', 'interception-domains', 'detail', id],
    { skip: !id },
  );

  const [editing, setEditing] = useState(false);
  const [addingPath, setAddingPath] = useState(false);
  const [editingPath, setEditingPath] = useState<InterceptionPath | null>(null);
  const [deletingPath, setDeletingPath] = useState<InterceptionPath | null>(null);

  const invalidateKeys = [
    ['api', 'admin', 'interception-domains', 'detail', id],
    ['api', 'admin', 'interception-domains'],
  ];

  const { mutate: updateDomain, loading: domainUpdateLoading } = useMutation(
    (payload: InterceptionDomainUpdatePayload) =>
      interceptionDomainApi.update(id, payload),
    {
      invalidateQueries: invalidateKeys,
      successMessage: t('pages:interceptionDomains.updateSuccess', 'Interception domain updated'),
    },
  );

  const { mutate: createPath } = useMutation(
    (payload: InterceptionPathCreatePayload) =>
      interceptionDomainApi.createPath(id, payload),
    {
      invalidateQueries: invalidateKeys,
      successMessage: 'Path added',
    },
  );

  const { mutate: updatePath } = useMutation(
    (args: { pathId: string; payload: InterceptionPathUpdatePayload }) =>
      interceptionDomainApi.updatePath(id, args.pathId, args.payload),
    {
      invalidateQueries: invalidateKeys,
      successMessage: 'Path updated',
    },
  );

  const { mutate: deletePath } = useMutation(
    (pathId: string) => interceptionDomainApi.deletePath(id, pathId),
    {
      invalidateQueries: invalidateKeys,
      successMessage: t('pages:interceptionDomains.pathDeleted', 'Path deleted'),
      onSuccess: () => setDeletingPath(null),
    },
  );

  if (!id) return <ErrorBanner message={t('pages:interceptionDomains.errMissingId', 'Missing :id in URL')} />;
  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return <ErrorBanner message={t('pages:interceptionDomains.errNotFound', 'Interception domain not found')} />;

  const paths = data.paths ?? [];

  const pathColumns: DataTableColumn<InterceptionPath>[] = [
    {
      key: 'pathPattern',
      label: t('pages:interceptionDomains.pathPattern', 'Path patterns'),
      render: (r) => (
        <code style={{ fontSize: 'var(--g-font-size-xs)' }}>
          {(r.pathPattern ?? []).join(', ')}
        </code>
      ),
    },
    {
      key: 'matchType',
      label: t('pages:interceptionDomains.pathMatchType', 'Match'),
      render: (r) =>
        t(`pages:interceptionDomains.enums.${r.matchType}`, r.matchType),
    },
    {
      key: 'action',
      label: t('pages:interceptionDomains.action', 'Action'),
      render: (r) => (
        <Badge variant={r.action === 'BLOCK' ? 'danger' : r.action === 'PASSTHROUGH' ? 'info' : 'success'}>
          {t(`pages:interceptionDomains.enums.${r.action}`, r.action)}
        </Badge>
      ),
    },
    {
      key: 'priority',
      label: t('pages:interceptionDomains.pathPriority', 'Priority'),
    },
    {
      key: 'enabled',
      label: t('pages:interceptionDomains.pathEnabled', 'Enabled'),
      render: (r) => (
        <Badge variant={statusToVariant(r.enabled ? 'enabled' : 'disabled')}>
          {r.enabled ? t('common:enabled', 'Enabled') : t('common:disabled', 'Disabled')}
        </Badge>
      ),
    },
    {
      key: 'actions',
      label: t('common:actions', 'Actions'),
      sortable: false,
      render: (r) => (
        <RowActions>
          <RowActionIconButton label={t('common:edit', 'Edit')} onAction={() => setEditingPath(r)}>
            <EditActionIcon />
          </RowActionIconButton>
          <RowDeleteAction label={t('common:delete', 'Delete')} onAction={() => setDeletingPath(r)} />
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link
            to="/compliance/interception-domains"
            className={styles.backLink}
            aria-label={t('common:back', 'Back')}
          >
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.detailTitle}>{data.name}</h1>
            {data.description ? <p className={styles.detailSubtitle}>{data.description}</p> : null}
            <div className={styles.infoCallout}>
              <div className={styles.infoCalloutBody}>
                {t(
                  'pages:interceptionDomains.allowlistNote',
                  'Enabled domains with matching host_pattern automatically appear in the compliance-proxy domain allowlist — no separate configuration needed.',
                )}
              </div>
            </div>
          </div>
        </div>
      </section>

      <div className={styles.detailToolbar}>
        <Button variant="primary" onClick={() => setEditing(true)}>
          {t('pages:interceptionDomains.editDomain', 'Edit domain')}
        </Button>
      </div>

      <Card>
        <div className={styles.summaryGrid}>
          <SummaryField
            label={t('pages:interceptionDomains.hostPattern', 'Host pattern')}
            value={<code>{data.hostPattern}</code>}
          />
          <SummaryField
            label={t('pages:interceptionDomains.hostMatchType', 'Host match type')}
            value={t(
              `pages:interceptionDomains.enums.${data.hostMatchType}`,
              data.hostMatchType,
            )}
          />
          <SummaryField
            label={t('pages:interceptionDomains.adapterId', 'Adapter')}
            value={<code>{data.adapterId}</code>}
          />
          <SummaryField
            label={t('pages:interceptionDomains.priority', 'Priority')}
            value={data.priority}
          />
          <SummaryField
            label={t('pages:interceptionDomains.enabled', 'Enabled')}
            value={
              <Stack direction="horizontal" gap="sm" align="center">
                <Switch
                  checked={data.enabled}
                  disabled={domainUpdateLoading}
                  aria-label={t(
                    'pages:interceptionDomains.toggleDomainEnabledAria',
                    'Enable or disable interception for domain {{name}}',
                    { name: data.name },
                  )}
                  onCheckedChange={(enabled) => {
                    if (enabled === data.enabled) return;
                    void updateDomain({ enabled });
                  }}
                />
                <Badge variant={statusToVariant(data.enabled ? 'enabled' : 'disabled')}>
                  {data.enabled
                    ? t('common:enabled', 'Enabled')
                    : t('common:disabled', 'Disabled')}
                </Badge>
              </Stack>
            }
          />
          <SummaryField
            label={t(
              'pages:interceptionDomains.defaultPathAction',
              'Default path action',
            )}
            value={t(
              `pages:interceptionDomains.enums.${data.defaultPathAction}`,
              data.defaultPathAction,
            )}
          />
          <SummaryField
            label={t('pages:interceptionDomains.onAdapterError', 'On adapter error')}
            value={t(
              `pages:interceptionDomains.enums.${data.onAdapterError}`,
              data.onAdapterError,
            )}
          />
          <SummaryField
            label={t('pages:interceptionDomains.networkZone', 'Network zone')}
            value={t(
              `pages:interceptionDomains.enums.${data.networkZone}`,
              data.networkZone,
            )}
          />
          <SummaryField
            label={t('pages:interceptionDomains.updatedAt', 'Updated')}
            value={data.updatedAt ? new Date(data.updatedAt).toLocaleString() : '-'}
          />
          <SummaryField
            label={t('pages:interceptionDomains.createdAt', 'Created')}
            value={data.createdAt ? new Date(data.createdAt).toLocaleString() : '-'}
          />
        </div>
      </Card>

      <div className={styles.pathsHeader}>
        <div className={styles.pathsHeaderCopy}>
          <h2 className={styles.pathsTitle}>{t('pages:interceptionDomains.paths', 'Paths')}</h2>
          <p className={styles.pathsSubtitle}>
            {t(
              'pages:interceptionDomains.pathsSubtitle',
              'Rules applied to requests whose host matches this domain. Evaluated in priority order; unmatched requests fall back to the domain default.',
            )}
          </p>
        </div>
        <div className={styles.pathsHeaderAction}>
          <Button variant="primary" onClick={() => setAddingPath(true)}>
            {t('pages:interceptionDomains.addPath', 'Add path')}
          </Button>
        </div>
      </div>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={pathColumns}
          data={paths}
          emptyMessage={t(
            'pages:interceptionDomains.noPaths',
            'No paths configured — the default action applies to every request.',
          )}
        />
      </Card>

      <InterceptionDomainForm
        open={editing}
        mode="edit"
        initial={data}
        onClose={() => setEditing(false)}
        onSubmit={async (payload) => {
          await updateDomain(payload as InterceptionDomainUpdatePayload);
        }}
      />

      <InterceptionPathForm
        open={addingPath}
        mode="create"
        initial={null}
        onClose={() => setAddingPath(false)}
        onSubmit={async (payload) => {
          await createPath(payload as InterceptionPathCreatePayload);
        }}
      />

      <InterceptionPathForm
        open={editingPath !== null}
        mode="edit"
        initial={editingPath}
        onClose={() => setEditingPath(null)}
        onSubmit={async (payload) => {
          if (!editingPath) return;
          await updatePath({
            pathId: editingPath.id,
            payload: payload as InterceptionPathUpdatePayload,
          });
        }}
      />

      <AlertDialog
        open={deletingPath !== null}
        onOpenChange={(open) => {
          if (!open) setDeletingPath(null);
        }}
        title={t('pages:interceptionDomains.deletePathTitle', 'Delete path?')}
        description={
          deletingPath
            ? t(
                'pages:interceptionDomains.confirmDeletePath',
                'Delete path "{{pattern}}"? Compliance proxy will apply the default path action to matching requests immediately.',
                { pattern: (deletingPath.pathPattern ?? []).join(', ') },
              )
            : ''
        }
        confirmLabel={t('common:delete', 'Delete')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={() => {
          if (deletingPath) void deletePath(deletingPath.id);
        }}
        variant="danger"
      />
    </Stack>
  );
}
