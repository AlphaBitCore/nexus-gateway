import { useState, useCallback, useEffect } from 'react';
import { useParams, useNavigate, useSearchParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { organizationApi, iamApi } from '@/api/services';
import { useMutation } from '../../../hooks/useMutation';
import { usePermission } from '../../../hooks/usePermission';
import { useZodForm, FormInput, FormSwitch } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import { z } from 'zod';
import {
  PageHeader, Badge, statusToVariant, DataTable, AlertDialog, Breadcrumb,
  Skeleton, ErrorBanner, Tooltip, FormField, Switch, OrgTreeSelect,
  Button, Stack, Card, Tabs, TabsList, TabsTrigger, TabsContent,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { Organization, Project, AdminUser } from '../../../api/types';
import { formatDate } from '@/lib/format';
import styles from './OrganizationDetail.module.css';

/* ── Schema ────────────────────────────────────────────────────────────── */

const orgEditSchema = z.object({
  name: z.string().min(1),
  code: z.string().min(1),
  description: z.string().optional().default(''),
  contactName: z.string().optional().default(''),
  contactEmail: z.string().optional().default(''),
  contactPhone: z.string().optional().default(''),
  enabled: z.boolean(),
  timezone: z.string().optional().default(''),
});

type OrgEditValues = z.infer<typeof orgEditSchema>;

/* ── Avatar cell ────────────────────────────────────────────────────────── */

function AvatarCell({ name }: { name: string }) {
  const initials = name.split(' ').filter(Boolean).map((w) => w[0]).join('').slice(0, 2).toUpperCase();
  return (
    <span style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2-5)' }}>
      <span style={{
        width: 28, height: 28, borderRadius: '50%',
        background: 'var(--color-info-light)',
        color: 'var(--color-primary)', fontSize: 'var(--g-font-size-xs)', fontWeight: 'var(--g-font-weight-bold)',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        flexShrink: 0, textTransform: 'uppercase', letterSpacing: '0.04em',
        border: '1px solid var(--color-info-light)',
      }}>
        {initials}
      </span>
      <span>{name}</span>
    </span>
  );
}

/* ── Component ──────────────────────────────────────────────────────────── */

export function OrganizationDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [isEditing, setIsEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [tip, setTip] = useState<{ text: string; x: number; y: number } | null>(null);
  const [editParentId, setEditParentId] = useState<string>('');

  /* ── Members tab state ── */
  const [includeSubTeams, setIncludeSubTeams] = useState(false);
  const [membersOffset, setMembersOffset] = useState(0);
  const [membersLimit, setMembersLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE as AdminListPageSize);

  const showTip = useCallback((text: string, e: React.MouseEvent) => {
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    setTip({ text, x: rect.left, y: rect.top - 6 });
    setTimeout(() => setTip(null), 3000);
  }, []);

  const form = useZodForm<OrgEditValues>({
    schema: orgEditSchema,
    defaultValues: {
      name: '', code: '', description: '', contactName: '',
      contactEmail: '', contactPhone: '', enabled: true, timezone: '',
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const canUpdate = usePermission('organization:update');
  const canDelete = usePermission('organization:delete');
  const canCreateUser = usePermission('user:create');

  const { data: org, loading, error, refetch } = useApi<Organization>(
    () => organizationApi.get(id!),
    ['admin', 'organizations', 'detail', id],
  );

  const { data: membersData, loading: membersLoading, error: membersError, refetch: refetchMembers } = useApi<{ data: AdminUser[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        organizationId: id!,
        limit: String(membersLimit),
        offset: String(membersOffset),
      };
      if (includeSubTeams) params.includeSubOrgs = 'true';
      return iamApi.listUsers(params);
    },
    ['admin', 'iam', 'users', 'by-org', id, includeSubTeams, membersOffset, membersLimit],
  );

  const memberRows = membersData?.data ?? [];
  const membersTotal = membersData?.total ?? 0;

  const { mutate: saveOrg, loading: saveLoading } = useMutation(
    (data: unknown) => organizationApi.update(id!, data as Record<string, unknown>),
    {
      invalidateQueries: [['api', 'admin', 'organizations']],
      onSuccess: () => { setIsEditing(false); },
      successMessage: t('pages:organizations.organizationUpdated'),
    },
  );

  const { mutate: deleteOrg } = useMutation(
    () => organizationApi.delete(id!),
    {
      invalidateQueries: [['api', 'admin', 'organizations']],
      onSuccess: () => navigate('/iam/organizations'),
      successMessage: t('pages:organizations.organizationDeleted'),
    },
  );

  useEffect(() => {
    if (org && searchParams.get('edit') === 'true' && !isEditing) {
      startEditing();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [org]);

  const startEditing = () => {
    if (!org) return;
    form.reset({
      name: org.name,
      code: org.code,
      description: org.description ?? '',
      contactName: org.contactName ?? '',
      contactEmail: org.contactEmail ?? '',
      contactPhone: org.contactPhone ?? '',
      enabled: org.enabled,
      timezone: org.timezone ?? '',
    });
    setEditParentId(org.parentId ?? '');
    setIsEditing(true);
  };

  const handleSave = () => {
    const values = form.getValues();
    const currentParentId = org?.parentId ?? '';
    const payload: Record<string, unknown> = {
      name: values.name, code: values.code, description: values.description || undefined,
      contactName: values.contactName || undefined, contactEmail: values.contactEmail || undefined,
      contactPhone: values.contactPhone || undefined, enabled: values.enabled,
      timezone: values.timezone || undefined,
    };
    // Only send parentId when it actually changed (avoids unnecessary path recomputation).
    if (editParentId !== currentParentId) {
      payload.parentId = editParentId; // "" = move to root, id = reparent
    }
    saveOrg(payload);
  };

  const editName = form.watch('name');
  const editCode = form.watch('code');

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!org) return null;

  const childCount = org._count?.children ?? org.children?.length ?? 0;
  const projectCount = org._count?.projects ?? org.projects?.length ?? 0;
  const canDeleteOrg = childCount === 0 && projectCount === 0;

  const projectColumns: DataTableColumn<Project>[] = [
    { key: 'name', label: t('pages:organizations.name') },
    { key: 'code', label: t('pages:organizations.code'), render: (r) => <code className={styles.monoCode}>{r.code}</code> },
    { key: 'status', label: t('pages:organizations.status'), render: (r) => <Badge variant={statusToVariant(r.status)}>{r.status}</Badge> },
    { key: 'createdAt', label: t('pages:organizations.created'), render: (r) => formatDate(r.createdAt) },
  ];

  const childColumns: DataTableColumn<Organization>[] = [
    { key: 'name', label: t('pages:organizations.name') },
    { key: 'code', label: t('pages:organizations.code'), render: (r) => <code className={styles.monoCode}>{r.code}</code> },
    { key: '_count', label: t('pages:organizations.projects'), render: (r) => String(r._count?.projects ?? 0) },
    { key: 'enabled', label: t('pages:organizations.status'), render: (r) => <Badge variant={statusToVariant(r.enabled ? 'active' : 'disabled')}>{r.enabled ? t('pages:organizations.active') : t('pages:organizations.disabled')}</Badge> },
  ];

  const memberColumns: DataTableColumn<AdminUser>[] = [
    { key: 'displayName', label: t('pages:iam.displayName'), render: (r) => <AvatarCell name={r.displayName} /> },
    { key: 'email', label: t('pages:iam.email'), render: (r) => r.email || '—' },
    { key: 'status', label: t('pages:organizations.status'), render: (r) => <Badge variant={statusToVariant(r.status)}>{r.status}</Badge> },
    { key: 'lastLoginAt', label: t('pages:iam.lastLogin'), render: (r) => r.lastLoginAt ? formatDate(r.lastLoginAt) : t('pages:iam.never') },
    {
      key: 'actions', label: '',
      render: (r) => (
        <Button variant="secondary" size="sm" onClick={(e) => { e.stopPropagation(); navigate(`/iam/users/${r.id}`); }}>
          {t('pages:iam.view')}
        </Button>
      ),
    },
  ];

  return (
    <Stack gap="md">
      <Stack direction="horizontal" gap="sm" align="center">
        <Button variant="secondary" size="sm" onClick={() => navigate(-1)}>
          ← {t('common:back')}
        </Button>
        <Breadcrumb items={[
          { label: t('pages:organizations.title'), to: '/iam/organizations' },
          { label: org.name },
        ]} />
      </Stack>

      <PageHeader
        title={org.name}
        subtitle={org.source === 'idp' ? `IdP-managed · Code: ${org.code}` : (org.description || `Code: ${org.code}`)}
        action={
          <Stack direction="horizontal" gap="sm">
            {canUpdate && !isEditing && (
              org.source === 'idp' ? (
                <Tooltip content={t('pages:organizations.editLockedIdP')}>
                  <Button variant="secondary" disabled>{t('pages:organizations.edit')}</Button>
                </Tooltip>
              ) : (
                <Button variant="secondary" onClick={startEditing}>{t('pages:organizations.edit')}</Button>
              )
            )}
            {canDelete && (
              <Button
                variant="danger"
                onClick={(e) => {
                  if (canDeleteOrg) { setDeleting(true); }
                  else {
                    const detail = [childCount > 0 ? t('pages:organizations.subOrgsCount', { count: childCount }) : '', projectCount > 0 ? t('pages:organizations.projectsCount', { count: projectCount }) : ''].filter(Boolean).join(t('pages:organizations.and'));
                    showTip(t('pages:organizations.cannotDeleteTip', { detail }), e);
                  }
                }}
                title={canDeleteOrg ? t('pages:organizations.deleteTitle') : t('pages:organizations.cannotDeleteTitle', { detail: `${childCount} sub-org(s) and ${projectCount} project(s)` })}
                className={canDeleteOrg ? undefined : styles.disabledDelete}
              >{t('pages:organizations.delete')}</Button>
            )}
          </Stack>
        }
      />

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">{t('pages:organizations.info')}</TabsTrigger>
          <TabsTrigger value="members">{t('pages:organizations.membersTab', { count: membersTotal })}</TabsTrigger>
          <TabsTrigger value="projects">{t('pages:organizations.projectsTab', { count: projectCount })}</TabsTrigger>
          <TabsTrigger value="children">{t('pages:organizations.subOrganizationsTab', { count: childCount })}</TabsTrigger>
        </TabsList>

        {/* Info Tab */}
        <TabsContent value="info">
          <Card>
            {isEditing ? (
              <Stack gap="md">
                <FormInput form={form} name="name" label={t('pages:organizations.name')} required />
                <FormInput form={form} name="code" label={t('pages:organizations.code')} required helpText={t('pages:organizations.codeHelpText')} />
                <FormField label={t('pages:organizations.parent')} helpText={t('pages:organizations.parentOrganizationHelpText')}>
                  <OrgTreeSelect
                    mode="single"
                    value={editParentId}
                    onChange={(v) => setEditParentId(Array.isArray(v) ? (v[0] ?? '') : v)}
                    allowClear
                    placeholder={t('pages:organizations.noneRoot')}
                    excludeIds={[id!]}
                  />
                </FormField>
                <FormInput form={form} name="description" label={t('pages:organizations.description')} />
                <FormInput form={form} name="contactName" label={t('pages:organizations.contactName')} />
                <FormInput form={form} name="contactEmail" label={t('pages:organizations.contactEmail')} type="email" />
                <FormInput form={form} name="contactPhone" label={t('pages:organizations.contactPhone')} />
                <FormInput form={form} name="timezone" label={t('pages:organizations.timezone')} helpText={t('pages:organizations.timezoneHelpText')} placeholder={t('pages:organizations.timezonePlaceholder')} />
                <FormSwitch form={form} name="enabled" label={t('pages:organizations.enabled')} helpText={t('pages:organizations.enabledHelpText')} />
                <Stack direction="horizontal" gap="sm" className={styles.actionsEnd}>
                  <Button variant="secondary" onClick={() => setIsEditing(false)}>{t('pages:organizations.cancel')}</Button>
                  <Button onClick={handleSave} disabled={saveLoading || !editName || !editCode}>
                    {saveLoading ? t('pages:organizations.saving') : t('pages:organizations.save')}
                  </Button>
                </Stack>
              </Stack>
            ) : (
              <>
                <h2 className={styles.widgetTitle}>{t('pages:organizations.organizationInformation')}</h2>
                <div className={styles.kvGrid}>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:organizations.name')}</div>
                    <div className={styles.kvValue}>{org.name}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabelRow}>
                      <span className={styles.kvLabel}>{t('pages:organizations.code')}</span>
                      <Tooltip content={t('pages:organizations.codeTooltip')}>
                        <span className={styles.tooltipHelpIcon}>?</span>
                      </Tooltip>
                    </div>
                    <div className={`${styles.kvValue} ${styles.monoCode}`}>{org.code}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabelRow}>
                      <span className={styles.kvLabel}>{t('pages:organizations.parent')}</span>
                      <Tooltip content={t('pages:organizations.parentTooltip')}>
                        <span className={styles.tooltipHelpIcon}>?</span>
                      </Tooltip>
                    </div>
                    <div className={styles.kvValue}>
                      {org.parent ? (
                        <Link to={`/iam/organizations/${org.parent.id}`} className={styles.linkStyle}>
                          {org.parent.name}
                        </Link>
                      ) : (
                        <span className={styles.textMuted}>{t('pages:organizations.noneRoot')}</span>
                      )}
                    </div>
                  </div>
                  <div>
                    <div className={styles.kvLabelRow}>
                      <span className={styles.kvLabel}>{t('pages:organizations.status')}</span>
                      <Tooltip content={t('pages:organizations.statusTooltip')}>
                        <span className={styles.tooltipHelpIcon}>?</span>
                      </Tooltip>
                    </div>
                    <div className={styles.badgeOffset}>
                      <Badge variant={statusToVariant(org.enabled ? 'active' : 'disabled')}>{org.enabled ? t('pages:organizations.active') : t('pages:organizations.disabled')}</Badge>
                    </div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:organizations.contactName')}</div>
                    <div className={styles.kvValue}>{org.contactName || '--'}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:organizations.contactEmail')}</div>
                    <div className={styles.kvValue}>{org.contactEmail || '--'}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:organizations.contactPhone')}</div>
                    <div className={styles.kvValue}>{org.contactPhone || '--'}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:organizations.timezone')}</div>
                    <div className={styles.kvValue}>{org.timezone || 'UTC'}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:organizations.created')}</div>
                    <div className={styles.kvValue}>
                      {formatDate(org.createdAt)}
                    </div>
                  </div>
                  {org.description && (
                    <div className={styles.gridFullSpan}>
                      <div className={styles.kvLabel}>{t('pages:organizations.description')}</div>
                      <div className={styles.kvValue}>{org.description}</div>
                    </div>
                  )}
                </div>
              </>
            )}
          </Card>
        </TabsContent>

        {/* Members Tab */}
        <TabsContent value="members">
          <Card>
            <Stack gap="md">
              <Stack direction="horizontal" gap="sm" align="center" style={{ justifyContent: 'space-between' }}>
                <h2 className={styles.widgetTitle} style={{ margin: 'var(--g-space-0)' }}>{t('pages:organizations.members')}</h2>
                <Stack direction="horizontal" gap="sm" align="center">
                  <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', fontSize: 'var(--g-font-size-sm)', cursor: 'pointer', userSelect: 'none' }}>
                    <Switch
                      checked={includeSubTeams}
                      onCheckedChange={(v) => { setIncludeSubTeams(v); setMembersOffset(0); }}
                    />
                    <span>{t('pages:organizations.includeSubTeams')}</span>
                  </label>
                  {canCreateUser && (
                    <Button size="sm" onClick={() => navigate(`/iam/users/new?orgId=${id}`)}>
                      {t('pages:organizations.addUser')}
                    </Button>
                  )}
                </Stack>
              </Stack>

              {membersLoading && <Skeleton.ListPageSkeleton />}
              {membersError && <ErrorBanner message={membersError.message} onRetry={refetchMembers} />}

              {!membersLoading && !membersError && (
                <>
                  {memberRows.length === 0 ? (
                    <div className={styles.emptySection}>{t('pages:organizations.noMembers')}</div>
                  ) : (
                    <DataTable<AdminUser>
                      hideSearch
                      onRowClick={(r) => navigate(`/iam/users/${r.id}`)}
                      columns={memberColumns}
                      data={memberRows}
                      emptyMessage={t('pages:organizations.noMembersShort')}
                    />
                  )}
                  <ListPagination
                    offset={membersOffset}
                    limit={membersLimit}
                    total={membersTotal}
                    onOffsetChange={setMembersOffset}
                    onLimitChange={(n) => { setMembersLimit(n); setMembersOffset(0); }}
                  />
                </>
              )}
            </Stack>
          </Card>
        </TabsContent>

        {/* Projects Tab */}
        <TabsContent value="projects">
          <Card>
            <h2 className={styles.widgetTitle}>{t('pages:organizations.projects')}</h2>
            {(org.projects ?? []).length === 0 ? (
              <div className={styles.emptySection}>
                {t('pages:organizations.noProjects')}
              </div>
            ) : (
              <DataTable<Project> hideSearch
                onRowClick={(row) => navigate(`/iam/projects/${row.id}`)}
                columns={projectColumns}
                data={org.projects ?? []}
                emptyMessage={t('pages:organizations.noProjectsShort')}
              />
            )}
          </Card>
        </TabsContent>

        {/* Sub-Organizations Tab */}
        <TabsContent value="children">
          <Card>
            <h2 className={styles.widgetTitle}>{t('pages:organizations.subOrganizations')}</h2>
            {(org.children ?? []).length === 0 ? (
              <div className={styles.emptySection}>
                {t('pages:organizations.noSubOrganizations')}
              </div>
            ) : (
              <DataTable<Organization> hideSearch
                onRowClick={(row) => navigate(`/iam/organizations/${row.id}`)}
                columns={childColumns}
                data={org.children ?? []}
                emptyMessage={t('pages:organizations.noSubOrganizationsShort')}
              />
            )}
          </Card>
        </TabsContent>
      </Tabs>

      {tip && (
        <div className={styles.tip} style={{ left: tip.x, top: tip.y }}>
          {tip.text}
        </div>
      )}

      <AlertDialog
        open={deleting}
        onOpenChange={(open) => { if (!open) setDeleting(false); }}
        title={t('pages:organizations.deleteOrganization')}
        description={t('pages:organizations.deleteConfirm', { name: org.name, code: org.code })}
        confirmLabel={t('pages:organizations.delete')}
        onConfirm={() => deleteOrg(undefined as never)}
        variant="danger"
      />
    </Stack>
  );
}
