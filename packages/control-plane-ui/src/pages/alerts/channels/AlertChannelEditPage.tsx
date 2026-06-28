/**
 * AlertChannelEditPage — create / edit a unified alert channel.
 *
 * Layout:
 *   - Breadcrumb + PageHeader (title swaps between "Create channel" and
 *     "Edit channel: <name>")
 *   - Card 1: common fields (name, type, enabled, severities, sourceTypes)
 *   - Card 2: per-type config panel (webhook | slack | email | pagerduty)
 *   - Footer: Save / Cancel
 *
 * Masked secrets:
 *   Hub redacts sensitive config values on GET with the literal prefix
 *   `xxxx-••••-<last4>`. On PUT, Hub's `mergeMaskedSecrets` restores any
 *   value the UI forwards verbatim. The UI therefore:
 *     - Renders masked fields read-only with a "Change" button.
 *     - Clicking "Change" clears the input and lets the user type a new
 *       secret. Saving sends the new value; Hub stores it plainly.
 *     - If the user never touches the field, the masked token is PUT back
 *       as-is and Hub substitutes the original.
 *
 * Route params: `/alerts/channels/:id` where `id === 'new'` means create.
 */
import { useCallback, useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi } from '@/api/services';
import type { AlertChannel, AlertSeverity } from '@/api/services';
import {
  AlertDialog,
  Button,
  Stack,
  Card,
  Skeleton,
  ErrorBanner,
  Switch,
  Select,
  Input,
  FormField,
  MultiSelectDropdown,
} from '@/components/ui';
import { CHANNEL_TYPES, SEVERITIES, SOURCE_TYPES } from './channelMasking';
import { useChannelForm } from './useChannelForm';
import { AlertChannelConfigPanel } from './AlertChannelEditPage.ConfigPanel';
import styles from './AlertChannelEditPage.module.css';

export { MASK_PREFIX } from './channelMasking';

