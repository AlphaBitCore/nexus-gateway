import { useState, useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useToast } from '@/context/ToastContext';
import {
  Dialog, Button, Switch, Tooltip, Stack,
  FormField,
  MultiSelectDropdown,
} from '@/components/ui';
import { useZodForm, FormInput, FormSelect } from '@/lib/forms';
import { useMutation } from '@/hooks/useMutation';
import { useSyncFeedback } from '@/hooks/useSyncFeedback';
import { serviceUrlsApi, hookApi } from '@/api/services';
import type { HookWritePayload } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import type { HookConfig } from '@/api/types';
import {
  HOOK_APPLICABLE_INGRESS,
  HOOK_CATEGORY_AUTO_VALUE,
  HOOK_FAIL_BEHAVIOR,
  HOOK_FORM_DEFAULTS,
  HOOK_ROW_TYPE,
  HOOK_SELECT_CATEGORY_OPTIONS,
  HOOK_SELECT_FAIL_OPTIONS,
  HOOK_SELECT_STAGE_OPTIONS,
  HOOK_SELECT_TYPE_OPTIONS,
  HOOK_STAGE,
  HOOK_UI_EMPTY,
} from '@/constants/hooks';
import { aiguardComplianceWebhookUrl } from '@/lib/aiguardWebhook';
import styles from './HookForm.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';
import {
  CATEGORY_AUTO_SENTINEL,
  toCategorySentinel,
  fromCategorySentinel,
  normalizeApplicableIngress,
  hookFormSchema,
  type HookFormValues,
  type WebhookTargetOption,
} from './hookFormModel';
import { useHookRegistry } from './useHookRegistry';
import { useHookConfigState } from './useHookConfigState';
import { ConfigSection } from './ConfigSection';

interface HookFormProps {
  hook?: HookConfig;
  onClose: () => void;
  onSaved: () => void;
  embedded?: boolean;
  onCreateSuccess?: (created: HookConfig) => void;
}

