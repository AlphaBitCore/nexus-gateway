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
  PASSTHROUGH_MIN_REASON_LEN,
  PASSTHROUGH_MAX_EXPIRY_HOURS,
  type PassthroughSnapshot,
  type PassthroughPayload,
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
  Switch,
  Input,
  FormField,
  Badge,
  Dialog,
  Skeleton,
  ErrorBanner,
  Select,
} from '@/components/ui';
import styles from './PassthroughPage.module.css';

type TierKind = 'global' | 'adapter' | 'provider';

interface TierFormState {
  enabled: boolean;
  bypassHooks: boolean;
  bypassCache: boolean;
  bypassNormalize: boolean;
  expiresAt: string; // ISO local datetime-local input
  reason: string;
}

const EMPTY_FORM: TierFormState = {
  enabled: false,
  bypassHooks: false,
  bypassCache: false,
  bypassNormalize: false,
  expiresAt: '',
  reason: '',
};

function tierToForm(t: PassthroughTier | undefined): TierFormState {
  if (!t) return EMPTY_FORM;
  return {
    enabled: t.enabled,
    bypassHooks: t.bypassHooks,
    bypassCache: t.bypassCache,
    bypassNormalize: t.bypassNormalize,
    expiresAt: t.expiresAt ? toLocalInputValue(t.expiresAt) : '',
    reason: t.reason ?? '',
  };
}

/** Convert ISO to the `<input type="datetime-local">` value (no Z, no ms). */
function toLocalInputValue(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function formToPayload(f: TierFormState): PassthroughPayload {
  return {
    enabled: f.enabled,
    bypassHooks: f.bypassHooks,
    bypassCache: f.bypassCache,
    bypassNormalize: f.bypassNormalize,
    expiresAt: f.enabled && f.expiresAt ? new Date(f.expiresAt).toISOString() : null,
    reason: f.reason,
  };
}

/** Default expiresAt for newly-enabled rows: NOW + 1 hour, rounded to the minute. */
function defaultExpiresAt(): string {
  const d = new Date(Date.now() + 60 * 60 * 1000);
  d.setSeconds(0, 0);
  return toLocalInputValue(d.toISOString());
}

function maxExpiresAt(): string {
  const d = new Date(Date.now() + PASSTHROUGH_MAX_EXPIRY_HOURS * 60 * 60 * 1000);
  d.setSeconds(0, 0);
  return toLocalInputValue(d.toISOString());
}

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
      <PageHeader title={t('pages:passthrough.title')} subtitle={t('pages:passthrough.subtitle')} />
      <Stack gap="lg">
        <ActiveBanner snapshot={snap} />
        <GlobalPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} />
        <AdapterOverridesPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} canDelete={canDelete} />
        <ProviderOverridesPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} canDelete={canDelete} />
      </Stack>
    </>
  );
}

function emptyTier(): PassthroughTier {
  return {
    enabled: false,
    bypassHooks: false,
    bypassCache: false,
    bypassNormalize: false,
  };
}

function ActiveBanner({ snapshot }: { snapshot: PassthroughSnapshot }) {
  const { t } = useTranslation();
  // Count enabled rows across all tiers.
  const enabledTiers: { kind: string; key: string; tier: PassthroughTier }[] = [];
  if (snapshot.global.enabled) enabledTiers.push({ kind: 'global', key: 'global', tier: snapshot.global });
  for (const [k, v] of Object.entries(snapshot.adapters)) if (v.enabled) enabledTiers.push({ kind: 'adapter', key: k, tier: v });
  for (const [k, v] of Object.entries(snapshot.providers)) if (v.enabled) enabledTiers.push({ kind: 'provider', key: k, tier: v });

  if (enabledTiers.length === 0) {
    return (
      <div className={styles.bannerInactive}>
        <strong>{t('pages:passthrough.banner.inactiveTitle')}</strong>
        <span>{t('pages:passthrough.banner.inactiveBody')}</span>
      </div>
    );
  }
  return (
    <div className={styles.bannerActive}>
      <div className={styles.bannerTitleRow}>
        <span className={styles.bannerDot} aria-hidden />
        <strong>{t('pages:passthrough.banner.activeTitle', { count: enabledTiers.length })}</strong>
      </div>
      <div className={styles.bannerBody}>{t('pages:passthrough.banner.activeBody')}</div>
      <ul className={styles.bannerList}>
        {enabledTiers.map(e => (
          <li key={`${e.kind}:${e.key}`}>
            <Badge variant="danger">{e.kind}</Badge> <code>{e.key}</code>{' '}
            {bypassSummary(e.tier)} · <Countdown expiresAt={e.tier.expiresAt} />
          </li>
        ))}
      </ul>
    </div>
  );
}

