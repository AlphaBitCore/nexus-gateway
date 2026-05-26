/**
 * AlertRuleEditPage — edit one alert rule (enabled / severity / cooldown /
 * requiresAck / params).
 *
 * Layout:
 *   - PageHeader "Edit Rule: <displayName>" + breadcrumb back to /alerts/rules
 *   - Card 1: rule metadata (ruleId, sourceType, updatedAt)
 *   - Card 2: top-level fields
 *       * Enabled toggle
 *       * Default Severity (Select)
 *       * Cooldown seconds (Input[number])
 *       * Requires acknowledgement (Switch)
 *   - Card 3: rule-specific params — delegated to `getRuleEditor(ruleId)`
 *     from the ruleEditors registry, which passes `params` + `paramsSchema`
 *     + `onChange` to the matched editor
 *   - Footer: Save / Cancel / Reset buttons. Reset shows an AlertDialog
 *     confirmation before calling `alertsApi.resetRule(id)`.
 *
 * Save dispatches `alertsApi.updateRule(id, { enabled, params, cooldownSec,
 * requiresAck, defaultSeverity })`. Hub validates `params` against the
 * stored `paramsSchema` and rejects with 400 on mismatch; the mutation's
 * error toast (via useMutation) surfaces the reason.
 *
 * Severity casing: Hub normalises via `strings.ToLower`, so we only ever
 * send lowercase values.
 */
import { useCallback, useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi, deviceGroupsApi } from '@/api/services';
import type { AlertRule, AlertSeverity } from '@/api/services';
import {
  PageHeader,
  Breadcrumb,
  Button,
  Stack,
  Card,
  Skeleton,
  ErrorBanner,
  Switch,
  Select,
  Input,
  FormField,
  AlertDialog,
} from '@/components/ui';
import { getRuleEditor } from '../ruleEditors';
import styles from './AlertRuleEditPage.module.css';

const SEVERITIES: AlertSeverity[] = ['critical', 'high', 'medium', 'low', 'info'];

