/**
 * Emergency Passthrough admin page (`/ai-gateway/passthrough`).
 *
 * The kill-switch UI for incident response. Operates the 3-tier passthrough
 * config (global / adapter / provider) backed by
 * `packages/control-plane/internal/handler/admin_passthrough.go`. Reads use
 * the bulk snapshot endpoint so all 3 panels render in one round-trip.
 *
 * Emergency-UX choices on this page:
 *   - Red banner at top whenever any tier has `enabled=true`, with the
 *     active tier's expiresAt countdown.
 *   - Reason field is required (≥ 20 chars) and surfaces a live char counter.
 *   - Expires-at is constrained to NOW + 8h max (matches DB CHECK).
 *   - Toggling bypassNormalize auto-toggles bypassCache (cross-constraint
 *     enforced server-side; we mirror it client-side for instant feedback).
 *   - Saving an enabled=true row pops a confirmation modal that recaps the
 *     reason + which flags are being bypassed + when it'll auto-disable.
 */
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  passthroughApi,
  validatePassthroughPayload,
  type PassthroughSnapshot,
  type PassthroughTier,
} from '@/api/services';
import { providerApi } from '@/api/services';
import { PROVIDER_ADAPTER_TYPES } from '@/pages/ai-gateway/providers/_shared/adapterTypes';
import type { Provider } from '@/api/types';
import {
  PageHeader,
  Card,
  Stack,
  Button,
  Badge,
  Dialog,
  Skeleton,
  ErrorBanner,
  FormField,
  Select,
} from '@/components/ui';
import {
  emptyTier,
  tierToForm,
  formToPayload,
  bypassSummary,
  type TierFormState,
} from './passthroughForm';
import { ActiveBanner } from './ActiveBanner';
import { Countdown } from './Countdown';
import { TierEditor } from './TierEditor';
import { EnableConfirmDialog } from './EnableConfirmDialog';
import styles from './PassthroughPage.module.css';

export function PassthroughPage() {
  const { t } = useTranslation();
  const canEmergencyEnable = usePermission('passthrough:emergencyEnable');
  const canDelete = usePermission('passthrough:write');

  const { data: snapshot, loading, error, refetch } = useApi<PassthroughSnapshot>(
    () => passthroughApi.getSnapshot(),
    ['admin', 'passthrough', 'snapshot'],
  );

  if (loading && !snapshot) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const snap: PassthroughSnapshot = snapshot ?? { global: emptyTier(), adapters: {}, providers: {} };

  return (
    <>
      <div className={styles.pageHeader}>
        <PageHeader title={t('pages:passthrough.title')} subtitle={t('pages:passthrough.subtitle')} />
      </div>
      <Stack gap="lg" className={styles.contentStack}>
        <ActiveBanner snapshot={snap} />
        <GlobalPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} />
        <AdapterOverridesPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} canDelete={canDelete} />
        <ProviderOverridesPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} canDelete={canDelete} />
      </Stack>
    </>
  );
}

function GlobalPanel({ snapshot, onChange, canEnable }: { snapshot: PassthroughSnapshot; onChange: () => void; canEnable: boolean }) {
  const { t } = useTranslation();
  const [form, setForm] = useState<TierFormState>(() => tierToForm(snapshot.global));
  const [confirmOpen, setConfirmOpen] = useState(false);

  useEffect(() => { setForm(tierToForm(snapshot.global)); }, [snapshot.global]);

  const code = validatePassthroughPayload(formToPayload(form));
  const valid = code === null;

  const { mutate: save, loading: saving } = useMutation(
    () => passthroughApi.putGlobal(formToPayload(form)),
    {
      invalidateQueries: [['admin', 'passthrough', 'snapshot']],
      onSuccess: () => { setConfirmOpen(false); onChange(); },
      successMessage: t('pages:passthrough.toasts.savedGlobal'),
      errorMessage: t('pages:passthrough.toasts.saveError'),
    },
  );

  const onSave = () => {
    if (form.enabled) setConfirmOpen(true);
    else save(undefined);
  };

  return (
    <section className={styles.panelSection}>
      <div className={styles.panelHeader}>
        <h2 className={styles.panelTitle}>{t('pages:passthrough.global.title')}</h2>
        <p className={styles.subtitle}>{t('pages:passthrough.global.subtitle')}</p>
      </div>
      <Card>
        <Stack gap="md">
        <TierEditor
          form={form}
          setForm={setForm}
          disabled={!canEnable && form.enabled !== snapshot.global.enabled}
          showEnabledByline
          enabledBy={snapshot.global.enabledBy}
        />
        {!valid && form.enabled && (
          <div className={styles.validation}>{t(`pages:passthrough.validation.${code}`)}</div>
        )}
        <Stack direction="horizontal" gap="sm" className={styles.globalActions}>
          <Button className={styles.saveButton} onClick={onSave} disabled={saving || !canEnable || (form.enabled && !valid)} variant={form.enabled ? 'danger' : 'primary'}>
            {saving ? t('common:saving') : form.enabled ? t('pages:passthrough.global.saveEnableBtn') : t('pages:passthrough.global.saveDisableBtn')}
          </Button>
          {!canEnable && (
            <span className={styles.subtitle}>{t('pages:passthrough.noPermissionToEnable')}</span>
          )}
        </Stack>
        </Stack>

        <EnableConfirmDialog
          open={confirmOpen}
          onClose={() => setConfirmOpen(false)}
          onConfirm={() => save(undefined)}
          scope="global"
          scopeKey="global"
          form={form}
        />
      </Card>
    </section>
  );
}