function bypassSummary(t: PassthroughTier): string {
  const flags: string[] = [];
  if (t.bypassHooks) flags.push('hooks');
  if (t.bypassCache) flags.push('cache');
  if (t.bypassNormalize) flags.push('normalize');
  return flags.length ? `[${flags.join(',')}]` : '';
}

function Countdown({ expiresAt }: { expiresAt?: string | null }) {
  const { t } = useTranslation();
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);
  if (!expiresAt) return <span>{t('pages:passthrough.countdown.noExpiry')}</span>;
  const remainingMs = new Date(expiresAt).getTime() - now;
  if (remainingMs <= 0) return <span className={styles.countdownExpired}>{t('pages:passthrough.countdown.expired')}</span>;
  const totalSec = Math.floor(remainingMs / 1000);
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  return <span className={styles.countdownActive}>{h > 0 ? `${h}h ${m}m` : `${m}m ${s}s`}</span>;
}

function TierEditor({
  form,
  setForm,
  disabled,
  showEnabledByline,
  enabledBy,
}: {
  form: TierFormState;
  setForm: (next: TierFormState) => void;
  disabled?: boolean;
  showEnabledByline?: boolean;
  enabledBy?: string;
}) {
  const { t } = useTranslation();
  // Cross-constraint: bypassNormalize requires bypassCache.
  const setBypass = (key: 'bypassHooks' | 'bypassCache' | 'bypassNormalize', v: boolean) => {
    const next = { ...form, [key]: v };
    if (key === 'bypassNormalize' && v) next.bypassCache = true;
    if (key === 'bypassCache' && !v) next.bypassNormalize = false;
    setForm(next);
  };
  return (
    <Stack gap="md">
      <FormField label={t('pages:passthrough.fields.enabled')} helpText={t('pages:passthrough.fields.enabledHint')}>
        <Switch
          checked={form.enabled}
          disabled={disabled}
          onCheckedChange={v => {
            const next = { ...form, enabled: v };
            // First enable in this session: prefill expires + reset reason char counter.
            if (v && !form.expiresAt) next.expiresAt = defaultExpiresAt();
            setForm(next);
          }}
        />
      </FormField>

      <div className={styles.flagGrid}>
        <FormField label={t('pages:passthrough.fields.bypassHooks')} helpText={t('pages:passthrough.fields.bypassHooksHint')}>
          <Switch checked={form.bypassHooks} disabled={disabled} onCheckedChange={v => setBypass('bypassHooks', v)} />
        </FormField>
        <FormField label={t('pages:passthrough.fields.bypassCache')} helpText={t('pages:passthrough.fields.bypassCacheHint')}>
          <Switch checked={form.bypassCache} disabled={disabled || form.bypassNormalize} onCheckedChange={v => setBypass('bypassCache', v)} />
        </FormField>
        <FormField label={t('pages:passthrough.fields.bypassNormalize')} helpText={t('pages:passthrough.fields.bypassNormalizeHint')}>
          <Switch checked={form.bypassNormalize} disabled={disabled} onCheckedChange={v => setBypass('bypassNormalize', v)} />
        </FormField>
      </div>

      <FormField
        label={t('pages:passthrough.fields.expiresAt')}
        helpText={t('pages:passthrough.fields.expiresAtHint', { hours: PASSTHROUGH_MAX_EXPIRY_HOURS })}
      >
        <Input
          type="datetime-local"
          value={form.expiresAt}
          max={maxExpiresAt()}
          disabled={disabled || !form.enabled}
          onChange={e => setForm({ ...form, expiresAt: e.target.value })}
        />
      </FormField>

      <FormField
        label={t('pages:passthrough.fields.reason', { count: PASSTHROUGH_MIN_REASON_LEN })}
        helpText={t('pages:passthrough.fields.reasonHint', { count: form.reason.length, min: PASSTHROUGH_MIN_REASON_LEN })}
      >
        <textarea
          className={styles.reasonInput}
          value={form.reason}
          disabled={disabled || !form.enabled}
          rows={3}
          onChange={e => setForm({ ...form, reason: e.target.value })}
        />
      </FormField>

      {showEnabledByline && enabledBy && (
        <div className={styles.byline}>{t('pages:passthrough.enabledByLine', { user: enabledBy })}</div>
      )}
    </Stack>
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
    <Card>
      <Stack gap="md">
        <h2 className={styles.panelTitle}>{t('pages:passthrough.global.title')}</h2>
        <p className={styles.subtitle}>{t('pages:passthrough.global.subtitle')}</p>
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
        <Stack direction="horizontal" gap="sm">
          <Button onClick={onSave} disabled={saving || !canEnable || (form.enabled && !valid)} variant={form.enabled ? 'danger' : 'primary'}>
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
  );
}