export function AlertRuleEditPage() {
  const { id: rawId } = useParams<{ id: string }>();
  const id = rawId ?? '';
  const { t } = useTranslation();
  const navigate = useNavigate();

  const { data: rule, loading, error, refetch } = useApi<AlertRule>(
    () => alertsApi.getRule(id),
    ['admin', 'alerts', 'rules', 'detail', id],
    { skip: !id },
  );

  // Mutable form state derived from the fetched rule.
  const [enabled, setEnabled] = useState(false);
  const [defaultSeverity, setDefaultSeverity] = useState<AlertSeverity>('medium');
  const [cooldownSec, setCooldownSec] = useState(0);
  const [requiresAck, setRequiresAck] = useState(false);
  const [params, setParams] = useState<Record<string, unknown>>({});
  // Optional per-group filter. Empty string = fleet-wide
  // (clears the filter on save); non-empty = bind to that group.
  const [groupIdFilter, setGroupIdFilter] = useState<string>('');
  const [resetOpen, setResetOpen] = useState(false);

  // List groups for the dropdown — kept independent of rule fetch so
  // the picker is still usable for a freshly-loaded rule with no
  // filter. Page size is fleet-scale (50) which is plenty until we
  // hit organisations with hundreds of groups.
  const { data: groupList } = useApi(
    () => deviceGroupsApi.list({ limit: '50' }),
    ['admin', 'device-groups', 'list', 'rule-filter-picker'],
  );

  // Seed form state from the fetched rule. Re-runs after refetch / reset.
  useEffect(() => {
    if (!rule) return;
    setEnabled(rule.enabled);
    setDefaultSeverity(rule.defaultSeverity);
    setCooldownSec(rule.cooldownSec);
    setRequiresAck(rule.requiresAck);
    setParams(rule.params ?? {});
    setGroupIdFilter(rule.groupIdFilter ?? '');
  }, [rule]);

  const { mutate: saveRule, loading: saving } = useMutation<void, AlertRule>(
    () =>
      alertsApi.updateRule(id, {
        enabled,
        defaultSeverity,
        cooldownSec,
        requiresAck,
        params,
        // Empty string clears the filter (handler interprets "" as
        // "rule fires fleet-wide"); non-empty binds to that group.
        // Sent unconditionally so the save round-trip is idempotent.
        groupIdFilter,
      }),
    {
      onSuccess: () => {
        refetch();
        navigate('/alerts/rules');
      },
      successMessage: t('pages:alerts.rules.edit.saveSuccess'),
    },
  );

  const { mutate: doReset, loading: resetting } = useMutation<void, AlertRule>(
    () => alertsApi.resetRule(id),
    {
      onSuccess: () => {
        setResetOpen(false);
        refetch();
      },
      successMessage: t('pages:alerts.rules.edit.resetSuccess'),
    },
  );

  const onSave = useCallback(() => {
    void saveRule();
  }, [saveRule]);
  const onCancel = useCallback(() => navigate('/alerts/rules'), [navigate]);
  const onResetConfirm = useCallback(() => {
    void doReset();
  }, [doReset]);

  if (!id) return <ErrorBanner message={t('pages:alerts.rules.edit.missingId')} />;
  if (loading && !rule) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!rule) return null;

  const Editor = getRuleEditor(rule.id);

  const severityOptions = SEVERITIES.map((s) => ({
    value: s,
    label: t(`pages:alerts.rules.severities.${s}`),
  }));

  return (
    <Stack gap="md">
      <Breadcrumb
        items={[
          { label: t('pages:alerts.rules.title'), to: '/alerts/rules' },
          { label: rule.displayName },
        ]}
      />

      <PageHeader
        title={t('pages:alerts.rules.edit.title', { name: rule.displayName })}
        subtitle={t('pages:alerts.rules.edit.subtitle')}
      />

      {/* Metadata (read-only) */}
      <Card>
        <h3 className={styles.sectionTitle}>
          {t('pages:alerts.rules.edit.metadataSection')}
        </h3>
        <dl className={styles.metaGrid}>
          <dt>{t('pages:alerts.rules.columns.ruleId')}</dt>
          <dd>
            <code className={styles.inlineCode}>{rule.id}</code>
          </dd>
          <dt>{t('pages:alerts.rules.columns.sourceType')}</dt>
          <dd>{rule.sourceType}</dd>
          <dt>{t('pages:alerts.rules.edit.updatedAt')}</dt>
          <dd>{new Date(rule.updatedAt).toLocaleString()}</dd>
        </dl>
      </Card>

      {/* Top-level knobs */}
      <Card>
        <h3 className={styles.sectionTitle}>
          {t('pages:alerts.rules.edit.generalSection')}
        </h3>
        <Stack gap="md">
          <div className={styles.switchRow}>
            <label>{t('pages:alerts.rules.edit.enabledLabel')}</label>
            <Switch checked={enabled} onCheckedChange={setEnabled} />
          </div>
          <FormField label={t('pages:alerts.rules.edit.severityLabel')}>
            <Select
              value={defaultSeverity}
              onValueChange={(v) => setDefaultSeverity(v as AlertSeverity)}
              options={severityOptions}
            />
          </FormField>
          <FormField
            label={t('pages:alerts.rules.edit.cooldownLabel')}
            helpText={t('pages:alerts.rules.edit.cooldownHelp')}
          >
            <Input
              type="number"
              min={0}
              step={60}
              value={String(cooldownSec)}
              onChange={(e) => setCooldownSec(Number(e.target.value) || 0)}
            />
          </FormField>
          <div className={styles.switchRow}>
            <label>{t('pages:alerts.rules.edit.requiresAckLabel')}</label>
            <Switch checked={requiresAck} onCheckedChange={setRequiresAck} />
          </div>
          {/* Per-group filter. NULL/empty = fleet-wide; non-empty
              binds the rule to that DeviceGroup so the Raiser drops
              firings whose target isn't a member. */}
          <FormField
            label={t('pages:alerts.rules.edit.groupFilterLabel', 'Restrict to device group')}
            helpText={t(
              'pages:alerts.rules.edit.groupFilterHelp',
              'When set, this rule only fires for events whose target device is a member of the selected group. Leave as "Fleet-wide" for the default behaviour.',
            )}
          >
            <select
              value={groupIdFilter}
              onChange={(e) => setGroupIdFilter(e.target.value)}
              style={{
                padding: 'var(--g-space-2)',
                borderRadius: 'var(--g-radius-sm)',
                border: '1px solid var(--color-border)',
                width: '100%',
                maxWidth: 360,
              }}
            >
              <option value="">{t('pages:alerts.rules.edit.groupFilterFleetWide', 'Fleet-wide (no filter)')}</option>
              {(groupList?.data ?? []).map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </select>
          </FormField>
        </Stack>
      </Card>

      {/* Rule-specific params form */}
      <Card>
        <h3 className={styles.sectionTitle}>
          {t('pages:alerts.rules.edit.paramsSection')}
        </h3>
        <Editor value={params} schema={rule.paramsSchema ?? {}} onChange={setParams} />
      </Card>

      {/* Footer */}
      <Stack direction="horizontal" gap="sm" className={styles.footerActions}>
        <Button variant="secondary" onClick={onCancel}>
          {t('common:cancel')}
        </Button>
        <Button variant="secondary" onClick={() => setResetOpen(true)} disabled={resetting}>
          {t('pages:alerts.rules.edit.reset')}
        </Button>
        <Button onClick={onSave} disabled={saving} loading={saving}>
          {t('common:save')}
        </Button>
      </Stack>

      <AlertDialog
        open={resetOpen}
        onOpenChange={setResetOpen}
        title={t('pages:alerts.rules.edit.resetConfirmTitle')}
        description={t('pages:alerts.rules.edit.resetConfirmBody')}
        confirmLabel={t('pages:alerts.rules.edit.reset')}
        cancelLabel={t('common:cancel')}
        onConfirm={onResetConfirm}
        loading={resetting}
      />
    </Stack>
  );
}