export function AlertChannelEditPage() {
  const { id: rawId } = useParams<{ id: string }>();
  const id = rawId ?? '';
  const isNew = id === 'new' || id === '';
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [deleteOpen, setDeleteOpen] = useState(false);

  const { data: channel, loading, error, refetch } = useApi<AlertChannel>(
    () => alertsApi.getChannel(id),
    ['admin', 'alerts', 'channels', 'detail', id],
    { skip: isNew },
  );

  const form = useChannelForm(channel, isNew);
  const {
    name,
    setName,
    type,
    setType,
    enabled,
    setEnabled,
    severities,
    setSeverities,
    sourceTypes,
    setSourceTypes,
    buildConfig,
  } = form;

  /* ── Mutations ─────────────────────────────────────────────────────────── */
  const { mutate: saveChannel, loading: saving } = useMutation<void, AlertChannel>(
    () => {
      const body = {
        name: name.trim(),
        type,
        enabled,
        severities,
        sourceTypes,
        config: buildConfig(),
      };
      if (isNew) return alertsApi.createChannel(body);
      return alertsApi.updateChannel(id, body);
    },
    {
      onSuccess: (saved) => {
        if (isNew) {
          navigate(`/alerts/channels/${encodeURIComponent(saved.id)}`);
        } else {
          refetch();
        }
      },
      successMessage: isNew
        ? t('pages:alerts.channels.edit.createSuccess')
        : t('pages:alerts.channels.edit.saveSuccess'),
    },
  );

  const onSave = useCallback(() => {
    void saveChannel();
  }, [saveChannel]);
  const onCancel = useCallback(() => navigate('/alerts/channels'), [navigate]);

  const { mutate: deleteChannel, loading: deleting } = useMutation<void, void>(
    () => alertsApi.deleteChannel(id),
    {
      onSuccess: () => {
        setDeleteOpen(false);
        navigate('/alerts/channels');
      },
      successMessage: t('pages:alerts.channels.deleteSuccess'),
    },
  );

  const onDeleteConfirm = useCallback(() => {
    void deleteChannel();
  }, [deleteChannel]);

  /* ── Options for selects ───────────────────────────────────────────────── */
  const typeOptions = useMemo(
    () =>
      CHANNEL_TYPES.map((v) => ({
        value: v,
        label: t(`pages:alerts.channels.types.${v}`),
      })),
    [t],
  );

  const severityOptions = useMemo(
    () =>
      SEVERITIES.map((v) => ({
        value: v,
        label: t(`pages:alerts.channels.severities.${v}`),
      })),
    [t],
  );

  const sourceTypeOptions = useMemo(
    () => SOURCE_TYPES.map((v) => ({ value: v, label: v })),
    [],
  );

  if (!isNew && loading && !channel) return <Skeleton.DetailPageSkeleton />;
  if (!isNew && error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const title = isNew
    ? t('pages:alerts.channels.edit.createTitle')
    : t('pages:alerts.channels.edit.editTitle', { name: channel?.name ?? '' });

  return (
    <Stack gap="md">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link to="/alerts/channels" className={styles.backLink} aria-label={t('common:back', 'Back')}>
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.detailTitle}>{title}</h1>
            <p className={styles.detailSubtitle}>{t('pages:alerts.channels.edit.subtitle')}</p>
          </div>
          {!isNew && (
            <Button variant="danger" className={styles.headerDeleteButton} onClick={() => setDeleteOpen(true)}>
              {t('common:delete')}
            </Button>
          )}
        </div>
      </section>

      {/* Common fields */}
      <section className={styles.contentSection}>
        <h3 className={styles.sectionTitle}>
          {t('pages:alerts.channels.edit.generalSection')}
        </h3>
        <Card>
          <div className={styles.generalGrid}>
            <FormField label={t('pages:alerts.channels.edit.nameLabel')}>
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t('pages:alerts.channels.edit.namePlaceholder')}
              />
            </FormField>
            <FormField label={t('pages:alerts.channels.edit.typeLabel')}>
              <Select
                value={type}
                onValueChange={(v) => setType(v as AlertChannel['type'])}
                options={typeOptions}
              />
            </FormField>
            <div className={styles.switchRow}>
              <label>{t('pages:alerts.channels.edit.enabledLabel')}</label>
              <Switch checked={enabled} onCheckedChange={setEnabled} />
            </div>
            <MultiSelectDropdown
              className={styles.fullWidthField}
              label={t('pages:alerts.channels.edit.severitiesLabel')}
              emptyLabel={t('pages:alerts.channels.edit.severitiesAll')}
              options={severityOptions}
              value={severities}
              onChange={(next) => setSeverities(next as AlertSeverity[])}
            />
            <MultiSelectDropdown
              className={styles.fullWidthField}
              label={t('pages:alerts.channels.edit.sourceTypesLabel')}
              emptyLabel={t('pages:alerts.channels.edit.sourceTypesAll')}
              options={sourceTypeOptions}
              value={sourceTypes}
              onChange={setSourceTypes}
            />
            <p className={styles.hint}>
              {t('pages:alerts.channels.edit.sourceTypesHelp')}
            </p>
          </div>
        </Card>
      </section>

      {/* Per-type config panel */}
      <AlertChannelConfigPanel form={form} />

      {/* Footer */}
      <Stack direction="horizontal" gap="sm" className={styles.footerActions}>
        <Button className={styles.footerButton} variant="secondary" onClick={onCancel}>
          {t('common:cancel')}
        </Button>
        <Button className={styles.footerButton} onClick={onSave} disabled={saving} loading={saving}>
          {t('common:save')}
        </Button>
      </Stack>
      <AlertDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={t('pages:alerts.channels.deleteChannel', 'Delete channel')}
        description={t(
          'pages:alerts.channels.deleteConfirm',
          'Are you sure you want to delete channel "{{name}}"? This action cannot be undone.',
          { name: channel?.name ?? name },
        )}
        confirmLabel={t('common:delete')}
        cancelLabel={t('common:cancel')}
        onConfirm={onDeleteConfirm}
        loading={deleting}
        variant="danger"
      />
    </Stack>
  );
}
