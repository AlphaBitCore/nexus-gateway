import { useState, useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useToast } from '@/context/ToastContext';
import {
  Dialog, Button, Switch, Tooltip, Stack, Card,
  FormField, Select, Textarea,
  MultiSelectDropdown,
} from '@/components/ui';
import { useZodForm, FormInput, FormSelect } from '@/lib/forms';
import { z } from 'zod';
import { useMutation } from '@/hooks/useMutation';
import { useSyncFeedback } from '@/hooks/useSyncFeedback';
import { hookApi, serviceUrlsApi } from '@/api/services';
import type { HookWritePayload } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import type { HookConfig, AdminHookImplementationsResponse, HookImplementationSummary } from '@/api/types';
import {
  HOOK_APPLICABLE_INGRESS,
  HOOK_APPLICABLE_INGRESS_ORDER,
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
import { JsonSchemaHookConfigForm, buildDefaultsFromSchema } from '@/components/config/JsonSchemaHookConfigForm';
import { validateDataAgainstJsonSchema } from '@/lib/validate-json-schema';
import { aiguardComplianceWebhookUrl } from '@/lib/aiguardWebhook';
import styles from './HookForm.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

/**
 * Radix Select treats value="" as "no selection". Use a sentinel for the
 * "auto" category so Radix treats it as a real selected value.
 */
const CATEGORY_AUTO_SENTINEL = '__auto__';
const toCategorySentinel = (v: string) => (v === HOOK_CATEGORY_AUTO_VALUE ? CATEGORY_AUTO_SENTINEL : v);
const fromCategorySentinel = (v: string) => (v === CATEGORY_AUTO_SENTINEL ? HOOK_CATEGORY_AUTO_VALUE : v);
const INGRESS_ALLOWED = new Set<string>(HOOK_APPLICABLE_INGRESS_ORDER);

interface HookFormProps {
  hook?: HookConfig;
  onClose: () => void;
  onSaved: () => void;
  embedded?: boolean;
  onCreateSuccess?: (created: HookConfig) => void;
}

function normalizeApplicableIngress(
  nextRaw: string[] | undefined,
  prevRaw?: string[],
): string[] {
  const all = HOOK_APPLICABLE_INGRESS.ALL;
  const canonicalize = (codes?: string[]) =>
    Array.from(new Set(
      (codes ?? [])
        .map((code) => {
          const normalized = (code ?? '').trim().toUpperCase();
          return INGRESS_ALLOWED.has(normalized) ? normalized : '';
        })
        .filter(Boolean),
    ));

  const next = canonicalize(nextRaw);
  const prev = canonicalize(prevRaw);
  const hadAll = prev.includes(all);
  const hasAll = next.includes(all);
  const specifics = next.filter((code) => code !== all);

  if (hasAll && specifics.length > 0) {
    // User selected ALL from a specific subset: ALL wins.
    if (!hadAll) return [all];
    // User selected a specific ingress while ALL was active: specific wins.
    return specifics;
  }
  if (hasAll) return [all];
  if (specifics.length > 0) return specifics;
  // Keep a valid non-empty value aligned with backend default semantics.
  return [all];
}

function asConfigRecord(cfg: unknown): Record<string, unknown> {
  if (cfg && typeof cfg === 'object' && !Array.isArray(cfg)) return cfg as Record<string, unknown>;
  return {};
}

function implementationsForRow(
  list: HookImplementationSummary[],
  rowType: string,
  stage: string,
): HookImplementationSummary[] {
  return list.filter((impl) => {
    if (!impl.supportedStages.includes(stage)) return false;
    if (rowType === HOOK_ROW_TYPE.WEBHOOK) return impl.implementationId === 'webhook.forward';
    if (rowType === HOOK_ROW_TYPE.SCRIPT) return impl.implementationId === 'noop';
    return impl.implementationId !== 'webhook.forward';
  });
}

const hookFormSchema = z.object({
  name: z.string().min(1),
  type: z.string().min(1),
  stage: z.string().min(1),
  priority: z.coerce.number().int(),
  timeoutMs: z.coerce.number().int().min(0),
  failBehavior: z.string().min(1),
  enabled: z.boolean(),
  category: z.string(),
  whEndpoint: z.string().optional().default(''),
});

type HookFormValues = z.infer<typeof hookFormSchema>;
type WebhookTargetOption = 'custom' | 'aiguard';

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

  const existingCfg = asConfigRecord(hook?.config);
  const [registry, setRegistry] = useState<AdminHookImplementationsResponse | null>(null);
  const [registryError, setRegistryError] = useState<string | null>(null);

  const [selectedImplementationId, setSelectedImplementationId] = useState(hook?.implementationId ?? '');
  const [configObject, setConfigObject] = useState<Record<string, unknown>>(() => ({ ...existingCfg }));
  const [manualConfigJson, setManualConfigJson] = useState(() =>
    hook ? JSON.stringify(existingCfg, null, 2) : '{}',
  );
  const [useManualConfigEditor, setUseManualConfigEditor] = useState(false);

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

  const implementations = registry?.data ?? [];
  const hookCategories = registry?.hookCategories ?? [];

  const filteredImplementations = useMemo(
    () => implementationsForRow(implementations, type, stage),
    [implementations, type, stage],
  );

  const selectedMeta = useMemo(
    () => filteredImplementations.find((i) => i.implementationId === selectedImplementationId),
    [filteredImplementations, selectedImplementationId],
  );

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

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await hookApi.getImplementations();
        if (!cancelled) {
          setRegistry(res);
          setRegistryError(null);
        }
      } catch (e) {
        if (!cancelled) {
          setRegistry(null);
          setRegistryError(e instanceof Error ? e.message : 'Failed to load hook registry');
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!filteredImplementations.length) return;
    const ids = new Set(filteredImplementations.map((i) => i.implementationId));
    if (selectedImplementationId && ids.has(selectedImplementationId)) return;

    const first = filteredImplementations[0];
    setSelectedImplementationId(first.implementationId);
    const sch = first.configSchema as Record<string, unknown> | undefined;
    const sameRow = hook?.implementationId === first.implementationId;
    if (sch) {
      setUseManualConfigEditor(false);
      setConfigObject(
        sameRow ? { ...buildDefaultsFromSchema(sch), ...asConfigRecord(hook?.config) } : { ...buildDefaultsFromSchema(sch) },
      );
    } else {
      setUseManualConfigEditor(true);
      setManualConfigJson(sameRow ? JSON.stringify(asConfigRecord(hook?.config), null, 2) : '{}');
    }
  }, [filteredImplementations, selectedImplementationId, hook]);

  const handleImplementationChange = (id: string) => {
    setSelectedImplementationId(id);
    const impl = implementations.find((i) => i.implementationId === id);
    const sch = impl?.configSchema as Record<string, unknown> | undefined;
    if (sch) {
      setConfigObject({ ...buildDefaultsFromSchema(sch) });
      setUseManualConfigEditor(false);
    } else {
      setManualConfigJson('{}');
      setUseManualConfigEditor(true);
    }
  };

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

  const buildConfigPayload = (): Record<string, unknown> | null => {
    const schema = selectedMeta?.configSchema as Record<string, unknown> | undefined;
    if (schema && !useManualConfigEditor) {
      const err = validateDataAgainstJsonSchema(schema, configObject);
      if (err) {
        addToast(`Config does not match schema: ${err}`, 'error');
        return null;
      }
      return { ...configObject };
    }
    try {
      const parsed = JSON.parse(manualConfigJson) as Record<string, unknown>;
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        addToast('Config must be a JSON object', 'error');
        return null;
      }
      if (schema) {
        const err = validateDataAgainstJsonSchema(schema, parsed);
        if (err) {
          addToast(`Config does not match schema: ${err}`, 'error');
          return null;
        }
      }
      return parsed;
    } catch {
      addToast('Invalid config JSON', 'error');
      return null;
    }
  };

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
      <FormInput form={form} name="name" label={t('pages:hooks.nameLabel')} required />

      {hook?.classification && (
        <div className={styles.classificationBox}>
          <div className={styles.classificationTitle}>{t('pages:hooks.classificationTitle')}</div>
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

      <FormSelect form={form} name="type" label={t('pages:hooks.formTypeLabel')} required helpText={t('pages:hooks.formTypeHelp')} options={HOOK_SELECT_TYPE_OPTIONS} />

      <FormSelect form={form} name="stage" label={t('pages:hooks.formStageLabel')} required helpText={t('pages:hooks.formStageHelp')} options={HOOK_SELECT_STAGE_OPTIONS} />

      <FormSelect form={form} name="category" label={t('pages:hooks.categoryOverrideLabel')} helpText={t('pages:hooks.categoryOverrideHint')} options={categorySelectOptions} />

      <FormInput form={form} name="priority" label={t('pages:hooks.formPriorityLabel')} helpText={t('pages:hooks.formPriorityHelp')} type="number" />

      <FormInput form={form} name="timeoutMs" label={t('pages:hooks.formTimeoutLabel')} helpText={t('pages:hooks.formTimeoutHelp')} type="number" />

      <FormSelect form={form} name="failBehavior" label={t('pages:hooks.formFailBehaviorLabel')} helpText={t('pages:hooks.formFailBehaviorHelp')} options={HOOK_SELECT_FAIL_OPTIONS} />

      <Stack direction="horizontal" gap="sm" className={styles.enabledRow}>
        <label className={styles.enabledLabel}>{t('pages:hooks.formEnabledLabel')}</label>
        <Tooltip content={t('pages:hooks.formEnabledTooltip')}>
          <HelpIconButton aria-label={t('pages:hooks.formEnabledLabel')} />
        </Tooltip>
        <Switch
          checked={enabled}
          onCheckedChange={(v) => form.setValue('enabled', v)}
        />
      </Stack>

      <FormField
        label={t('pages:hooks.applicableIngressLabel')}
        helpText={t('pages:hooks.applicableIngressHelp')}
        required
      >
        <MultiSelectDropdown
          label={t('pages:hooks.applicableIngressLabel')}
          options={applicableIngressOptions}
          value={applicableIngress}
          onChange={(next) => {
            setApplicableIngress((prev) => normalizeApplicableIngress(next, prev));
          }}
          emptyLabel={t('pages:hooks.applicableIngressEmpty')}
        />
      </FormField>

      <Card>
        <div className={styles.sectionTitle}>{t('pages:hooks.configurationSection')}</div>
        {registryError ? (
          <p className={styles.registryError}>{registryError}</p>
        ) : null}

        {implSelectOptions.length > 0 ? (
          <FormField label={t('pages:hooks.implementationFieldLabel')} required helpText={t('pages:hooks.implementationFieldHelp')}>
            <Select
              value={selectedImplementationId || implSelectOptions[0]?.value || ''}
              onValueChange={handleImplementationChange}
              options={implSelectOptions}
              placeholder={t('pages:hooks.selectImplementation')}
            />
          </FormField>
        ) : (
          <p className={styles.noImplementations}>
            {t('pages:hooks.noImplementationsMatch')}
          </p>
        )}

        {type === HOOK_ROW_TYPE.WEBHOOK ? (
          <Stack gap="sm">
            <FormField
              label={t('pages:hooks.webhookTargetLabel', 'Webhook target')}
              helpText={t(
                'pages:hooks.webhookTargetHelp',
                'Choose AIGuard for the built-in compliance endpoint, or keep custom for external webhooks.',
              )}
            >
              <Select
                value={webhookTargetOption}
                onValueChange={(value) => {
                  const option = value as WebhookTargetOption;
                  setWebhookTargetOption(option);
                  if (option === 'aiguard') {
                    form.setValue('whEndpoint', aiguardWebhookUrl, { shouldDirty: true });
                  }
                }}
                options={[
                  { value: 'aiguard', label: t('pages:hooks.webhookTargetAIGuard', 'AIGuard') },
                  { value: 'custom', label: t('pages:hooks.webhookTargetCustom', 'Custom') },
                ]}
              />
            </FormField>
            <FormInput form={form} name="whEndpoint" label={t('pages:hooks.endpointUrlLabel')} required helpText={t('pages:hooks.endpointUrlHelp')} type="url" placeholder={t('pages:hooks.endpointUrlPlaceholder')} />
          </Stack>
        ) : null}

        <Stack direction="horizontal" gap="sm" className={styles.configRow}>
          <label className={styles.enabledLabel}>{t('pages:hooks.manualJsonLabel')}</label>
          <Tooltip content={t('pages:hooks.manualJsonTooltip')}>
            <HelpIconButton aria-label={t('pages:hooks.manualJsonLabel')} />
          </Tooltip>
          <Switch
            checked={useManualConfigEditor}
            onCheckedChange={(c) => {
              setUseManualConfigEditor(c);
              if (c) setManualConfigJson(JSON.stringify(configObject, null, 2));
            }}
          />
        </Stack>

        {schema && !useManualConfigEditor ? (
          <JsonSchemaHookConfigForm schema={schema} value={configObject} onChange={setConfigObject} />
        ) : (
          <FormField label={t('pages:hooks.configJsonLabel')}>
            <Textarea
              name="manual-config-json"
              value={manualConfigJson}
              onChange={(e) => setManualConfigJson(e.target.value)}
              className={styles.monoTextarea}
            />
          </FormField>
        )}
      </Card>
    </Stack>
  );

  if (embedded) {
    return (
      <div className={styles.pageWrap}>
        <div className={styles.sectionTitleNoMargin}>
          {hook ? t('pages:hooks.editHook') : t('pages:hooks.newHook')}
        </div>
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
