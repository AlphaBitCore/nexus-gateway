/**
 * Provider detail Cache tab.
 *
 * Rewritten to use the three-tier API: reads effective config from
 * `/api/admin/cache/effective?provider_id=…`, writes per-field overrides via
 * `/api/admin/cache/provider/:id`, and renders per-field "Inherited vs
 * Overridden" badges so operators see at a glance which tier sources each
 * value. "Reset to default" removes a single field from the override (PUT
 * with the field omitted); the row remains intact so other overrides stay.
 */
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import {
  cacheApi,
  type CacheEffectiveResponse,
  type CacheProviderConfig,
  familyOf,
} from '@/api/services/system/cache';
import {
  Badge,
  Button,
  Card,
  ErrorBanner,
  FormField,
  Input,
  Skeleton,
  Stack,
  Switch,
} from '@/components/ui';
import styles from './ProviderCacheTab.module.css';

interface ProviderCacheTabProps {
  providerID: string;
  adapterType: string;
}

export function ProviderCacheTab({ providerID, adapterType }: ProviderCacheTabProps) {
  const { t } = useTranslation();
  const family = familyOf(adapterType);

  // No admin-tunable cache config for adapters outside the Anthropic / Gemini
  // families. Show an info card so the operator knows it's a fully provider-
  // managed cache path.
  if (family === 'none') {
    return (
      <Card>
        <Stack gap="sm">
          <h3 style={{ margin: 'var(--g-space-0)' }}>{t('pages:providers.cacheAutoTitle')}</h3>
          <p className={styles.cardSubtitle}>{t('pages:providers.cacheAutoInfo')}</p>
        </Stack>
      </Card>
    );
  }

  return <ProviderOverrideEditor providerID={providerID} adapterType={adapterType} family={family} />;
}

type Family = 'anthropic' | 'gemini';

function ProviderOverrideEditor({
  providerID, adapterType, family,
}: {
  providerID: string;
  adapterType: string;
  family: Family;
}) {
  const { t } = useTranslation();

  const { data: eff, loading, error, refetch } = useApi<CacheEffectiveResponse>(
    () => cacheApi.getEffective(providerID),
    ['admin', 'cache', 'effective', providerID],
  );
  const { data: override, refetch: refetchOverride } = useApi<CacheProviderConfig>(
    () => cacheApi.getProvider(providerID),
    ['admin', 'cache', 'provider', providerID],
  );

  // Edit buffer mirrors the persisted override row plus any in-flight
  // changes. `null` means "no edits — render from the fetched override
  // (or fall back to inherited values from `eff` for unset fields)".
  // This pattern avoids the prod-20260513c race where useState({}) +
  // useEffect would leave a render gap where draft was empty, and a fast
  // Save click during that gap PUT an empty body, wiping the override row.
  const [draft, setDraft] = useState<CacheProviderConfig | null>(null);
  // Reset draft when switching providers.
  useEffect(() => { setDraft(null); }, [providerID]);

  const editBuf: CacheProviderConfig = draft ?? override ?? {};

  const { mutate: save, loading: saving } = useMutation(
    // PUT the editBuf — the merged view of (persisted override + in-flight edits).
    // Per-field absence in editBuf means "no override at this field → inherit".
    () => cacheApi.putProvider(providerID, editBuf),
    {
      invalidateQueries: [
        ['admin', 'cache', 'effective', providerID],
        ['admin', 'cache', 'provider', providerID],
        ['admin', 'cache', 'overrides'],
      ],
      onSuccess: () => { setDraft(null); refetch(); refetchOverride(); },
      successMessage: t('pages:providers.cacheSaved'),
      errorMessage: t('pages:providers.cacheSaveError'),
    },
  );

  // Block rendering until BOTH eff and override fetches resolve, so the
  // Save button is never live against a stale or empty buffer. Override
  // returns `{}` for providers with no Tier-3 row — that is loaded state,
  // not absent state, so we accept it once `override !== undefined`.
  if (loading || !eff || override === undefined) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const isOverridden = (field: keyof CacheProviderConfig) =>
    editBuf[field] !== undefined && editBuf[field] !== null;

  // setField writes into the draft buffer. The draft starts from a clone
  // of the persisted override so toggling an inherited-default field
  // produces a complete-row PUT, not a partial body that drops other
  // fields (this is what caused the prod-20260513c data loss incident).
  const setField = <K extends keyof CacheProviderConfig>(key: K, value: CacheProviderConfig[K]) =>
    setDraft(prev => ({ ...(prev ?? override ?? {}), [key]: value }));

  // resetField removes the field from the draft, reverting that knob to
  // its inherited value. If draft is null and the field was already in
  // override, we have to seed the draft from override and then delete the
  // field — otherwise the field would stay in override at Save time.
  const resetField = (key: keyof CacheProviderConfig) =>
    setDraft(prev => {
      const next = { ...(prev ?? override ?? {}) };
      delete next[key];
      return next;
    });

  return (
    <Stack gap="md">
      <Card>
        <Stack gap="sm">
          <h3 style={{ margin: 'var(--g-space-0)' }}>
            {family === 'anthropic' ? t('pages:providers.cacheTitle') : t('pages:providers.cacheGeminiTitle')}
          </h3>
          <p className={styles.cardSubtitle}>
            {family === 'anthropic'
              ? t('pages:providers.cacheAnthropicSubtitle')
              : t('pages:providers.cacheGeminiInfo')}
          </p>
        </Stack>
      </Card>

      {family === 'anthropic' && (
        <Card>
          <Stack gap="md">
            <BoolField
              label={t('pages:providers.cacheInjectEnabled')}
              helpText={t('pages:providers.cacheInjectEnabledHint')}
              fieldKey="marker_inject_enabled"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
            <BoolField
              label={t('pages:providers.cacheBoundary3Enabled')}
              helpText={t('pages:providers.cacheBoundary3EnabledHint')}
              fieldKey="marker_boundary3_enabled"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
          </Stack>
        </Card>
      )}

      {family === 'gemini' && (
        <Card>
          <Stack gap="md">
            <BoolField
              label={t('pages:providers.cacheGeminiEnabled')}
              helpText={t('pages:providers.cacheGeminiEnabledHint')}
              fieldKey="cache_enabled"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
            <NumberOverrideField
              label={t('pages:providers.cacheGeminiMinSystemChars')}
              helpText={t('pages:providers.cacheGeminiMinSystemCharsHint')}
              fieldKey="min_system_chars"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
            <NumberOverrideField
              label={t('pages:providers.cacheGeminiTtlSeconds')}
              helpText={t('pages:providers.cacheGeminiTtlSecondsHint')}
              fieldKey="ttl_seconds"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
            <NumberOverrideField
              label={t('pages:providers.cacheGeminiCircuitThreshold')}
              helpText={t('pages:providers.cacheGeminiCircuitThresholdHint')}
              fieldKey="circuit_breaker_threshold"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
            <NumberOverrideField
              label={t('pages:providers.cacheGeminiCircuitOpenSecs')}
              helpText={t('pages:providers.cacheGeminiCircuitOpenSecsHint')}
              fieldKey="circuit_breaker_open_secs"
              draft={editBuf}
              eff={eff}
              setField={setField}
              resetField={resetField}
              isOverridden={isOverridden}
            />
          </Stack>
        </Card>
      )}

      <Stack direction="horizontal" gap="sm">
        <Button onClick={() => save(undefined)} disabled={saving}>
          {saving ? t('common:saving') : t('common:save')}
        </Button>
      </Stack>
    </Stack>
  );
}

