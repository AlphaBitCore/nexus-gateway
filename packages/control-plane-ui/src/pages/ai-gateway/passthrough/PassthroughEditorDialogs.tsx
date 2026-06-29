/**
 * Drawer editors for the adapter- and provider-tier passthrough overrides.
 *
 * Extracted from `PassthroughPage.tsx` to keep that file readable; behavior is
 * identical. Both dialogs mirror the same shape: pick a key (only when adding),
 * edit the tier, and pop the enable-confirmation modal before saving an
 * `enabled=true` row.
 */
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import {
  passthroughApi,
  validatePassthroughPayload,
  type PassthroughTier,
} from '@/api/services';
import { providerApi } from '@/api/services';
import { PROVIDER_ADAPTER_TYPES } from '@/pages/ai-gateway/providers/_shared/adapterTypes';
import type { Provider } from '@/api/types';
import {
  Button,
  Stack,
  Dialog,
  FormField,
  Select,
} from '@/components/ui';
import {
  tierToForm,
  formToPayload,
  type TierFormState,
} from './passthroughForm';
import { TierEditor } from './TierEditor';
import { EnableConfirmDialog } from './EnableConfirmDialog';
import styles from './PassthroughPage.module.css';

export function AdapterEditorDialog({
  adapterType,
  existing,
  onClose,
  onSaved,
}: {
  adapterType: string;
  existing?: PassthroughTier;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const isNew = adapterType === '';
  const [selectedAdapter, setSelectedAdapter] = useState<string>(adapterType);
  const [form, setForm] = useState<TierFormState>(() => tierToForm(existing));
  const [confirmOpen, setConfirmOpen] = useState(false);

  const code = validatePassthroughPayload(formToPayload(form));
  const valid = code === null && (!isNew || !!selectedAdapter);

  const { mutate: save, loading: saving } = useMutation(
    () => passthroughApi.putAdapter(selectedAdapter, formToPayload(form)),
    {
      invalidateQueries: [['admin', 'passthrough', 'snapshot']],
      onSuccess: () => { setConfirmOpen(false); onSaved(); },
      successMessage: t('pages:passthrough.toasts.savedAdapter'),
      errorMessage: t('pages:passthrough.toasts.saveError'),
    },
  );

  const onSave = () => { if (form.enabled) setConfirmOpen(true); else save(undefined); };

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o) onClose(); }}
      title={isNew ? t('pages:passthrough.adapter.addBtn') : t('pages:passthrough.adapter.editTitle', { adapter: selectedAdapter })}
      variant="drawer"
      size="xl"
      footer={(
        <Stack direction="horizontal" gap="sm" className={styles.drawerFooterActions}>
          <Button onClick={onSave} disabled={saving || (form.enabled && !valid) || (isNew && !selectedAdapter)} variant={form.enabled ? 'danger' : 'primary'}>
            {saving ? t('common:saving') : form.enabled ? t('pages:passthrough.global.saveEnableBtn') : t('pages:passthrough.global.saveDisableBtn')}
          </Button>
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
        </Stack>
      )}
      footerClassName={styles.drawerFooter}
    >
      <Stack gap="md">
        {isNew && (
          <FormField label={t('pages:passthrough.adapter.colAdapter')} helpText={t('pages:passthrough.adapter.adapterTypeHint')}>
            <Select
              value={selectedAdapter}
              onValueChange={setSelectedAdapter}
              options={[{ value: '', label: t('common:choose') }, ...PROVIDER_ADAPTER_TYPES.map(a => ({ value: a, label: a }))]}
            />
          </FormField>
        )}
        <TierEditor form={form} setForm={setForm} showEnabledByline enabledBy={existing?.enabledBy} />
        {!valid && form.enabled && code && (
          <div className={styles.validation}>{t(`pages:passthrough.validation.${code}`)}</div>
        )}
      </Stack>
      <EnableConfirmDialog
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        onConfirm={() => save(undefined)}
        scope="adapter"
        scopeKey={selectedAdapter}
        form={form}
      />
    </Dialog>
  );
}

export function ProviderEditorDialog({
  providerId,
  existing,
  onClose,
  onSaved,
}: {
  providerId: string;
  existing?: PassthroughTier;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const isNew = providerId === '';
  const [selectedProvider, setSelectedProvider] = useState<string>(providerId);
  const [form, setForm] = useState<TierFormState>(() => tierToForm(existing));
  const [confirmOpen, setConfirmOpen] = useState(false);

  // Load provider list for the dropdown (only when adding a new override).
  const { data: providersResp } = useApi<{ data: Provider[]; total: number }>(
    () => providerApi.list({ limit: '200' }),
    ['admin', 'providers', 'list', 'passthrough-picker'],
  );
  const providers = useMemo(() => providersResp?.data ?? [], [providersResp]);

  const code = validatePassthroughPayload(formToPayload(form));
  const valid = code === null && (!isNew || !!selectedProvider);

  const { mutate: save, loading: saving } = useMutation(
    () => passthroughApi.putProvider(selectedProvider, formToPayload(form)),
    {
      invalidateQueries: [['admin', 'passthrough', 'snapshot']],
      onSuccess: () => { setConfirmOpen(false); onSaved(); },
      successMessage: t('pages:passthrough.toasts.savedProvider'),
      errorMessage: t('pages:passthrough.toasts.saveError'),
    },
  );
  const onSave = () => { if (form.enabled) setConfirmOpen(true); else save(undefined); };

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o) onClose(); }}
      title={isNew ? t('pages:passthrough.provider.addBtn') : t('pages:passthrough.provider.editTitle', { provider: selectedProvider.slice(0, 8) })}
      variant="drawer"
      size="xl"
      footer={(
        <Stack direction="horizontal" gap="sm" className={styles.drawerFooterActions}>
          <Button onClick={onSave} disabled={saving || (form.enabled && !valid) || (isNew && !selectedProvider)} variant={form.enabled ? 'danger' : 'primary'}>
            {saving ? t('common:saving') : form.enabled ? t('pages:passthrough.global.saveEnableBtn') : t('pages:passthrough.global.saveDisableBtn')}
          </Button>
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
        </Stack>
      )}
      footerClassName={styles.drawerFooter}
    >
      <Stack gap="md">
        {isNew && (
          <FormField label={t('pages:passthrough.provider.colProvider')} helpText={t('pages:passthrough.provider.providerHint')}>
            <Select
              value={selectedProvider}
              onValueChange={setSelectedProvider}
              options={[{ value: '', label: t('common:choose') }, ...providers.map(p => ({ value: p.id, label: `${p.name} (${p.adapterType})` }))]}
            />
          </FormField>
        )}
        <TierEditor form={form} setForm={setForm} showEnabledByline enabledBy={existing?.enabledBy} />
        {!valid && form.enabled && code && (
          <div className={styles.validation}>{t(`pages:passthrough.validation.${code}`)}</div>
        )}
      </Stack>
      <EnableConfirmDialog
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        onConfirm={() => save(undefined)}
        scope="provider"
        scopeKey={selectedProvider}
        form={form}
      />
    </Dialog>
  );
}
