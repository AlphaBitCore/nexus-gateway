/**
 * InterceptionDomainForm — reusable modal form for Create / Edit of an
 * InterceptionDomain. Rendered by both the list page (create path) and the
 * detail page (edit path).
 *
 * The form carries all 11 domain fields plus a collapsed `adapterConfig`
 * JSON textarea. Enum dropdowns source their options from the locked Prisma
 * enum set.
 */
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Dialog,
  FormField,
  Input,
  Select,
  Stack,
  Switch,
  Textarea,
} from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import { useApi } from '@/hooks/useApi';
import {
  interceptionDomainApi,
  type InterceptionDomain,
  type InterceptionDomainCreatePayload,
  type InterceptionDomainUpdatePayload,
  type DefaultPathAction,
  type FailureAction,
  type HostMatchType,
  type NetworkZone,
} from '@/api/services';
import styles from './InterceptionDomainForm.module.css';

const HOST_MATCH_TYPES: HostMatchType[] = ['EXACT', 'PREFIX', 'GLOB', 'REGEX'];
const DEFAULT_PATH_ACTIONS: DefaultPathAction[] = ['PROCESS', 'PASSTHROUGH', 'BLOCK'];
const FAILURE_ACTIONS: FailureAction[] = ['FAIL_OPEN', 'FAIL_CLOSED'];
const NETWORK_ZONES: NetworkZone[] = ['PUBLIC', 'INTERNAL'];

export interface InterceptionDomainFormValues {
  name: string;
  description: string;
  hostPattern: string;
  hostMatchType: HostMatchType;
  adapterId: string;
  adapterConfigJson: string;
  enabled: boolean;
  priority: number;
  defaultPathAction: DefaultPathAction;
  onAdapterError: FailureAction;
  networkZone: NetworkZone;
}

export interface InterceptionDomainFormProps {
  open: boolean;
  mode: 'create' | 'edit';
  initial?: InterceptionDomain | null;
  onClose: () => void;
  /**
   * Called on a valid submit. For `create` the payload is the full
   * `InterceptionDomainCreatePayload`; for `edit` it is the partial update.
   * The caller is responsible for running the mutation and handling the
   * returned promise. The form waits for the returned promise before
   * clearing its `saving` flag so the confirm button shows a spinner.
   */
  onSubmit: (
    payload: InterceptionDomainCreatePayload | InterceptionDomainUpdatePayload,
  ) => Promise<unknown>;
}

const emptyValues: InterceptionDomainFormValues = {
  name: '',
  description: '',
  hostPattern: '',
  hostMatchType: 'EXACT',
  adapterId: '',
  adapterConfigJson: '',
  enabled: true,
  priority: 0,
  defaultPathAction: 'PROCESS',
  onAdapterError: 'FAIL_OPEN',
  networkZone: 'PUBLIC',
};

function valuesFromDomain(d: InterceptionDomain): InterceptionDomainFormValues {
  return {
    name: d.name,
    description: d.description ?? '',
    hostPattern: d.hostPattern,
    hostMatchType: d.hostMatchType,
    adapterId: d.adapterId,
    adapterConfigJson: d.adapterConfig
      ? JSON.stringify(d.adapterConfig, null, 2)
      : '',
    enabled: d.enabled,
    priority: d.priority,
    defaultPathAction: d.defaultPathAction,
    onAdapterError: d.onAdapterError,
    networkZone: d.networkZone,
  };
}

