/**
 * Prompt Cache 3-Tier Config global page.
 *
 * Page layout:
 *   1. Global Defaults panel        — Tier 1 (normaliser_enabled + cache_master_kill_switch).
 *   2. Adapter Defaults panel       — Tier 2 knobs for the 4 adapter families that have
 *                                     family-specific cache configuration (anthropic, bedrock,
 *                                     gemini, vertex). OpenAI-compat adapters have no
 *                                     adapter-level knobs, so they are intentionally absent
 *                                     from this panel's tab strip.
 *   3. Normalisation rules panel    — flat table of every bundled rule across every adapter
 *                                     family, grouped by adapter, with toggles. Source of truth
 *                                     for `BUNDLED_RULES` is `packages/shared/normaliser/bundled.go`.
 *   4. Active Overrides panel       — Tier-3 listing.
 */
import { Fragment, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import {
  cacheApi,
  type CacheAdapterConfig,
  type CacheAdaptersList,
  type CacheGlobalConfig,
  type CacheOverrideRow,
  type CacheOverridesList,
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
import styles from './SettingsCacheTab.module.css';

/**
 * Bundled rule metadata, mirroring `packages/shared/normaliser/bundled.go`
 * one-for-one. Order in this array is the order rules appear on screen.
 * The DB only stores admin overrides (enabled, dry_run_always) keyed by rule_id;
 * regex / body_path / default-enabled live in Go.
 */
const BUNDLED_RULES: {
  id: string;
  adapterType: string;
  defaultEnabled: boolean;
  keyNormalizeSafe: boolean;
  /** i18n key under `pages:settings.promptCache.ruleDesc.*`. */
  descriptionKey: string;
}[] = [
  // Anthropic family
  { id: 'claude-code-cch-strip',           adapterType: 'anthropic',    defaultEnabled: false, keyNormalizeSafe: true, descriptionKey: 'claudeCodeCchStrip' },
  { id: 'bedrock-claude-cch-strip',        adapterType: 'bedrock',      defaultEnabled: false, keyNormalizeSafe: true, descriptionKey: 'bedrockClaudeCchStrip' },
  // OpenAI-compatible family — field-order canonicalisation, one rule per adapter
  { id: 'openai-field-order-normalize',         adapterType: 'openai',       defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'openaiFieldOrder' },
  { id: 'azure-openai-field-order-normalize',   adapterType: 'azure-openai', defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'azureOpenaiFieldOrder' },
  { id: 'deepseek-field-order-normalize',       adapterType: 'deepseek',     defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'deepseekFieldOrder' },
  { id: 'glm-field-order-normalize',            adapterType: 'glm',          defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'glmFieldOrder' },
  { id: 'moonshot-field-order-normalize',       adapterType: 'moonshot',     defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'moonshotFieldOrder' },
  { id: 'mistral-field-order-normalize',        adapterType: 'mistral',      defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'mistralFieldOrder' },
  { id: 'xai-field-order-normalize',            adapterType: 'xai',          defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'xaiFieldOrder' },
  { id: 'groq-field-order-normalize',           adapterType: 'groq',         defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'groqFieldOrder' },
  { id: 'perplexity-field-order-normalize',     adapterType: 'perplexity',   defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'perplexityFieldOrder' },
  { id: 'together-field-order-normalize',       adapterType: 'together',     defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'togetherFieldOrder' },
  { id: 'fireworks-field-order-normalize',      adapterType: 'fireworks',    defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'fireworksFieldOrder' },
  { id: 'minimax-field-order-normalize',        adapterType: 'minimax',      defaultEnabled: true, keyNormalizeSafe: true, descriptionKey: 'minimaxFieldOrder' },
];

/**
 * Display order of adapter groups in the rules panel. Anthropic family first
 * (it has the most operationally-interesting rules — defaults are off, so
 * admins look here first), then OpenAI-compat alphabetised.
 */
const ADAPTER_GROUP_ORDER: string[] = [
  'anthropic',
  'bedrock',
  'openai',
  'azure-openai',
  'deepseek',
  'fireworks',
  'glm',
  'groq',
  'minimax',
  'mistral',
  'moonshot',
  'perplexity',
  'together',
  'xai',
];

/** Adapter tabs for the Tier-2 knob panel. Only families with cache-specific knobs. */
const ADAPTER_TABS: { type: string; labelKey: string }[] = [
  { type: 'anthropic', labelKey: 'pages:settings.promptCache.adapterAnthropic' },
  { type: 'bedrock', labelKey: 'pages:settings.promptCache.adapterBedrock' },
  { type: 'gemini', labelKey: 'pages:settings.promptCache.adapterGemini' },
  { type: 'vertex', labelKey: 'pages:settings.promptCache.adapterVertex' },
];

export function SettingsCacheTab() {
  return (
    <Stack gap="lg">
      <GlobalPanel />
      <AdapterPanel />
      <NormalisationRulesPanel />
      <OverridesPanel />
    </Stack>
  );
}

function GlobalPanel() {
  const { t } = useTranslation();
  const [normaliserEnabled, setNormaliserEnabled] = useState(false);
  const [killSwitch, setKillSwitch] = useState(false);

  const { data, loading, error, refetch } = useApi<CacheGlobalConfig>(
    () => cacheApi.getGlobal(),
    ['admin', 'cache', 'global'],
  );

  useEffect(() => {
    if (!data) return;
    setNormaliserEnabled(!!data.normaliser_enabled);
    setKillSwitch(!!data.cache_master_kill_switch);
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () =>
      cacheApi.putGlobal({
        normaliser_enabled: normaliserEnabled,
        cache_master_kill_switch: killSwitch,
      }),
    {
      invalidateQueries: [['admin', 'cache', 'global']],
      onSuccess: () => refetch(),
      successMessage: t('pages:settings.promptCache.saved'),
      errorMessage: t('pages:settings.promptCache.saveError'),
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.globalTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.globalSubtitle')}</p>

        <FormField
          label={t('pages:settings.promptCache.normaliserEnabled')}
          helpText={t('pages:settings.promptCache.normaliserEnabledHint')}
        >
          <Switch checked={normaliserEnabled} onCheckedChange={setNormaliserEnabled} />
        </FormField>

        <FormField
          label={t('pages:settings.promptCache.killSwitch')}
          helpText={t('pages:settings.promptCache.killSwitchHint')}
        >
          <Switch checked={killSwitch} onCheckedChange={setKillSwitch} />
        </FormField>

        <Stack direction="horizontal" gap="sm">
          <Button onClick={() => save(undefined)} disabled={saving}>
            {saving ? t('common:saving') : t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Card>
  );
}

function AdapterPanel() {
  const { t } = useTranslation();
  const [activeAdapter, setActiveAdapter] = useState<string>('anthropic');

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.adapterTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.adapterSubtitle')}</p>

        <Stack direction="horizontal" gap="sm">
          {ADAPTER_TABS.map(tab => (
            <Button
              key={tab.type}
              onClick={() => setActiveAdapter(tab.type)}
              variant={activeAdapter === tab.type ? 'primary' : 'secondary'}
            >
              {t(tab.labelKey)}
            </Button>
          ))}
        </Stack>

        <AdapterConfigCard adapterType={activeAdapter} />
      </Stack>
    </Card>
  );
}

function AdapterConfigCard({ adapterType }: { adapterType: string }) {
  const { t } = useTranslation();
  const family = familyOf(adapterType);

  const { data, loading, error, refetch } = useApi<CacheAdapterConfig>(
    () => cacheApi.getAdapter(adapterType),
    ['admin', 'cache', 'adapter', adapterType],
  );

  // Local draft of changes the user has made since the last save. NULL means
  // "no edits — display values come from `data`". This pattern avoids the
  // prod-20260513c race where useState({}) + useEffect left a single render
  // where cfg was empty before data populated, and a fast Save click during
  // that gap wiped the row.
  const [draft, setDraft] = useState<Partial<CacheAdapterConfig> | null>(null);
  useEffect(() => { setDraft(null); }, [adapterType]);

  const cfg: CacheAdapterConfig = { ...(data ?? {}), ...(draft ?? {}) };

  const set = <K extends keyof CacheAdapterConfig>(key: K, value: CacheAdapterConfig[K]) =>
    setDraft(prev => ({ ...(prev ?? {}), [key]: value }));

  const { mutate: save, loading: saving } = useMutation(
    // Always send the merged body (data + draft); preserve any rules row
    // owned by NormalisationRulesPanel so saving knobs here does not wipe
    // them.
    () => cacheApi.putAdapter(adapterType, { ...cfg, rules: data?.rules }),
    {
      invalidateQueries: [['admin', 'cache', 'adapter', adapterType], ['admin', 'cache', 'adapters', 'list']],
      onSuccess: () => { setDraft(null); refetch(); },
      successMessage: t('pages:settings.promptCache.saved'),
      errorMessage: t('pages:settings.promptCache.saveError'),
    },
  );

  // Block rendering until data has loaded, so the Save button is never active
  // against an empty body. Without this, a fast user click during the data
  // fetch could PUT an empty config and wipe the adapter row.
  if (loading || !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Card>
      <Stack gap="md">
        <h3 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.adapterDefaultsTitle', { adapter: adapterType })}</h3>

        {family === 'anthropic' && (
          <>
            <FormField label={t('pages:providers.cacheInjectEnabled')} helpText={t('pages:providers.cacheInjectEnabledHint')}>
              <Switch checked={!!cfg.marker_inject_enabled} onCheckedChange={v => set('marker_inject_enabled', v)} />
            </FormField>
            <FormField label={t('pages:providers.cacheBoundary3Enabled')} helpText={t('pages:providers.cacheBoundary3EnabledHint')}>
              <Switch
                checked={!!cfg.marker_boundary3_enabled}
                onCheckedChange={v => set('marker_boundary3_enabled', v)}
                disabled={!cfg.marker_inject_enabled}
              />
            </FormField>
          </>
        )}

        {family === 'gemini' && (
          <>
            <FormField label={t('pages:providers.cacheGeminiEnabled')} helpText={t('pages:providers.cacheGeminiEnabledHint')}>
              <Switch checked={!!cfg.cache_enabled} onCheckedChange={v => set('cache_enabled', v)} />
            </FormField>
            <NumberField
              label={t('pages:providers.cacheGeminiMinSystemChars')}
              helpText={t('pages:providers.cacheGeminiMinSystemCharsHint')}
              value={cfg.min_system_chars ?? 4096}
              onChange={v => set('min_system_chars', v)}
              disabled={!cfg.cache_enabled}
            />
            <NumberField
              label={t('pages:providers.cacheGeminiTtlSeconds')}
              helpText={t('pages:providers.cacheGeminiTtlSecondsHint')}
              value={cfg.ttl_seconds ?? 3600}
              onChange={v => set('ttl_seconds', v)}
              disabled={!cfg.cache_enabled}
            />
            <NumberField
              label={t('pages:providers.cacheGeminiCircuitThreshold')}
              helpText={t('pages:providers.cacheGeminiCircuitThresholdHint')}
              value={cfg.circuit_breaker_threshold ?? 5}
              onChange={v => set('circuit_breaker_threshold', v)}
              disabled={!cfg.cache_enabled}
            />
            <NumberField
              label={t('pages:providers.cacheGeminiCircuitOpenSecs')}
              helpText={t('pages:providers.cacheGeminiCircuitOpenSecsHint')}
              value={cfg.circuit_breaker_open_secs ?? 300}
              onChange={v => set('circuit_breaker_open_secs', v)}
              disabled={!cfg.cache_enabled}
            />
          </>
        )}

        <Stack direction="horizontal" gap="sm">
          <Button onClick={() => save(undefined)} disabled={saving}>
            {saving ? t('common:saving') : t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Card>
  );
}

/**
 * Lists every bundled normalisation rule across every adapter family in one
 * flat table, grouped by adapter. Reads all Tier-2 rows in a single batch
 * call and writes per-rule toggles back through the per-adapter PUT route.
 */
function NormalisationRulesPanel() {
  const { t } = useTranslation();
  const [query, setQuery] = useState('');

  const { data, loading, error, refetch } = useApi<CacheAdaptersList>(
    () => cacheApi.listAdapters(),
    ['admin', 'cache', 'adapters', 'list'],
  );

  const rowsByAdapter = useMemo(() => {
    const q = query.trim().toLowerCase();
    const filtered = q
      ? BUNDLED_RULES.filter(r => r.id.toLowerCase().includes(q) || r.adapterType.toLowerCase().includes(q))
      : BUNDLED_RULES;
    const grouped: Record<string, typeof BUNDLED_RULES> = {};
    for (const r of filtered) {
      (grouped[r.adapterType] ||= []).push(r);
    }
    return grouped;
  }, [query]);

  const adaptersInOrder = ADAPTER_GROUP_ORDER.filter(a => rowsByAdapter[a]?.length);

  const setRule = async (adapterType: string, ruleId: string, enabled: boolean) => {
    // Merge into the existing adapter row so unrelated knobs are not wiped.
    const existing = data?.items?.[adapterType] ?? {};
    const overrides = existing.rules ?? {};
    const nextRules = { ...overrides, [ruleId]: { ...overrides[ruleId], enabled } };
    await cacheApi.putAdapter(adapterType, { ...existing, rules: nextRules });
    refetch();
  };

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.rulesTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.rulesSubtitle')}</p>

        <div className={styles.rulesFilterBar}>
          <Input
            className={styles.rulesFilterInput}
            placeholder={t('pages:settings.promptCache.rulesFilterPlaceholder')}
            value={query}
            onChange={e => setQuery(e.target.value)}
          />
          <span className={styles.subtitle}>
            {t('pages:settings.promptCache.rulesCount', {
              shown: Object.values(rowsByAdapter).reduce((n, list) => n + list.length, 0),
              total: BUNDLED_RULES.length,
            })}
          </span>
        </div>

        <table className={styles.rulesTable}>
          <colgroup>
            <col className={styles.colRuleId} />
            <col className={styles.colKeySafe} />
            <col className={styles.colStatus} />
          </colgroup>
          <thead>
            <tr>
              <th>{t('pages:settings.promptCache.ruleId')}</th>
              <th>{t('pages:settings.promptCache.ruleKeySafe')}</th>
              <th>{t('pages:settings.promptCache.ruleStatus')}</th>
            </tr>
          </thead>
          <tbody>
            {adaptersInOrder.map(adapter => {
              const rules = rowsByAdapter[adapter];
              const adapterRow = data?.items?.[adapter];
              const overrides = adapterRow?.rules ?? {};
              return (
                <Fragment key={adapter}>
                  <tr className={styles.adapterGroup}>
                    <td colSpan={3}>{t(`pages:settings.promptCache.adapterGroup.${adapter}`, adapter)}</td>
                  </tr>
                  {rules.map(rule => {
                    const ovr = overrides[rule.id]?.enabled;
                    const effective = ovr ?? rule.defaultEnabled;
                    return (
                      <tr key={rule.id}>
                        <td>
                          <code>{rule.id}</code>
                          <div className={styles.ruleDescription}>
                            {t(`pages:settings.promptCache.ruleDesc.${rule.descriptionKey}`)}
                          </div>
                        </td>
                        <td>
                          {rule.keyNormalizeSafe
                            ? <Badge variant="success">{t('common:yes')}</Badge>
                            : <Badge variant="default">{t('common:no')}</Badge>}
                        </td>
                        <td>
                          <Switch checked={effective} onCheckedChange={v => setRule(rule.adapterType, rule.id, v)} />
                        </td>
                      </tr>
                    );
                  })}
                </Fragment>
              );
            })}
            {adaptersInOrder.length === 0 && (
              <tr>
                <td colSpan={3} className={styles.empty}>
                  {t('pages:settings.promptCache.rulesNoMatch')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </Stack>
    </Card>
  );
}

function OverridesPanel() {
  const { t } = useTranslation();
  const { data, loading, error, refetch } = useApi<CacheOverridesList>(
    () => cacheApi.listOverrides(),
    ['admin', 'cache', 'overrides'],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const items = data?.items ?? [];

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.overridesTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.overridesSubtitle')}</p>

        {items.length === 0 ? (
          <p className={styles.empty}>{t('pages:settings.promptCache.overridesEmpty')}</p>
        ) : (
          <table className={styles.overridesTable}>
            <thead>
              <tr>
                <th>{t('pages:settings.promptCache.overrideProvider')}</th>
                <th>{t('pages:settings.promptCache.overrideAdapter')}</th>
                <th>{t('pages:settings.promptCache.overrideFields')}</th>
                <th>{t('pages:settings.promptCache.overrideActions')}</th>
              </tr>
            </thead>
            <tbody>
              {items.map(row => <OverrideRowView key={row.provider_id} row={row} />)}
            </tbody>
          </table>
        )}
      </Stack>
    </Card>
  );
}

function OverrideRowView({ row }: { row: CacheOverrideRow }) {
  const { t } = useTranslation();
  return (
    <tr>
      <td><strong>{row.provider_name}</strong> <code>{row.provider_id.slice(0, 8)}…</code></td>
      <td><Badge variant="default">{row.adapter_type}</Badge></td>
      <td>
        <ul className={styles.diffList}>
          {row.overridden_keys.map(key => {
            const d = row.diff[key];
            return (
              <li key={key}>
                <code>{key}</code>: <span className={styles.diffInherited}>{String(d.inherited)}</span> → <strong>{String(d.override)}</strong>
              </li>
            );
          })}
        </ul>
      </td>
      <td>
        <Link to={`/ai-gateway/providers/${row.provider_id}`}>{t('pages:settings.promptCache.openProvider')}</Link>
      </td>
    </tr>
  );
}

function NumberField({
  label, helpText, value, onChange, disabled,
}: {
  label: string;
  helpText: string;
  value: number;
  onChange: (v: number) => void;
  disabled?: boolean;
}) {
  return (
    <FormField label={label} helpText={helpText}>
      <Input
        type="number"
        min={0}
        value={value}
        disabled={disabled}
        onChange={e => onChange(Number(e.target.value))}
      />
    </FormField>
  );
}