export function HookForm({ hook, onClose, onSaved, embedded, onCreateSuccess }: HookFormProps) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const showSyncFeedback = useSyncFeedback();

  const form = useZodForm<HookFormValues>({
    schema: hookFormSchema,
    defaultValues: {
      name: hook?.name ?? '',
      type: hook?.type ?? HOOK_ROW_TYPE.BUILTIN,
      stage: hook?.stage ?? HOOK_STAGE.REQUEST,
      priority: hook?.priority ?? HOOK_FORM_DEFAULTS.PRIORITY,
      timeoutMs: hook?.timeoutMs ?? HOOK_FORM_DEFAULTS.TIMEOUT_MS,
      failBehavior: hook?.failBehavior ?? HOOK_FAIL_BEHAVIOR.FAIL_OPEN,
      enabled: hook?.enabled ?? true,
      category: toCategorySentinel(hook?.category ?? HOOK_CATEGORY_AUTO_VALUE),
      whEndpoint: hook?.endpoint ?? '',
    },
  });

  const type = form.watch('type');
  const stage = form.watch('stage');
  const name = form.watch('name');
  const enabled = form.watch('enabled');
  const whEndpoint = form.watch('whEndpoint');
  // Resolve the AI Gateway publicURL so the AIGuard preset writes the
  // gateway-reported externally-reachable webhook URL instead of the
  // legacy window-based localhost:3050 guess. Initial preset detection
  // (below) runs before this resolves — that case still falls back to
  // the window heuristic and is corrected by the effect at L213 once
  // the API responds.
  const { data: serviceURLs } = useApi(
    () => serviceUrlsApi.publicURLs(),
    ['admin', 'services', 'public-urls'],
  );
  const aiguardWebhookUrl = aiguardComplianceWebhookUrl(serviceURLs?.aiGateway);
  const [webhookTargetOption, setWebhookTargetOption] = useState<WebhookTargetOption>(() => {
    const endpoint = hook?.endpoint?.trim();
    if (endpoint && endpoint === aiguardComplianceWebhookUrl()) return 'aiguard';
    return 'custom';
  });

  const { registryError, implementations, hookCategories } = useHookRegistry();

  const {
    selectedImplementationId,
    configObject,
    setConfigObject,
    manualConfigJson,
    setManualConfigJson,
    useManualConfigEditor,
    setUseManualConfigEditor,
    filteredImplementations,
    selectedMeta,
    handleImplementationChange,
    buildConfigPayload,
  } = useHookConfigState({ hook, implementations, type, stage, addToast });

  // applicableIngress: empty selection submits `undefined` (server keeps the
  // existing value / falls through to the `["ALL"]` default). The API rejects
  // an explicit empty array, so we never send one.
  const [applicableIngress, setApplicableIngress] = useState<string[]>(
    () => normalizeApplicableIngress(hook?.applicableIngress ?? [HOOK_APPLICABLE_INGRESS.ALL]),
  );

  useEffect(() => {
    setApplicableIngress((prev) =>
      normalizeApplicableIngress(
        hook?.applicableIngress ?? [HOOK_APPLICABLE_INGRESS.ALL],
        prev,
      ),
    );
  }, [hook]);

  const categorySelectOptions = useMemo(() => {
    if (hookCategories.length > 0) {
      return [
        { value: CATEGORY_AUTO_SENTINEL, label: t('pages:hooks.categoryAutoLabel', 'Auto (built-in registry default)') },
        ...hookCategories.map((c) => ({ value: c.code || c.code, label: c.name })),
      ];
    }
    // Map the default options to use sentinel for the empty-string auto value
    return HOOK_SELECT_CATEGORY_OPTIONS.map((o) => ({
      ...o,
      value: toCategorySentinel(o.value),
    }));
  }, [hookCategories]);

  const applicableIngressOptions = useMemo(
    () => ([
      { value: HOOK_APPLICABLE_INGRESS.ALL, label: t('pages:hooks.ingressCode.ALL', 'All ingress types') },
      { value: HOOK_APPLICABLE_INGRESS.AI_GATEWAY, label: t('pages:hooks.ingressCode.AI_GATEWAY', 'AI Gateway') },
      { value: HOOK_APPLICABLE_INGRESS.COMPLIANCE_PROXY, label: t('pages:hooks.ingressCode.COMPLIANCE_PROXY', 'Compliance Proxy') },
      { value: HOOK_APPLICABLE_INGRESS.AGENT, label: t('pages:hooks.ingressCode.AGENT', 'Agent') },
    ]),
    [t],
  );

  useEffect(() => {
    if (type !== HOOK_ROW_TYPE.WEBHOOK) return;
    if (whEndpoint.trim() === aiguardWebhookUrl) {
      if (webhookTargetOption !== 'aiguard') setWebhookTargetOption('aiguard');
      return;
    }
    if (webhookTargetOption !== 'custom') setWebhookTargetOption('custom');
  }, [type, whEndpoint, webhookTargetOption, aiguardWebhookUrl]);

  const { mutate, loading } = useMutation(
    (data: HookWritePayload) =>
      hook ? hookApi.update(hook.id, data) : hookApi.create(data),
    {
      invalidateQueries: [['api', 'admin', 'hooks']],
      onSuccess: (result) => {
        showSyncFeedback('ai-gateway');
        if (!hook && result?.id && onCreateSuccess) {
          onCreateSuccess(result);
          onSaved();
          if (!embedded) onClose();
          return;
        }
        onSaved();
        onClose();
      },
      successMessage: hook ? t('pages:hooks.hookUpdated') : t('pages:hooks.hookCreated'),
    },
  );

  const onSubmit = (values: HookFormValues) => {
    const parsedConfig = buildConfigPayload();
    if (!parsedConfig) return;

    const payload: HookWritePayload = {
      name: values.name,
      type: values.type,
      stage: values.stage,
      implementationId: selectedImplementationId,
      priority: values.priority,
      timeoutMs: values.timeoutMs,
      failBehavior: values.failBehavior,
      enabled: values.enabled,
      config: parsedConfig,
    };
    const resolvedCategory = fromCategorySentinel(values.category);
    if (hook) {
      payload.category = resolvedCategory === HOOK_CATEGORY_AUTO_VALUE ? null : resolvedCategory;
    } else if (resolvedCategory !== HOOK_CATEGORY_AUTO_VALUE) {
      payload.category = resolvedCategory;
    }
    if (values.type === HOOK_ROW_TYPE.WEBHOOK) {
      payload.endpoint = values.whEndpoint;
    }
    if (applicableIngress.length > 0) {
      payload.applicableIngress = applicableIngress;
    }
    mutate(payload);
  };

  const implSelectOptions = filteredImplementations.map((i) => ({
    value: i.implementationId,
    label: `${i.implementationId} (${i.hookType})`,
  }));

  const schema = selectedMeta?.configSchema as Record<string, unknown> | undefined;

  const footerButtons = (
    <Stack direction="horizontal" gap="sm">
      <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
      <Button
        onClick={form.handleSubmit(onSubmit)}
        disabled={
          loading ||
          !name ||
          !selectedImplementationId ||
          applicableIngress.length === 0 ||
          (type === HOOK_ROW_TYPE.WEBHOOK && !whEndpoint.trim())
        }
      >
        {loading ? t('pages:hooks.saving', 'Saving...') : t('common:save')}
      </Button>
    </Stack>
  );

  const formInner = (
    <Stack gap="md">
      <div className={styles.basicFieldsGrid}>
        <FormInput form={form} name="name" label={t('pages:hooks.nameLabel')} required />

        <FormSelect form={form} name="type" label={t('pages:hooks.formTypeLabel')} required helpText={t('pages:hooks.formTypeHelp')} options={HOOK_SELECT_TYPE_OPTIONS} />

        <FormSelect form={form} name="stage" label={t('pages:hooks.formStageLabel')} required helpText={t('pages:hooks.formStageHelp')} options={HOOK_SELECT_STAGE_OPTIONS} />
      </div>

      {hook?.classification && (
        <div className={styles.classificationBox}>
          <div className={styles.classificationTitle}>{t('pages:hooks.classificationTitle')}</div>
          <div className={styles.classificationGrid}>
            <div>
              <strong>{t('pages:hooks.effectiveCategory')}</strong> {hook.classification.categoryLabel} ({hook.classification.category})
              <span className={styles.categorySourceHint}>
                {' '}({t('pages:hooks.categorySource', { source: hook.classification.categorySource })})
              </span>
            </div>
            <div>
              <strong>{t('pages:hooks.phaseThisRow')}</strong> {hook.classification.phaseLabel}
            </div>
            <div>
              <strong>{t('pages:hooks.implementationClassLabel')}</strong> {hook.classification.implementationLabel ?? hook.classification.implementationId ?? HOOK_UI_EMPTY}
            </div>
          </div>
          {hook.classification.logicalHookType ? (
            <div>
              <strong>{t('pages:hooks.logicalType')}</strong> {hook.classification.logicalHookType}
            </div>
          ) : null}
          <div className={styles.classificationNote}>
            {t('pages:hooks.supportedStagesNote', { stages: hook.classification.supportedStages.join(', ') })}
            {hook.classification.dualPhaseCapable
              ? t('pages:hooks.dualPhaseNote')
              : null}
          </div>
        </div>
      )}

      <div className={styles.basicFieldsGrid}>
        <FormSelect form={form} name="category" label={t('pages:hooks.categoryOverrideLabel')} helpText={t('pages:hooks.categoryOverrideHint')} options={categorySelectOptions} />

        <FormInput form={form} name="priority" label={t('pages:hooks.formPriorityLabel')} helpText={t('pages:hooks.formPriorityHelp')} type="number" />

        <FormInput form={form} name="timeoutMs" label={t('pages:hooks.formTimeoutLabel')} helpText={t('pages:hooks.formTimeoutHelp')} type="number" />
      </div>

      <div className={styles.basicFieldsGrid}>
        <FormSelect form={form} name="failBehavior" label={t('pages:hooks.formFailBehaviorLabel')} helpText={t('pages:hooks.formFailBehaviorHelp')} options={HOOK_SELECT_FAIL_OPTIONS} />

        <div className={styles.switchField}>
          <Stack direction="horizontal" gap="sm" className={styles.enabledRow}>
            <label className={styles.enabledLabel}>{t('pages:hooks.formEnabledLabel')}</label>
            <Tooltip content={t('pages:hooks.formEnabledTooltip')}>
              <HelpIconButton aria-label={t('pages:hooks.formEnabledLabel')} />
            </Tooltip>
          </Stack>
          <Switch
            checked={enabled}
            onCheckedChange={(v) => form.setValue('enabled', v)}
          />
        </div>

        <FormField
          label={t('pages:hooks.applicableIngressLabel')}
          helpText={t('pages:hooks.applicableIngressHelp')}
          required
        >
          <MultiSelectDropdown
            label={t('pages:hooks.applicableIngressLabel')}
            options={applicableIngressOptions}
            value={applicableIngress}
            className={styles.applicableIngressSelect}
            onChange={(next) => {
              setApplicableIngress((prev) => normalizeApplicableIngress(next, prev));
            }}
            emptyLabel={t('pages:hooks.applicableIngressEmpty')}
          />
        </FormField>
      </div>

      <ConfigSection
        form={form}
        type={type}
        registryError={registryError}
        implSelectOptions={implSelectOptions}
        selectedImplementationId={selectedImplementationId}
        onImplementationChange={handleImplementationChange}
        webhookTargetOption={webhookTargetOption}
        setWebhookTargetOption={setWebhookTargetOption}
        aiguardWebhookUrl={aiguardWebhookUrl}
        schema={schema}
        useManualConfigEditor={useManualConfigEditor}
        setUseManualConfigEditor={setUseManualConfigEditor}
        configObject={configObject}
        setConfigObject={setConfigObject}
        manualConfigJson={manualConfigJson}
        setManualConfigJson={setManualConfigJson}
      />
    </Stack>
  );

  if (embedded) {
    return (
      <div className={styles.pageWrap}>
        {formInner}
        <div className={styles.footerActions}>
          {footerButtons}
        </div>
      </div>
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onClose(); }}
      title={hook ? t('pages:hooks.editHook') : t('pages:hooks.createHook')}
      size="lg"
    >
      {formInner}
      <div className={styles.footerActions}>
        {footerButtons}
      </div>
    </Dialog>
  );
}
