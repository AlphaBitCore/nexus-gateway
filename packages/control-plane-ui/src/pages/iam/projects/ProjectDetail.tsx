import { useState, useCallback } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { projectApi } from '@/api/services';
import { useMutation } from '../../../hooks/useMutation';
import { usePermission } from '../../../hooks/usePermission';
import { useZodForm, FormInput, FormSelect, FormTextarea } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import { z } from 'zod';
import {
  PageHeader, Badge, statusToVariant, DataTable, AlertDialog, Breadcrumb,
  Skeleton, ErrorBanner, Tooltip,
  Button, Stack, Card, Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { Project, VirtualKey } from '../../../api/types';
import { formatDate } from '@/lib/format';
import styles from './ProjectDetail.module.css';

/* ── Schema ────────────────────────────────────────────────────────────── */

const projectEditSchema = z.object({
  name: z.string().min(1),
  code: z.string().min(1),
  description: z.string().optional().default(''),
  contactName: z.string().optional().default(''),
  contactEmail: z.string().optional().default(''),
  status: z.string().min(1),
});

type ProjectEditValues = z.infer<typeof projectEditSchema>;

/* ── Component ──────────────────────────────────────────────────────────── */

interface ProjectWithVKs extends Project {
  virtualKeys?: VirtualKey[];
}

export function ProjectDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [deleting, setDeleting] = useState(false);
  const [isEditing, setIsEditing] = useState(false);
  const [tip, setTip] = useState<{ text: string; x: number; y: number } | null>(null);

  const form = useZodForm<ProjectEditValues>({
    schema: projectEditSchema,
    defaultValues: {
      name: '', code: '', description: '', contactName: '',
      contactEmail: '', status: 'active',
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const canDelete = usePermission('project:delete');

  const showTip = useCallback((text: string, e: React.MouseEvent) => {
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    setTip({ text, x: rect.left, y: rect.top - 6 });
    setTimeout(() => setTip(null), 3000);
  }, []);

  const { data: project, loading, error, refetch } = useApi<ProjectWithVKs>(
    () => projectApi.get(id!),
    ['admin', 'projects', 'detail', id],
  );

  const { mutate: deleteProject } = useMutation(
    () => projectApi.delete(id!),
    {
      invalidateQueries: [['api', 'admin', 'projects']],
      onSuccess: () => navigate('/iam/projects'),
      successMessage: t('pages:projects.projectDeleted'),
    },
  );

  const { mutate: saveProject, loading: saving } = useMutation(
    (data: unknown) => projectApi.update(id!, data as Record<string, unknown>),
    {
      invalidateQueries: [['api', 'admin', 'projects']],
      onSuccess: () => { setIsEditing(false); },
      successMessage: t('pages:projects.projectUpdated'),
    },
  );

  const startEditing = () => {
    if (!project) return;
    form.reset({
      name: project.name,
      code: project.code,
      description: project.description ?? '',
      contactName: project.contactName ?? '',
      contactEmail: project.contactEmail ?? '',
      status: project.status,
    });
    setIsEditing(true);
  };

  const handleSave = () => {
    const values = form.getValues();
    saveProject({ name: values.name, code: values.code, description: values.description, contactName: values.contactName, contactEmail: values.contactEmail, status: values.status });
  };

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!project) return null;

  const virtualKeys = project.virtualKeys ?? [];

  const vkColumns: DataTableColumn<VirtualKey>[] = [
    { key: 'name', label: t('pages:projects.name') },
    { key: 'sourceApp', label: t('pages:projects.sourceApp'), render: (r) => r.sourceApp ?? '--' },
    { key: 'rateLimitRpm', label: t('pages:projects.rpm'), render: (r) => r.rateLimitRpm != null ? String(r.rateLimitRpm) : '--' },
    { key: 'enabled', label: t('pages:projects.status'), render: (r) => <Badge variant={statusToVariant(r.enabled ? 'enabled' : 'disabled')}>{r.enabled ? t('common:enabled') : t('common:disabled')}</Badge> },
  ];

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:projects.title'), to: '/iam/projects' },
        { label: project.name },
      ]} />

      <PageHeader
        title={project.name}
        subtitle={project.description || `Code: ${project.code}`}
        action={
          <Stack direction="horizontal" gap="sm">
            {!isEditing && (
              <Button variant="secondary" onClick={startEditing}>{t('pages:projects.edit')}</Button>
            )}
            {isEditing && (
              <>
                <Button variant="secondary" onClick={() => setIsEditing(false)}>{t('pages:projects.cancel')}</Button>
                <Button onClick={handleSave} disabled={saving}>
                  {saving ? t('pages:projects.saving') : t('pages:projects.save')}
                </Button>
              </>
            )}
            {canDelete ? (() => {
              const vkCount = project._count?.virtualKeys ?? virtualKeys.length;
              const canDel = vkCount === 0;
              return (
                <Button
                  variant="danger"
                  onClick={(e) => {
                    if (canDel) { setDeleting(true); }
                    else { showTip(t('pages:projects.cannotDeleteTip', { count: vkCount }), e); }
                  }}
                  title={canDel ? t('pages:projects.deleteTitle') : t('pages:projects.cannotDeleteTitle', { count: vkCount })}
                  className={canDel ? undefined : styles.disabledDelete}
                >{t('pages:projects.delete')}</Button>
              );
            })() : null}
          </Stack>
        }
      />

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">{t('pages:projects.info')}</TabsTrigger>
          <TabsTrigger value="virtualKeys">{t('pages:projects.virtualKeysTab', { count: project._count?.virtualKeys ?? virtualKeys.length })}</TabsTrigger>
        </TabsList>

        {/* Info Tab — Read */}
        <TabsContent value="info">
          {!isEditing && (
            <Card>
              <h2 className={styles.widgetTitle}>{t('pages:projects.projectInformation')}</h2>
              <div className={styles.kvGrid}>
                <div>
                  <div className={styles.kvLabel}>{t('pages:projects.name')}</div>
                  <div className={styles.kvValue}>{project.name}</div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:projects.code')}</span>
                    <Tooltip content={t('pages:projects.codeTooltip')}>
                      <span className={styles.tooltipHelpIcon}>?</span>
                    </Tooltip>
                  </div>
                  <div className={`${styles.kvValue} ${styles.monoCode}`}>{project.code}</div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:projects.organization')}</span>
                    <Tooltip content={t('pages:projects.organizationTooltip')}>
                      <span className={styles.tooltipHelpIcon}>?</span>
                    </Tooltip>
                  </div>
                  <div className={styles.kvValue}>
                    {project.organization ? (
                      <Link to={`/iam/organizations/${project.organization?.id}`} className={styles.linkStyle}>
                        {project.organization?.name}
                      </Link>
                    ) : (
                      <span className={styles.textMuted}>--</span>
                    )}
                  </div>
                </div>
                <div>
                  <div className={styles.kvLabelRow}>
                    <span className={styles.kvLabel}>{t('pages:projects.status')}</span>
                    <Tooltip content={t('pages:projects.statusTooltip')}>
                      <span className={styles.tooltipHelpIcon}>?</span>
                    </Tooltip>
                  </div>
                  <div className={styles.badgeOffset}>
                    <Badge variant={statusToVariant(project.status)}>{project.status}</Badge>
                  </div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:projects.contactName')}</div>
                  <div className={styles.kvValue}>{project.contactName || '--'}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:projects.contactEmail')}</div>
                  <div className={styles.kvValue}>{project.contactEmail || '--'}</div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:projects.created')}</div>
                  <div className={styles.kvValue}>
                    {formatDate(project.createdAt)}
                  </div>
                </div>
                {project.description && (
                  <div className={styles.gridFullSpan}>
                    <div className={styles.kvLabel}>{t('pages:projects.description')}</div>
                    <div className={styles.kvValue}>{project.description}</div>
                  </div>
                )}
              </div>
            </Card>
          )}

          {/* Info Tab — Edit Mode */}
          {isEditing && (
            <Card>
              <h2 className={styles.widgetTitle}>{t('pages:projects.editProject')}</h2>
              <div className={styles.editGrid}>
                <FormInput form={form} name="name" label={t('pages:projects.name')} required />
                <FormInput form={form} name="code" label={t('pages:projects.code')} required helpText={t('pages:projects.codeHelpText')} />
                <FormSelect form={form} name="status" label={t('pages:projects.status')} helpText={t('pages:projects.statusHelpText')}
                  options={[
                    { value: 'active', label: t('pages:projects.active') },
                    { value: 'archived', label: t('pages:projects.archived') },
                  ]}
                />
                <FormInput form={form} name="contactName" label={t('pages:projects.contactName')} />
                <FormInput form={form} name="contactEmail" label={t('pages:projects.contactEmail')} />
                <div className={styles.gridFullSpan}>
                  <FormTextarea form={form} name="description" label={t('pages:projects.description')} rows={3} />
                </div>
              </div>
            </Card>
          )}
        </TabsContent>

        {/* Virtual Keys Tab */}
        <TabsContent value="virtualKeys">
          <Card>
            <h2 className={styles.widgetTitle}>{t('pages:projects.virtualKeys')}</h2>
            {virtualKeys.length === 0 ? (
              <div className={styles.emptySection}>
                {t('pages:projects.noVirtualKeys')}
              </div>
            ) : (
              <DataTable<VirtualKey> hideSearch
                columns={vkColumns}
                data={virtualKeys}
                emptyMessage={t('pages:projects.noVirtualKeysShort')}
              />
            )}
          </Card>
        </TabsContent>
      </Tabs>

      <AlertDialog
        open={deleting}
        onOpenChange={(open) => { if (!open) setDeleting(false); }}
        title={t('pages:projects.deleteProject')}
        description={t('pages:projects.deleteConfirm', { name: project.name, code: project.code })}
        confirmLabel={t('pages:projects.delete')}
        onConfirm={() => deleteProject(undefined as never)}
        variant="danger"
      />

      {tip && (
        <div className={styles.tip} style={{ left: tip.x, top: tip.y }}>
          {tip.text}
        </div>
      )}
    </Stack>
  );
}
