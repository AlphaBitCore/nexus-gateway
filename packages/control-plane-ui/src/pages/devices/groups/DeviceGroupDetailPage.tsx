import { useState, useCallback, useMemo } from 'react';
import { Link, useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  deviceGroupsApi, hubApi,
  type DeviceGroupDetail,
} from '@/api/services';
import {
  DataTable,
  AlertDialog,
  Dialog,
  Skeleton,
  ErrorBanner,
  Button,
  Stack,
  SearchableCombobox,
  RowActions,
  RowDeleteAction,
  type DataTableColumn,
} from '@/components/ui';
import { DeviceGroupForm } from './DeviceGroupForm';
import { SmartMembershipCard, GroupBulkActionsCard } from './GroupAdvancedSections';
import styles from './DeviceGroupDetailPage.module.css';

export function DeviceGroupDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { t } = useTranslation();
  const canUpdate = usePermission('device-groups:update');
  const canDelete = usePermission('device-groups:delete');

  const [showAddMember, setShowAddMember] = useState(false);
  const [newDeviceId, setNewDeviceId] = useState('');
  const [newDeviceLabel, setNewDeviceLabel] = useState('');
  const [removingMember, setRemovingMember] = useState<{ deviceId: string; hostname: string } | null>(null);
  const [editBasicsOpen, setEditBasicsOpen] = useState(false);
  const [deleteGroupOpen, setDeleteGroupOpen] = useState(false);

  const { data, loading, error, refetch } = useApi<DeviceGroupDetail>(
    () => deviceGroupsApi.get(id!),
    ['admin', 'device-groups', 'detail', id],
    { skip: !id },
  );

  const { mutate: addMember, loading: addingMember } = useMutation(
    (deviceId: string) => deviceGroupsApi.addMember(id!, deviceId),
    {
      invalidateQueries: [['admin', 'device-groups', 'detail', id]],
      onSuccess: () => { setNewDeviceId(''); setNewDeviceLabel(''); setShowAddMember(false); refetch(); },
    },
  );

  const { mutate: removeMember } = useMutation(
    (deviceId: string) => deviceGroupsApi.removeMember(id!, deviceId),
    {
      invalidateQueries: [['admin', 'device-groups', 'detail', id]],
      onSuccess: () => { setRemovingMember(null); refetch(); },
    },
  );

  const { mutate: deleteGroup, loading: deleteGroupLoading } = useMutation(
    () => deviceGroupsApi.delete(id!),
    {
      invalidateQueries: [['admin', 'device-groups']],
      successMessage: t('pages:deviceGroups.deleteSuccess'),
      onSuccess: () => {
        setDeleteGroupOpen(false);
        navigate('/devices/groups');
      },
    },
  );

  const existingMemberIds = useMemo(
    () => new Set(data?.memberships.map(m => m.device.id) ?? []),
    [data?.memberships],
  );

  const fetchDevices = useCallback(async (query: string) => {
    const res = await hubApi.listNodes({ type: 'agent', search: query || undefined, pageSize: 50 });
    const items = res.nodes ?? [];
    return items
      .filter(t => !existingMemberIds.has(t.id))
      .map(t => {
        // The Membership picker used to show `t.name` only, which is the
        // agent's WS-registered name. Many agents enrol before ever sending
        // a hostname-carrying heartbeat, so that field is often empty —
        // every blank row in the dropdown was an enrolled-but-quiet agent.
        // Fall through hostname → name → short id so the row is always
        // identifiable, then append OS + currently-logged-in user (the
        // signal users actually care about per the device-list page).
        const primary = (t.hostname?.trim()) || (t.name?.trim()) || `agent ${t.id.slice(0, 8)}`;
        const parts: string[] = [primary];
        if (t.os) {
          parts.push(t.os === 'darwin' ? 'macOS' : t.os === 'windows' ? 'Windows' : t.os);
        }
        if (t.boundUserDisplayName) {
          parts.push(t.boundUserDisplayName);
        }
        return { id: t.id, label: parts.join(' · ') };
      });
  }, [existingMemberIds]);

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  const memberColumns: DataTableColumn<typeof data.memberships[0]>[] = [
    { key: 'hostname', label: t('pages:deviceGroups.deviceHostname'), render: (m) => m.device.hostname },
    { key: 'os', label: t('pages:deviceGroups.deviceOs'), render: (m) => m.device.os === 'darwin' ? 'macOS' : m.device.os === 'windows' ? 'Windows' : m.device.os },
    { key: 'status', label: t('pages:deviceGroups.deviceStatus'), render: (m) => m.device.status },
    {
      // Surface auto-expiry when set. Empty cell = permanent.
      key: 'expiresAt',
      label: t('pages:deviceGroups.memberExpiresAt', 'Expires'),
      render: (m) => m.expiresAt
        ? <span title={m.expiresAt}>{new Date(m.expiresAt).toLocaleString()}</span>
        : <span style={{ color: 'var(--color-text-muted)' }}>{'—'}</span>,
    },
    {
      key: 'actions',
      label: t('common:actions', 'Actions'),
      render: (m) => canUpdate ? (
        <RowActions>
          <RowDeleteAction
            label={t('common:remove')}
            onAction={() => setRemovingMember({ deviceId: m.device.id, hostname: m.device.hostname })}
          />
        </RowActions>
      ) : null,
    },
  ];

  return (
    <Stack gap="lg">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link to="/devices/groups" className={styles.backLink} aria-label={t('common:back')}>
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.detailTitle}>{data.name}</h1>
            {data.description ? <p className={styles.detailSubtitle}>{data.description}</p> : null}
          </div>
          <div className={styles.headerAside}>
            <Stack direction="horizontal" gap="sm" justify="end">
              {canUpdate && (
                <Button variant="primary" onClick={() => setEditBasicsOpen(true)}>
                  {t('pages:deviceGroups.editBasics')}
                </Button>
              )}
              {canDelete && (
                <Button
                  variant="danger"
                  disabled={deleteGroupLoading}
                  onClick={() => setDeleteGroupOpen(true)}
                >
                  {t('pages:deviceGroups.deleteGroup')}
                </Button>
              )}
            </Stack>
            <div className={styles.headerMeta}>
              <div className={styles.headerMetaItem}>
                <span>{t('pages:deviceGroups.created')}</span>
                <strong>{new Date(data.createdAt).toLocaleString()}</strong>
              </div>
              <div className={styles.headerMetaItem}>
                <span>{t('pages:deviceGroups.updated')}</span>
                <strong>{new Date(data.updatedAt).toLocaleString()}</strong>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section className={styles.membersSection}>
        <Stack gap="md">
          <Stack direction="horizontal" gap="md">
            <h3 className={styles.membersTitle}>{t('pages:deviceGroups.membersSection')}</h3>
            {canUpdate && (
              <Button className={styles.addDeviceButton} onClick={() => setShowAddMember(!showAddMember)}>
                {showAddMember ? t('common:cancel') : t('pages:deviceGroups.addMember')}
              </Button>
            )}
          </Stack>
          {showAddMember && (
            <Stack direction="horizontal" gap="sm" style={{ alignItems: 'flex-end' }}>
              <div style={{ flex: 1 }}>
                <SearchableCombobox
                  valueId={newDeviceId}
                  valueLabel={newDeviceLabel}
                  placeholder={t('pages:deviceGroups.searchDevicePlaceholder')}
                  ariaLabel={t('pages:deviceGroups.deviceIdPlaceholder')}
                  fetchOptions={fetchDevices}
                  onSelect={(opt) => {
                    setNewDeviceId(opt?.id ?? '');
                    setNewDeviceLabel(opt?.label ?? '');
                  }}
                  allowEmptyQueryFetch
                />
              </div>
              <Button onClick={() => addMember(newDeviceId.trim())} disabled={!newDeviceId.trim() || addingMember}>
                {t('common:add')}
              </Button>
            </Stack>
          )}
          <DataTable columns={memberColumns} data={data.memberships} hideSearch />
        </Stack>
      </section>

      {/* Advanced sections — smart membership / group config / bulk actions */}
      <SmartMembershipCard
        groupId={id!}
        currentQuery={(data as DeviceGroupDetail).membershipQuery ?? null}
        canUpdate={canUpdate}
        onSaved={refetch}
      />
      <GroupBulkActionsCard groupId={id!} canUpdate={canUpdate} />

      {deleteGroupOpen && (
        <AlertDialog
          open={deleteGroupOpen}
          onOpenChange={(open) => {
            if (!open) setDeleteGroupOpen(false);
          }}
          title={t('pages:deviceGroups.deleteTitle')}
          description={t('pages:deviceGroups.deleteDescription', {
            name: data.name,
            members: data.memberships.length,
          })}
          confirmLabel={t('common:delete')}
          cancelLabel={t('common:cancel')}
          variant="danger"
          onConfirm={() => {
            void deleteGroup(undefined);
          }}
        />
      )}

      {removingMember && (
        <AlertDialog
          open={!!removingMember}
          onOpenChange={(v) => !v && setRemovingMember(null)}
          title={t('pages:deviceGroups.removeMemberTitle')}
          description={t('pages:deviceGroups.removeMemberDescription', { hostname: removingMember.hostname })}
          confirmLabel={t('common:remove')}
          variant="danger"
          onConfirm={() => removeMember(removingMember.deviceId)}
        />
      )}

      <DeviceGroupForm
        open={editBasicsOpen}
        group={data}
        onClose={() => setEditBasicsOpen(false)}
        onSaved={() => {
          void refetch();
        }}
        invalidateExtra={[['admin', 'device-groups', 'detail', id!]]}
      />
    </Stack>
  );
}