export function InterceptionDomainForm({
  open,
  mode,
  initial,
  onClose,
  onSubmit,
}: InterceptionDomainFormProps) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const [values, setValues] = useState<InterceptionDomainFormValues>(emptyValues);
  const [saving, setSaving] = useState(false);

  const { data: adapterCatalog, loading: adapterCatalogLoading } = useApi(
    () => interceptionDomainApi.listTrafficAdaptersCatalog(),
    ['admin', 'traffic-adapters', 'catalog'],
    { skip: !open },
  );

  const adapterOptions = useMemo(
    () => adapterCatalog?.data ?? [],
    [adapterCatalog?.data],
  );

  useEffect(() => {
    if (!open) return;
    if (initial) {
      setValues(valuesFromDomain(initial));
      return;
    }
    setValues(emptyValues);
  }, [open, initial]);

  useEffect(() => {
    if (!open || initial) return;
    setValues((prev) => {
      if (prev.adapterId !== '') return prev;
      if (adapterOptions.length === 0) return prev;
      return { ...prev, adapterId: adapterOptions[0] };
    });
  }, [open, initial, adapterOptions]);

  const update = <K extends keyof InterceptionDomainFormValues>(
    key: K,
    v: InterceptionDomainFormValues[K],
  ) => {
    setValues((prev) => ({ ...prev, [key]: v }));
  };

  const buildPayload = ():
    | InterceptionDomainCreatePayload
    | InterceptionDomainUpdatePayload
    | null => {
    const name = values.name.trim();
    const hostPattern = values.hostPattern.trim();
    const adapterId = values.adapterId.trim();
    if (!name || !hostPattern || !adapterId) {
      addToast(
        t(
          'pages:interceptionDomains.validation.required',
          'name, hostPattern, and adapterId are required',
        ),
        'error',
      );
      return null;
    }
    let adapterConfig: Record<string, unknown> | null | undefined;
    const trimmed = values.adapterConfigJson.trim();
    if (trimmed === '') {
      adapterConfig = undefined;
    } else {
      try {
        adapterConfig = JSON.parse(trimmed) as Record<string, unknown>;
      } catch {
        addToast(
          t(
            'pages:interceptionDomains.validation.adapterConfigJson',
            'adapterConfig must be valid JSON',
          ),
          'error',
        );
        return null;
      }
    }

    const base = {
      name,
      description: values.description.trim() === '' ? null : values.description.trim(),
      hostPattern,
      hostMatchType: values.hostMatchType,
      adapterId,
      adapterConfig,
      enabled: values.enabled,
      priority: Number(values.priority) || 0,
      defaultPathAction: values.defaultPathAction,
      onAdapterError: values.onAdapterError,
      networkZone: values.networkZone,
    };
    return base;
  };

  const handleSubmit = async () => {
    const payload = buildPayload();
    if (!payload) return;
    setSaving(true);
    try {
      await onSubmit(payload);
      onClose();
    } catch {
      // Errors are surfaced by the caller's useMutation; keep the form open.
    } finally {
      setSaving(false);
    }
  };

  const title =
    mode === 'create'
      ? t('pages:interceptionDomains.createTitle', 'Create interception domain')
      : t('pages:interceptionDomains.editTitle', 'Edit interception domain');

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title={title}
      className={styles.formDialog}
    >
      <div className={styles.formShell}>
        <Stack gap="md" className={styles.formContent}>
          <FormField label={t('pages:interceptionDomains.name', 'Name')}>
            <Input
              value={values.name}
              onChange={(e) => update('name', e.target.value)}
              placeholder={t('pages:interception.endpointExamplePlaceholder')}
            />
          </FormField>

        <FormField label={t('pages:interceptionDomains.description', 'Description')}>
          <Textarea
            value={values.description}
            onChange={(e) => update('description', e.target.value)}
            rows={2}
          />
        </FormField>

        <Stack direction="horizontal" gap="md">
          <div style={{ flex: 2 }}>
            <FormField label={t('pages:interceptionDomains.hostPattern', 'Host pattern')}>
              <Input
                value={values.hostPattern}
                onChange={(e) => update('hostPattern', e.target.value)}
                placeholder="api.openai.com"
              />
            </FormField>
          </div>
          <div style={{ flex: 1 }}>
            <FormField
              label={t('pages:interceptionDomains.hostMatchType', 'Host match type')}
            >
              <Select
                value={values.hostMatchType}
                onValueChange={(v) => update('hostMatchType', v as HostMatchType)}
                options={HOST_MATCH_TYPES.map((v) => ({
                  value: v,
                  label: t(`pages:interceptionDomains.enums.${v}`, v),
                }))}
              />
            </FormField>
          </div>
        </Stack>

        <Stack direction="horizontal" gap="md">
          <div style={{ flex: 1 }}>
            <FormField label={t('pages:interceptionDomains.adapterId', 'Adapter')}>
              <Select
                value={values.adapterId}
                onValueChange={(v) => update('adapterId', v)}
                options={
                  adapterOptions.length > 0
                    ? adapterOptions.map((id) => ({ value: id, label: id }))
                    : [
                        {
                          value: '',
                          label: adapterCatalogLoading
                            ? t('pages:interceptionDomains.adapterCatalogLoading', 'Loading adapters…')
                            : t('pages:interceptionDomains.adapterCatalogEmpty', 'No adapters available'),
                        },
                      ]
                }
              />
            </FormField>
          </div>
          <div style={{ flex: 1 }}>
            <FormField label={t('pages:interceptionDomains.priority', 'Priority')}>
              <Input
                type="number"
                value={String(values.priority)}
                onChange={(e) => update('priority', Number(e.target.value) || 0)}
              />
            </FormField>
          </div>
        </Stack>

        <FormField
          label={t('pages:interceptionDomains.adapterConfig', 'Adapter config (JSON)')}
        >
          <Textarea
            value={values.adapterConfigJson}
            onChange={(e) => update('adapterConfigJson', e.target.value)}
            rows={3}
            placeholder='{"timeoutMs": 5000}'
            style={{ fontFamily: 'monospace', fontSize: 'var(--g-font-size-sm)' }}
          />
        </FormField>

        <Stack direction="horizontal" gap="md">
          <div style={{ flex: 1 }}>
            <FormField
              label={t(
                'pages:interceptionDomains.defaultPathAction',
                'Default path action',
              )}
            >
              <Select
                value={values.defaultPathAction}
                onValueChange={(v) =>
                  update('defaultPathAction', v as DefaultPathAction)
                }
                options={DEFAULT_PATH_ACTIONS.map((v) => ({
                  value: v,
                  label: t(`pages:interceptionDomains.enums.${v}`, v),
                }))}
              />
            </FormField>
          </div>
          <div style={{ flex: 1 }}>
            <FormField
              label={t('pages:interceptionDomains.onAdapterError', 'On adapter error')}
            >
              <Select
                value={values.onAdapterError}
                onValueChange={(v) => update('onAdapterError', v as FailureAction)}
                options={FAILURE_ACTIONS.map((v) => ({
                  value: v,
                  label: t(`pages:interceptionDomains.enums.${v}`, v),
                }))}
              />
            </FormField>
          </div>
          <div style={{ flex: 1 }}>
            <FormField
              label={t('pages:interceptionDomains.networkZone', 'Network zone')}
            >
              <Select
                value={values.networkZone}
                onValueChange={(v) => update('networkZone', v as NetworkZone)}
                options={NETWORK_ZONES.map((v) => ({
                  value: v,
                  label: t(`pages:interceptionDomains.enums.${v}`, v),
                }))}
              />
            </FormField>
          </div>
        </Stack>

        <FormField label={t('pages:interceptionDomains.enabled', 'Enabled')}>
          <Switch
            checked={values.enabled}
            onCheckedChange={(v) => update('enabled', v)}
          />
        </FormField>

        </Stack>
        <Stack direction="horizontal" gap="sm" justify="end" className={styles.formActions}>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t('common:cancel', 'Cancel')}
          </Button>
          <Button
            variant="primary"
            onClick={handleSubmit}
            disabled={saving}
          >
            {saving
              ? t('common:saving', 'Saving…')
              : mode === 'create'
                ? t('pages:interceptionDomains.create', 'Create')
                : t('common:save', 'Save')}
          </Button>
        </Stack>
      </div>
    </Dialog>
  );
}