function AdapterOverridesPanel({ snapshot, onChange, canEnable, canDelete }: { snapshot: PassthroughSnapshot; onChange: () => void; canEnable: boolean; canDelete: boolean }) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<string | null>(null);

  const adapters = Object.entries(snapshot.adapters).sort(([a], [b]) => a.localeCompare(b));

  return (
    <section className={styles.panelSection}>
      <div className={styles.panelHeaderRow}>
        <div className={styles.panelHeader}>
          <h2 className={styles.panelTitle}>{t('pages:passthrough.adapter.title')}</h2>
          <p className={styles.subtitle}>{t('pages:passthrough.adapter.subtitle')}</p>
        </div>
        <Button className={styles.textActionButton} onClick={() => setEditing('')} disabled={!canEnable}>
          <span className={styles.textActionIcon} aria-hidden>+</span>
          <span>{t('pages:passthrough.adapter.addBtn')}</span>
        </Button>
      </div>
      <Card>
        <Stack gap="md">
        {adapters.length === 0 ? (
          <p className={styles.emptyState}>{t('pages:passthrough.adapter.empty')}</p>
        ) : (
          <table className={styles.tierTable}>
            <thead>
              <tr>
                <th>{t('pages:passthrough.adapter.colAdapter')}</th>
                <th>{t('pages:passthrough.adapter.colState')}</th>
                <th>{t('pages:passthrough.adapter.colFlags')}</th>
                <th>{t('pages:passthrough.adapter.colExpires')}</th>
                <th>{t('pages:passthrough.adapter.colEnabledBy')}</th>
                <th>{t('common:actions')}</th>
              </tr>
            </thead>
            <tbody>
              {adapters.map(([adapter, tier]) => (
                <tr key={adapter}>
                  <td><code>{adapter}</code></td>
                  <td>{tier.enabled ? <Badge variant="danger">{t('pages:passthrough.state.enabled')}</Badge> : <Badge variant="default">{t('pages:passthrough.state.disabled')}</Badge>}</td>
                  <td>{bypassSummary(tier) || <span className={styles.empty}>—</span>}</td>
                  <td>{tier.enabled ? <Countdown expiresAt={tier.expiresAt} /> : '—'}</td>
                  <td>{tier.enabledBy ?? '—'}</td>
                  <td>
                    <Stack direction="horizontal" gap="xs">
                      <Button variant="secondary" size="sm" onClick={() => setEditing(adapter)}>{t('common:edit')}</Button>
                      <Button
                        variant="danger"
                        size="sm"
                        disabled={!canDelete}
                        onClick={async () => {
                          if (!window.confirm(t('pages:passthrough.adapter.deleteConfirm', { adapter }))) return;
                          await passthroughApi.deleteAdapter(adapter);
                          onChange();
                        }}
                      >
                        {t('common:delete')}
                      </Button>
                    </Stack>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        </Stack>

        {editing !== null && (
          <AdapterEditorDialog
            adapterType={editing}
            existing={editing ? snapshot.adapters[editing] : undefined}
            onClose={() => setEditing(null)}
            onSaved={() => { setEditing(null); onChange(); }}
          />
        )}
      </Card>
    </section>
  );
}

function AdapterEditorDialog({
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
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button onClick={onSave} disabled={saving || (form.enabled && !valid) || (isNew && !selectedAdapter)} variant={form.enabled ? 'danger' : 'primary'}>
            {saving ? t('common:saving') : form.enabled ? t('pages:passthrough.global.saveEnableBtn') : t('pages:passthrough.global.saveDisableBtn')}
          </Button>
        </Stack>
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

function ProviderOverridesPanel({ snapshot, onChange, canEnable, canDelete }: { snapshot: PassthroughSnapshot; onChange: () => void; canEnable: boolean; canDelete: boolean }) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<string | null>(null);

  const providers = Object.entries(snapshot.providers).sort(([a], [b]) => a.localeCompare(b));
  const providerName = (id: string) => snapshot.providerNames?.[id] ?? id.slice(0, 8) + '…';

  return (
    <section className={styles.panelSection}>
      <div className={styles.panelHeaderRow}>
        <div className={styles.panelHeader}>
          <h2 className={styles.panelTitle}>{t('pages:passthrough.provider.title')}</h2>
          <p className={styles.subtitle}>{t('pages:passthrough.provider.subtitle')}</p>
        </div>
        <Button className={styles.textActionButton} onClick={() => setEditing('')} disabled={!canEnable}>
          <span className={styles.textActionIcon} aria-hidden>+</span>
          <span>{t('pages:passthrough.provider.addBtn')}</span>
        </Button>
      </div>
      <Card>
        <Stack gap="md">
        {providers.length === 0 ? (
          <p className={styles.emptyState}>{t('pages:passthrough.provider.empty')}</p>
        ) : (
          <table className={styles.tierTable}>
            <thead>
              <tr>
                <th>{t('pages:passthrough.provider.colProvider')}</th>
                <th>{t('pages:passthrough.adapter.colState')}</th>
                <th>{t('pages:passthrough.adapter.colFlags')}</th>
                <th>{t('pages:passthrough.adapter.colExpires')}</th>
                <th>{t('pages:passthrough.adapter.colEnabledBy')}</th>
                <th>{t('common:actions')}</th>
              </tr>
            </thead>
            <tbody>
              {providers.map(([pid, tier]) => (
                <tr key={pid}>
                  <td><strong>{providerName(pid)}</strong> <code className={styles.muted}>{pid.slice(0, 8)}…</code></td>
                  <td>{tier.enabled ? <Badge variant="danger">{t('pages:passthrough.state.enabled')}</Badge> : <Badge variant="default">{t('pages:passthrough.state.disabled')}</Badge>}</td>
                  <td>{bypassSummary(tier) || <span className={styles.empty}>—</span>}</td>
                  <td>{tier.enabled ? <Countdown expiresAt={tier.expiresAt} /> : '—'}</td>
                  <td>{tier.enabledBy ?? '—'}</td>
                  <td>
                    <Stack direction="horizontal" gap="xs">
                      <Button variant="secondary" size="sm" onClick={() => setEditing(pid)}>{t('common:edit')}</Button>
                      <Button
                        variant="danger"
                        size="sm"
                        disabled={!canDelete}
                        onClick={async () => {
                          if (!window.confirm(t('pages:passthrough.provider.deleteConfirm', { provider: providerName(pid) }))) return;
                          await passthroughApi.deleteProvider(pid);
                          onChange();
                        }}
                      >
                        {t('common:delete')}
                      </Button>
                    </Stack>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        </Stack>
        {editing !== null && (
          <ProviderEditorDialog
            providerId={editing}
            existing={editing ? snapshot.providers[editing] : undefined}
            onClose={() => setEditing(null)}
            onSaved={() => { setEditing(null); onChange(); }}
          />
        )}
      </Card>
    </section>
  );
}

function ProviderEditorDialog({
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
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button onClick={onSave} disabled={saving || (form.enabled && !valid) || (isNew && !selectedProvider)} variant={form.enabled ? 'danger' : 'primary'}>
            {saving ? t('common:saving') : form.enabled ? t('pages:passthrough.global.saveEnableBtn') : t('pages:passthrough.global.saveDisableBtn')}
          </Button>
        </Stack>
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