function AdapterOverridesPanel({ snapshot, onChange, canEnable, canDelete }: { snapshot: PassthroughSnapshot; onChange: () => void; canEnable: boolean; canDelete: boolean }) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<string | null>(null);

  const adapters = Object.entries(snapshot.adapters).sort(([a], [b]) => a.localeCompare(b));

  return (
    <Card>
      <Stack gap="md">
        <Stack direction="horizontal" gap="md" justify="between">
          <div>
            <h2 className={styles.panelTitle}>{t('pages:passthrough.adapter.title')}</h2>
            <p className={styles.subtitle}>{t('pages:passthrough.adapter.subtitle')}</p>
          </div>
          <Button onClick={() => setEditing('')} disabled={!canEnable}>{t('pages:passthrough.adapter.addBtn')}</Button>
        </Stack>
        {adapters.length === 0 ? (
          <p className={styles.empty}>{t('pages:passthrough.adapter.empty')}</p>
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
    <Card>
      <Stack gap="md">
        <Stack direction="horizontal" gap="md" justify="between">
          <div>
            <h2 className={styles.panelTitle}>{t('pages:passthrough.provider.title')}</h2>
            <p className={styles.subtitle}>{t('pages:passthrough.provider.subtitle')}</p>
          </div>
          <Button onClick={() => setEditing('')} disabled={!canEnable}>{t('pages:passthrough.provider.addBtn')}</Button>
        </Stack>
        {providers.length === 0 ? (
          <p className={styles.empty}>{t('pages:passthrough.provider.empty')}</p>
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

function EnableConfirmDialog({
  open, onClose, onConfirm, scope, scopeKey, form,
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  scope: TierKind;
  scopeKey: string;
  form: TierFormState;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) onClose(); }} title={t('pages:passthrough.confirm.title')}>
      <Stack gap="md">
        <p className={styles.confirmBody}>{t('pages:passthrough.confirm.body')}</p>
        <ul className={styles.confirmList}>
          <li>{t('pages:passthrough.confirm.scope', { scope, scopeKey })}</li>
          <li>{t('pages:passthrough.confirm.flags', { flags: bypassSummary(form as unknown as PassthroughTier) || t('pages:passthrough.confirm.flagsNone') })}</li>
          <li>{t('pages:passthrough.confirm.expires', { expires: form.expiresAt ? new Date(form.expiresAt).toLocaleString() : '?' })}</li>
          <li>{t('pages:passthrough.confirm.reason', { reason: form.reason })}</li>
        </ul>
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
          <Button variant="danger" onClick={onConfirm}>{t('pages:passthrough.confirm.confirmBtn')}</Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
