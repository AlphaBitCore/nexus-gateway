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
} from '@/api/services';
import { providerApi } from '@/api/services';
import type { Provider } from '@/api/types';
import {
  PageHeader,
  Card,
  Stack,
  Button,
  Skeleton,
  ErrorBanner,
  AlertDialog,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent,
  RowActions,
  RowActionIconButton,
  RowDeleteAction,
  EditActionIcon,
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
import { AdapterEditorDialog, ProviderEditorDialog } from './PassthroughEditorDialogs';
import styles from './PassthroughPage.module.css';

export function PassthroughPage() {
  const { t } = useTranslation();
  const canEmergencyEnable = usePermission('passthrough:emergencyEnable');
  const canDelete = usePermission('passthrough:write');
  const [activeTierTab, setActiveTierTab] = useState('global');

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
        <Tabs value={activeTierTab} onValueChange={setActiveTierTab} className={styles.tierTabs}>
          <TabsList>
            <TabsTrigger value="global">{t('pages:passthrough.global.title')}</TabsTrigger>
            <TabsTrigger value="adapter">{t('pages:passthrough.adapter.title')}</TabsTrigger>
            <TabsTrigger value="provider">{t('pages:passthrough.provider.title')}</TabsTrigger>
          </TabsList>
          <TabsContent value="global" className={styles.tierTabContent}>
            <GlobalPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} />
          </TabsContent>
          <TabsContent value="adapter" className={styles.tierTabContent}>
            <AdapterOverridesPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} canDelete={canDelete} />
          </TabsContent>
          <TabsContent value="provider" className={styles.tierTabContent}>
            <ProviderOverridesPanel snapshot={snap} onChange={refetch} canEnable={canEmergencyEnable} canDelete={canDelete} />
          </TabsContent>
        </Tabs>
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
            {saving ? t('common:saving') : t('common:save')}
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
  const [deletingAdapter, setDeletingAdapter] = useState<string | null>(null);

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
      <Card padding="none">
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
                  <td>
                    <span className={tier.enabled ? styles.statusEnabled : styles.statusDisabled}>
                      {tier.enabled ? t('pages:passthrough.state.enabled') : t('pages:passthrough.state.disabled')}
                    </span>
                  </td>
                  <td>{bypassSummary(tier) || <span className={styles.empty}>—</span>}</td>
                  <td>{tier.enabled ? <Countdown expiresAt={tier.expiresAt} /> : '—'}</td>
                  <td>{tier.enabledBy ?? '—'}</td>
                  <td>
                    <RowActions>
                      <RowActionIconButton label={t('common:edit')} onAction={() => setEditing(adapter)}>
                        <EditActionIcon />
                      </RowActionIconButton>
                      <RowDeleteAction
                        label={t('common:delete')}
                        disabled={!canDelete}
                        onAction={() => setDeletingAdapter(adapter)}
                      />
                    </RowActions>
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
        <AlertDialog
          open={!!deletingAdapter}
          onOpenChange={(open) => { if (!open) setDeletingAdapter(null); }}
          title={t('common:deleteConfirmTitle')}
          description={t('pages:passthrough.adapter.deleteConfirm', { adapter: deletingAdapter ?? '' })}
          confirmLabel={t('common:delete')}
          cancelLabel={t('common:cancel')}
          variant="danger"
          onConfirm={() => {
            if (!deletingAdapter) return;
            void passthroughApi.deleteAdapter(deletingAdapter).then(() => {
              setDeletingAdapter(null);
              onChange();
            });
          }}
        />
      </Card>
    </section>
  );
}

function ProviderOverridesPanel({ snapshot, onChange, canEnable, canDelete }: { snapshot: PassthroughSnapshot; onChange: () => void; canEnable: boolean; canDelete: boolean }) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<string | null>(null);
  const [deletingProvider, setDeletingProvider] = useState<string | null>(null);

  const providers = Object.entries(snapshot.providers).sort(([a], [b]) => a.localeCompare(b));
  const { data: providersResp } = useApi<{ data: Provider[]; total: number }>(
    () => providerApi.list({ limit: '500' }),
    ['admin', 'providers', 'list', 'passthrough-table'],
  );
  const providerNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const p of providersResp?.data ?? []) {
      map.set(p.id, p.displayName?.trim() || p.name);
    }
    return map;
  }, [providersResp]);
  const providerName = (id: string) => snapshot.providerNames?.[id] ?? providerNameMap.get(id) ?? id.slice(0, 8) + '…';

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
      <Card padding="none">
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
                  <td>
                    <div className={styles.providerCell}>
                      <strong>{providerName(pid)}</strong>
                      <span className={styles.providerIdLine}>ID: {pid}</span>
                    </div>
                  </td>
                  <td>
                    <span className={tier.enabled ? styles.statusEnabled : styles.statusDisabled}>
                      {tier.enabled ? t('pages:passthrough.state.enabled') : t('pages:passthrough.state.disabled')}
                    </span>
                  </td>
                  <td>{bypassSummary(tier) || <span className={styles.empty}>—</span>}</td>
                  <td>{tier.enabled ? <Countdown expiresAt={tier.expiresAt} /> : '—'}</td>
                  <td>{tier.enabledBy ?? '—'}</td>
                  <td>
                    <RowActions>
                      <RowActionIconButton label={t('common:edit')} onAction={() => setEditing(pid)}>
                        <EditActionIcon />
                      </RowActionIconButton>
                      <RowDeleteAction
                        label={t('common:delete')}
                        disabled={!canDelete}
                        onAction={() => setDeletingProvider(pid)}
                      />
                    </RowActions>
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
        <AlertDialog
          open={!!deletingProvider}
          onOpenChange={(open) => { if (!open) setDeletingProvider(null); }}
          title={t('common:deleteConfirmTitle')}
          description={t('pages:passthrough.provider.deleteConfirm', { provider: deletingProvider ? providerName(deletingProvider) : '' })}
          confirmLabel={t('common:delete')}
          cancelLabel={t('common:cancel')}
          variant="danger"
          onConfirm={() => {
            if (!deletingProvider) return;
            void passthroughApi.deleteProvider(deletingProvider).then(() => {
              setDeletingProvider(null);
              onChange();
            });
          }}
        />
      </Card>
    </section>
  );
}