// ── Field components ─────────────────────────────────────────────────────

function SourceBadge({ source }: { source?: string }) {
  const { t } = useTranslation();
  switch (source) {
    case 'provider-override':
      return <Badge variant="warning">{t('pages:providers.sourceOverride')}</Badge>;
    case 'adapter-default':
      return <Badge variant="default">{t('pages:providers.sourceAdapter')}</Badge>;
    case 'global-default':
      return <Badge variant="default">{t('pages:providers.sourceGlobal')}</Badge>;
    default:
      return <Badge variant="default">{t('pages:providers.sourceCode')}</Badge>;
  }
}

interface FieldProps<K extends keyof CacheProviderConfig> {
  label: string;
  helpText: string;
  fieldKey: K;
  draft: CacheProviderConfig;
  eff: CacheEffectiveResponse;
  setField: <KK extends keyof CacheProviderConfig>(k: KK, v: CacheProviderConfig[KK]) => void;
  resetField: (k: keyof CacheProviderConfig) => void;
  isOverridden: (k: keyof CacheProviderConfig) => boolean;
}

function BoolField<K extends keyof CacheProviderConfig>(p: FieldProps<K>) {
  const { t } = useTranslation();
  const source = p.eff.sources[p.fieldKey as string];
  const overridden = p.isOverridden(p.fieldKey);
  const draftVal = p.draft[p.fieldKey] as boolean | undefined;
  const effectiveVal = p.eff.effective[p.fieldKey as string] as boolean | undefined;
  const visibleVal = overridden ? !!draftVal : !!effectiveVal;

  return (
    <FormField label={p.label} helpText={p.helpText}>
      <div className={styles.fieldRow}>
        <Switch
          checked={visibleVal}
          onCheckedChange={(v) => p.setField(p.fieldKey, v as CacheProviderConfig[K])}
        />
        <SourceBadge source={overridden ? 'provider-override' : source} />
        {overridden && (
          <Button variant="ghost" onClick={() => p.resetField(p.fieldKey)}>
            {t('pages:providers.cacheResetToDefault')}
          </Button>
        )}
      </div>
    </FormField>
  );
}

function NumberOverrideField<K extends keyof CacheProviderConfig>(p: FieldProps<K>) {
  const { t } = useTranslation();
  const source = p.eff.sources[p.fieldKey as string];
  const overridden = p.isOverridden(p.fieldKey);
  const draftVal = p.draft[p.fieldKey] as number | undefined;
  const effectiveVal = p.eff.effective[p.fieldKey as string] as number | undefined;
  const visibleVal = overridden ? draftVal ?? 0 : effectiveVal ?? 0;

  return (
    <FormField label={p.label} helpText={p.helpText}>
      <div className={styles.fieldRow}>
        <Input
          type="number"
          min={0}
          value={visibleVal}
          onChange={(e) => p.setField(p.fieldKey, Number(e.target.value) as CacheProviderConfig[K])}
        />
        <SourceBadge source={overridden ? 'provider-override' : source} />
        {overridden && (
          <Button variant="ghost" onClick={() => p.resetField(p.fieldKey)}>
            {t('pages:providers.cacheResetToDefault')}
          </Button>
        )}
      </div>
    </FormField>
  );
}
