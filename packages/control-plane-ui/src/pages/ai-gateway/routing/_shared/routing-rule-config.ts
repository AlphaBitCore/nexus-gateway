/**
 * Maps dashboard form state to gateway routing rule JSON (strategy tree) and back.
 */

import type { AdminModelsByProvider } from '@/api/types';

export type StrategyType =
  | 'single'
  | 'fallback'
  | 'loadbalance'
  | 'conditional'
  | 'ab_split'
  | 'smart'
  | 'policy';

/**
 * The user-selectable routing strategy types, in canonical display order. This is
 * the single source of truth for every strategy picker and filter in the UI (the
 * Strategy Type dropdown on create/edit and the strategy filter on the list page),
 * so they never drift. `policy` is intentionally excluded: it is the stage-0
 * pipeline type, chosen via the pipeline-stage control, not a routing strategy.
 */
export const STRATEGY_TYPES: readonly Exclude<StrategyType, 'policy'>[] = [
  'single',
  'fallback',
  'loadbalance',
  'conditional',
  'ab_split',
  'smart',
];

export interface ProviderModelEntry {
  provider: string;
  model: string;
  weight: string;
}

export function mapLegacyStrategy(s: string): StrategyType {
  const map: Record<string, StrategyType> = {
    priority: 'single',
    'round-robin': 'loadbalance',
    weighted: 'loadbalance',
    latency: 'single',
    cost: 'single',
    fallback: 'fallback',
    single: 'single',
    loadbalance: 'loadbalance',
    conditional: 'conditional',
    ab_split: 'ab_split',
    smart: 'smart',
    policy: 'policy',
  };
  return map[s] ?? 'single';
}

/** Split comma- or whitespace-separated gateway IDs from a text field. */
export function splitIdsCsv(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/** Discrete model types the gateway's routing engine knows about
 *  (matcher.go: ctx.RequestedModel.Type comparison). Keep in lockstep
 *  with the Go side; if a new type ships there, surface it here. */
export const MODEL_TYPE_OPTIONS = ['chat', 'embedding', 'image', 'audio'] as const;
export type ModelType = (typeof MODEL_TYPE_OPTIONS)[number];

export interface MatchConditionsFormState {
  models: string[];
  /** Free-text request-side model keywords (e.g. "auto", "gpt-4-*").
   *  Matched against the raw `model` string in the incoming request before
   *  the gateway hydrates it to a Model.id UUID. */
  requestedModelLiterals: string[];
  /** Discrete categories — chat / embedding / image / audio. Compared
   *  against the resolved Model.type (ctx.RequestedModel.Type). */
  modelTypes: string[];
  providers: string[];
  projects: string[];
  /** Glob patterns matched against the active virtual key's `name`. */
  virtualKeys: string[];
}

function emptyMatchConditionsFormState(): MatchConditionsFormState {
  return {
    models: [],
    requestedModelLiterals: [],
    modelTypes: [],
    providers: [],
    projects: [],
    virtualKeys: [],
  };
}

export function parseMatchConditionsForm(mc: unknown): MatchConditionsFormState {
  if (!mc || typeof mc !== 'object') return emptyMatchConditionsFormState();
  const m = mc as Record<string, unknown>;
  return {
    models: Array.isArray(m.models) ? m.models.map(String) : [],
    requestedModelLiterals: Array.isArray(m.requestedModelLiterals)
      ? m.requestedModelLiterals.map(String)
      : [],
    modelTypes: Array.isArray(m.modelTypes) ? m.modelTypes.map(String) : [],
    providers: Array.isArray(m.providers) ? m.providers.map(String) : [],
    projects: Array.isArray(m.projects) ? m.projects.map(String) : [],
    virtualKeys: Array.isArray(m.virtualKeys) ? m.virtualKeys.map(String) : [],
  };
}

/** Build API `matchConditions`; omit empty arrays. */
export function buildMatchConditionsPayload(state: MatchConditionsFormState): Record<string, unknown> {
  return {
    ...(state.models.length > 0 && { models: state.models }),
    ...(state.requestedModelLiterals.length > 0 && {
      requestedModelLiterals: state.requestedModelLiterals,
    }),
    ...(state.modelTypes.length > 0 && { modelTypes: state.modelTypes }),
    ...(state.providers.length > 0 && { providers: state.providers }),
    ...(state.projects.length > 0 && { projects: state.projects }),
    ...(state.virtualKeys.length > 0 && { virtualKeys: state.virtualKeys }),
  };
}

export function policyConfigToFormLines(config: unknown): {
  allowM: string[];
  denyM: string[];
  allowP: string[];
  denyP: string[];
} {
  const o = config as Record<string, unknown>;
  if (!o || o.type !== 'policy') return { allowM: [], denyM: [], allowP: [], denyP: [] };
  const arr = (a: unknown) => (Array.isArray(a) ? (a as string[]) : []);
  return {
    allowM: arr(o.allowModelIds),
    denyM: arr(o.denyModelIds),
    allowP: arr(o.allowProviderIds),
    denyP: arr(o.denyProviderIds),
  };
}

export function buildPolicyApiConfig(
  allowM: string[],
  denyM: string[],
  allowP: string[],
  denyP: string[],
): { ok: true; config: Record<string, unknown> } | { ok: false; message: string } {
  const allowModelIds = allowM.filter(s => s.length > 0);
  const denyModelIds = denyM.filter(s => s.length > 0);
  const allowProviderIds = allowP.filter(s => s.length > 0);
  const denyProviderIds = denyP.filter(s => s.length > 0);
  const has =
    allowModelIds.length > 0 ||
    denyModelIds.length > 0 ||
    allowProviderIds.length > 0 ||
    denyProviderIds.length > 0;
  if (!has) return { ok: false, message: 'Add at least one allow or deny model or provider ID.' };
  const config: Record<string, unknown> = { type: 'policy' };
  if (allowModelIds.length > 0) config.allowModelIds = allowModelIds;
  if (denyModelIds.length > 0) config.denyModelIds = denyModelIds;
  if (allowProviderIds.length > 0) config.allowProviderIds = allowProviderIds;
  if (denyProviderIds.length > 0) config.denyProviderIds = denyProviderIds;
  return { ok: true, config };
}

export function resolveProviderModelIds(
  groups: AdminModelsByProvider[],
  providerName: string,
  providerModelId: string,
): { providerId: string; modelId: string } | null {
  const g = groups.find((x) => x.provider?.name === providerName);
  if (!g) return null;
  const m = g?.models?.find((mod) => mod.providerModelId === providerModelId);
  if (!m) return null;
  return { providerId: g.provider?.id, modelId: m.id };
}

function uiFromApiIds(
  groups: AdminModelsByProvider[],
  providerId: string,
  modelId: string,
): { provider: string; model: string } | null {
  const g = groups.find((x) => x.provider?.id === providerId);
  const m = g?.models.find((mod) => mod.id === modelId);
  if (!g || !m) return null;
  return { provider: g.provider?.name, model: m.providerModelId };
}

function findPlanByModelId(
  groups: AdminModelsByProvider[],
  modelId: string,
): { providerId: string; modelId: string } | null {
  for (const g of groups) {
    const m = g?.models?.find((mod) => mod.id === modelId);
    if (m) return { providerId: g.provider?.id, modelId: m.id };
  }
  return null;
}

function findFirstEnabledPlan(groups: AdminModelsByProvider[]): { providerId: string; modelId: string } | null {
  for (const g of groups) {
    const m = g?.models?.find((mod) => mod.enabled);
    if (m) return { providerId: g.provider?.id, modelId: m.id };
  }
  return null;
}

export function isValidConditionalConfig(c: unknown): boolean {
  if (!c || typeof c !== 'object') return false;
  const o = c as Record<string, unknown>;
  if (o.type !== 'conditional') return false;
  if (!Array.isArray(o.conditions)) return false;
  if (!o.default || typeof o.default !== 'object') return false;
  return true;
}

/** Field paths commonly used in conditional `when` clauses (dot notation on routing context). */
export const CONDITIONAL_FIELD_PATH_PRESETS: readonly string[] = [
  'virtualKey.projectId',
  'virtualKey.name',
  'virtualKey.organizationId',
  'virtualKey.id',
  'virtualKey.sourceApp',
  'requestedModel.id',
  'endpointType',
];

export type ConditionalWhenOperator =
  | '$eq'
  | '$ne'
  | '$gt'
  | '$gte'
  | '$lt'
  | '$lte'
  | '$in'
  | '$nin'
  | '$regex';

export interface ConditionalBranchFormRow {
  id: string;
  fieldPath: string;
  operator: ConditionalWhenOperator;
  value: string;
  thenProvider: string;
  thenModel: string;
}

export interface ConditionalFormState {
  defaultProvider: string;
  defaultModel: string;
  branches: ConditionalBranchFormRow[];
}

export type ConditionalEditorHydration =
  | { mode: 'form'; form: ConditionalFormState }
  | { mode: 'json'; text: string };

const CONDITIONAL_WHEN_OPERATORS: readonly ConditionalWhenOperator[] = [
  '$eq',
  '$ne',
  '$gt',
  '$gte',
  '$lt',
  '$lte',
  '$in',
  '$nin',
  '$regex',
];

function newBranchId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `br-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`;
}

export function newConditionalBranchRow(): ConditionalBranchFormRow {
  return {
    id: newBranchId(),
    fieldPath: '',
    operator: '$eq',
    value: '',
    thenProvider: '',
    thenModel: '',
  };
}

export function emptyConditionalFormState(): ConditionalFormState {
  return {
    defaultProvider: '',
    defaultModel: '',
    branches: [newConditionalBranchRow()],
  };
}

function parseSimpleWhenClause(
  when: unknown,
): { fieldPath: string; operator: ConditionalWhenOperator; value: string } | null {
  if (!when || typeof when !== 'object' || Array.isArray(when)) return null;
  const o = when as Record<string, unknown>;
  const keys = Object.keys(o).filter((k) => k !== '$and' && k !== '$or');
  if (keys.length !== 1) return null;
  const path = keys[0];
  const v = o[path];

  if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
    const opKeys = Object.keys(v as object).filter((k) => k.startsWith('$'));
    if (opKeys.length !== 1) return null;
    const op = opKeys[0] as ConditionalWhenOperator;
    if (!CONDITIONAL_WHEN_OPERATORS.includes(op)) return null;
    const raw = (v as Record<string, unknown>)[op];
    if (op === '$in' || op === '$nin') {
      if (!Array.isArray(raw)) return null;
      return { fieldPath: path, operator: op, value: raw.map(String).join(', ') };
    }
    if (raw === null || raw === undefined) return { fieldPath: path, operator: op, value: '' };
    if (typeof raw === 'boolean' || typeof raw === 'number') {
      return { fieldPath: path, operator: op, value: String(raw) };
    }
    if (typeof raw === 'string') return { fieldPath: path, operator: op, value: raw };
    return null;
  }

  if (v === null || v === undefined) return { fieldPath: path, operator: '$eq', value: '' };
  if (typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean') {
    return { fieldPath: path, operator: '$eq', value: String(v) };
  }
  return null;
}

/**
 * Parse a conditional strategy tree into form state when every branch uses a single-field `when`
 * and `default` / each `then` is a `single` node with known provider+model IDs.
 */
export function tryParseConditionalFormFromConfig(
  config: unknown,
  groups: AdminModelsByProvider[],
): ConditionalFormState | null {
  if (!isValidConditionalConfig(config)) return null;
  const cfg = config as Record<string, unknown>;
  const defNode = cfg.default as Record<string, unknown>;
  if (defNode.type !== 'single') return null;
  if (typeof defNode.providerId !== 'string' || typeof defNode.modelId !== 'string') return null;
  const defUi = uiFromApiIds(groups, defNode.providerId, defNode.modelId);
  if (!defUi) return null;

  const branches: ConditionalBranchFormRow[] = [];
  for (const raw of cfg.conditions as unknown[]) {
    if (!raw || typeof raw !== 'object') return null;
    const br = raw as Record<string, unknown>;
    const whenParsed = parseSimpleWhenClause(br.when);
    if (!whenParsed) return null;
    const then = br.then as Record<string, unknown> | undefined;
    if (!then || then.type !== 'single') return null;
    if (typeof then.providerId !== 'string' || typeof then.modelId !== 'string') return null;
    const thenUi = uiFromApiIds(groups, then.providerId, then.modelId);
    if (!thenUi) return null;
    branches.push({
      id: newBranchId(),
      fieldPath: whenParsed.fieldPath,
      operator: whenParsed.operator,
      value: whenParsed.value,
      thenProvider: thenUi.provider,
      thenModel: thenUi.model,
    });
  }

  return {
    defaultProvider: defUi.provider,
    defaultModel: defUi.model,
    branches: branches.length > 0 ? branches : [newConditionalBranchRow()],
  };
}

function coerceWhenOperand(operator: ConditionalWhenOperator, raw: string): unknown {
  const t = raw.trim();
  if (operator === '$in' || operator === '$nin') {
    const parts = splitIdsCsv(t);
    return parts.map((s) => {
      if (s === 'true') return true;
      if (s === 'false') return false;
      const n = Number(s);
      if (s !== '' && Number.isFinite(n) && String(n) === s) return n;
      return s;
    });
  }
  if (operator === '$regex') return t;
  if (['$gt', '$gte', '$lt', '$lte'].includes(operator)) {
    const n = Number(t);
    return Number.isFinite(n) ? n : t;
  }
  if (t === 'true') return true;
  if (t === 'false') return false;
  const n = Number(t);
  if (t !== '' && Number.isFinite(n) && String(n) === t) return n;
  return t;
}

function buildWhenFromBranchRow(row: ConditionalBranchFormRow): Record<string, unknown> {
  const path = row.fieldPath.trim();
  const op = row.operator;
  const operand = coerceWhenOperand(op, row.value);
  return { [path]: { [op]: operand } };
}

export function buildConditionalApiConfig(
  state: ConditionalFormState,
  groups: AdminModelsByProvider[],
): { ok: true; config: Record<string, unknown> } | { ok: false; message: string } {
  const def = resolveProviderModelIds(groups, state.defaultProvider, state.defaultModel);
  if (!def) {
    return { ok: false, message: 'Select a default provider and model for the conditional strategy.' };
  }

  const conditions: Array<Record<string, unknown>> = [];
  for (const b of state.branches) {
    const path = b.fieldPath.trim();
    if (!path) continue;
    const then = resolveProviderModelIds(groups, b.thenProvider, b.thenModel);
    if (!then) {
      return {
        ok: false,
        message: 'Each condition with a match field needs a target provider and model.',
      };
    }
    conditions.push({
      when: buildWhenFromBranchRow(b),
      then: { type: 'single', providerId: then.providerId, modelId: then.modelId },
    });
  }

  return {
    ok: true,
    config: {
      type: 'conditional',
      conditions,
      default: { type: 'single', providerId: def.providerId, modelId: def.modelId },
    },
  };
}

export function hydrateConditionalEditorState(
  config: unknown,
  groups: AdminModelsByProvider[],
): ConditionalEditorHydration {
  if (!config || typeof config !== 'object') {
    return { mode: 'form', form: emptyConditionalFormState() };
  }
  const parsed = tryParseConditionalFormFromConfig(config, groups);
  if (parsed) return { mode: 'form', form: parsed };
  return { mode: 'json', text: JSON.stringify(config, null, 2) };
}

export function resolveConditionalConfigFromEditor(
  hydration: ConditionalEditorHydration,
  groups: AdminModelsByProvider[],
): { ok: true; config: unknown } | { ok: false; message: string } {
  if (hydration.mode === 'form') {
    return buildConditionalApiConfig(hydration.form, groups);
  }
  try {
    const parsed: unknown = JSON.parse(hydration.text);
    if (!isValidConditionalConfig(parsed)) {
      return {
        ok: false,
        message:
          'Conditional JSON must include type "conditional", a conditions array, and a default strategy.',
      };
    }
    return { ok: true, config: parsed };
  } catch {
    return { ok: false, message: 'Invalid JSON in conditional configuration.' };
  }
}

export function conditionalFormInternalModelIds(
  groups: AdminModelsByProvider[],
  form: ConditionalFormState,
): Set<string> {
  const ids = new Set<string>();
  const add = (providerName: string, providerModelId: string) => {
    const r = resolveProviderModelIds(groups, providerName, providerModelId);
    if (r) ids.add(r.modelId);
  };
  add(form.defaultProvider, form.defaultModel);
  for (const b of form.branches) {
    add(b.thenProvider, b.thenModel);
  }
  return ids;
}

export function parseRoutingConfigForForm(
  strategyType: StrategyType,
  config: unknown,
  groups: AdminModelsByProvider[],
): { entries: ProviderModelEntry[]; singleProvider: string; singleModel: string } {
  const empty: ProviderModelEntry = { provider: '', model: '', weight: '50' };
  const emptyState = { entries: [empty], singleProvider: '', singleModel: '' };

  if (strategyType === 'policy') {
    return emptyState;
  }

  if (!config || typeof config !== 'object') return emptyState;
  const cfg = config as Record<string, unknown>;

  if (strategyType === 'conditional') {
    return emptyState;
  }

  if (strategyType === 'single') {
    if (cfg.type === 'single' && typeof cfg.providerId === 'string' && typeof cfg.modelId === 'string') {
      const ui = uiFromApiIds(groups, cfg.providerId, cfg.modelId);
      if (ui) return { entries: [], singleProvider: ui.provider, singleModel: ui.model };
    }
    return {
      entries: [],
      singleProvider: String(cfg.provider ?? ''),
      singleModel: String(cfg.model ?? ''),
    };
  }

  if (strategyType === 'fallback') {
    if (cfg.type !== 'fallback' || !Array.isArray(cfg.targets)) return emptyState;
    const entries: ProviderModelEntry[] = (cfg.targets as unknown[]).map((raw) => {
      const t = raw as Record<string, unknown>;
      if (t.type === 'single' && typeof t.providerId === 'string' && typeof t.modelId === 'string') {
        const ui = uiFromApiIds(groups, t.providerId, t.modelId);
        if (ui) return { ...ui, weight: '50' };
      }
      return {
        provider: String(t.provider ?? ''),
        model: String(t.model ?? ''),
        weight: String(t.weight ?? '50'),
      };
    });
    return entries.length > 0 ? { entries, singleProvider: '', singleModel: '' } : emptyState;
  }

  if (strategyType === 'loadbalance') {
    if (cfg.type !== 'loadbalance' || !Array.isArray(cfg.weightedTargets)) return emptyState;
    const entries: ProviderModelEntry[] = (cfg.weightedTargets as unknown[]).map((raw) => {
      const wt = raw as Record<string, unknown>;
      const node = wt.node as Record<string, unknown> | undefined;
      if (node?.type === 'single' && typeof node.providerId === 'string' && typeof node.modelId === 'string') {
        const ui = uiFromApiIds(groups, node.providerId, node.modelId);
        if (ui) return { ...ui, weight: String(wt.weight ?? '50') };
      }
      return { provider: '', model: '', weight: String(wt.weight ?? '50') };
    });
    return entries.length > 0 ? { entries, singleProvider: '', singleModel: '' } : emptyState;
  }

  if (strategyType === 'ab_split') {
    if (cfg.type !== 'ab_split' || !Array.isArray(cfg.targets)) return emptyState;
    const entries: ProviderModelEntry[] = (cfg.targets as unknown[]).map((raw) => {
      const t = raw as Record<string, unknown>;
      if (typeof t.providerId === 'string' && typeof t.modelId === 'string') {
        const ui = uiFromApiIds(groups, t.providerId, t.modelId);
        if (ui) return { ...ui, weight: String(t.weight ?? '50') };
      }
      return { provider: '', model: '', weight: String(t.weight ?? '50') };
    });
    return entries.length > 0 ? { entries, singleProvider: '', singleModel: '' } : emptyState;
  }

  const targets = Array.isArray(cfg.targets) ? cfg.targets : [];
  if (targets.length === 0) return emptyState;
  return {
    entries: targets.map((raw: unknown) => {
      const t = raw as Record<string, unknown>;
      return {
        provider: String(t.provider ?? ''),
        model: String(t.model ?? ''),
        weight: String(t.weight ?? '50'),
      };
    }),
    singleProvider: '',
    singleModel: '',
  };
}

export function formatModelLabels(groups: AdminModelsByProvider[], modelIds: string[]): string {
  if (modelIds.length === 0) return '';
  return modelIds
    .map((mid) => {
      for (const g of groups) {
        const m = g?.models?.find((mod) => mod.id === mid);
        if (m) {
          const providerLabel = g.provider?.displayName?.trim() || g.provider?.name || '?';
          return `${providerLabel} / ${m.name}`;
        }
      }
      return mid;
    })
    .join(', ');
}

export function configuredInternalModelIds(
  providerGroups: AdminModelsByProvider[],
  strategyType: StrategyType,
  singleProvider: string,
  singleModel: string,
  entries: ProviderModelEntry[],
  conditionalForm?: ConditionalFormState | null,
): Set<string> {
  const ids = new Set<string>();
  if (strategyType === 'policy') {
    return ids;
  }
  if (strategyType === 'conditional' && conditionalForm) {
    return conditionalFormInternalModelIds(providerGroups, conditionalForm);
  }
  if (strategyType === 'single') {
    const r = resolveProviderModelIds(providerGroups, singleProvider, singleModel);
    if (r) ids.add(r.modelId);
    return ids;
  }
  for (const e of entries) {
    if (!e.provider.trim() || !e.model.trim()) continue;
    const r = resolveProviderModelIds(providerGroups, e.provider.trim(), e.model.trim());
    if (r) ids.add(r.modelId);
  }
  return ids;
}

export function buildRoutingApiConfig(input: {
  strategyType: StrategyType;
  providerGroups: AdminModelsByProvider[];
  singleProvider: string;
  singleModel: string;
  entries: ProviderModelEntry[];
  matchModelIds: string[];
  preservedConditionalConfig?: unknown | null;
  /** When set, builds conditional config from the structured form (takes precedence over preserved config). */
  conditionalForm?: ConditionalFormState | null;
}): { ok: true; config: unknown } | { ok: false; message: string } {
  const {
    strategyType,
    providerGroups,
    singleProvider,
    singleModel,
    entries,
    matchModelIds,
    preservedConditionalConfig,
    conditionalForm,
  } = input;

  const firstResolved = (): { providerId: string; modelId: string } | null => {
    for (const e of entries) {
      if (!e.provider.trim() || !e.model.trim()) continue;
      const r = resolveProviderModelIds(providerGroups, e.provider.trim(), e.model.trim());
      if (r) return r;
    }
    return null;
  };

  const singleFromUi = (): { providerId: string; modelId: string } | null =>
    resolveProviderModelIds(providerGroups, singleProvider, singleModel);

  switch (strategyType) {
    case 'single': {
      const r = singleFromUi();
      if (!r) return { ok: false, message: 'Select a provider and model.' };
      return { ok: true, config: { type: 'single', providerId: r.providerId, modelId: r.modelId } };
    }
    case 'fallback': {
      const targets = entries
        .filter((e) => e.provider.trim() && e.model.trim())
        .map((e) => resolveProviderModelIds(providerGroups, e.provider.trim(), e.model.trim()))
        .filter((x): x is { providerId: string; modelId: string } => x !== null)
        .map((r) => ({ type: 'single' as const, providerId: r.providerId, modelId: r.modelId }));
      if (targets.length === 0) return { ok: false, message: 'Add at least one fallback target.' };
      return { ok: true, config: { type: 'fallback', targets } };
    }
    case 'loadbalance': {
      const weightedTargets = entries
        .filter((e) => e.provider.trim() && e.model.trim())
        .map((e) => {
          const r = resolveProviderModelIds(providerGroups, e.provider.trim(), e.model.trim());
          if (!r) return null;
          const w = Number(e.weight);
          return {
            weight: Number.isFinite(w) && w >= 0 ? w : 50,
            node: { type: 'single' as const, providerId: r.providerId, modelId: r.modelId },
          };
        })
        .filter((x): x is NonNullable<typeof x> => x !== null);
      if (weightedTargets.length === 0) return { ok: false, message: 'Add at least one load-balance target.' };
      return { ok: true, config: { type: 'loadbalance', weightedTargets } };
    }
    case 'ab_split': {
      const targets = entries
        .filter((e) => e.provider.trim() && e.model.trim())
        .map((e) => {
          const r = resolveProviderModelIds(providerGroups, e.provider.trim(), e.model.trim());
          if (!r) return null;
          const w = Number(e.weight);
          return {
            providerId: r.providerId,
            modelId: r.modelId,
            weight: Number.isFinite(w) && w >= 0 ? w : 50,
          };
        })
        .filter((x): x is NonNullable<typeof x> => x !== null);
      if (targets.length === 0) return { ok: false, message: 'Add at least one A/B split target.' };
      return { ok: true, config: { type: 'ab_split', targets } };
    }
    case 'conditional': {
      if (conditionalForm) {
        const built = buildConditionalApiConfig(conditionalForm, providerGroups);
        if (built.ok) return { ok: true, config: built.config };
        return { ok: false, message: built.message };
      }
      if (isValidConditionalConfig(preservedConditionalConfig)) {
        return { ok: true, config: preservedConditionalConfig };
      }
      const plan =
        firstResolved() ??
        (matchModelIds[0] ? findPlanByModelId(providerGroups, matchModelIds[0]) : null) ??
        findFirstEnabledPlan(providerGroups);
      if (!plan) {
        return {
          ok: false,
          message: 'Cannot build conditional routing: add match models or configure via API.',
        };
      }
      const when =
        matchModelIds.length > 0 ? { 'requestedModel.id': { $eq: matchModelIds[0] } } : ({} as Record<string, unknown>);
      return {
        ok: true,
        config: {
          type: 'conditional',
          conditions: [
            {
              when,
              then: { type: 'single', providerId: plan.providerId, modelId: plan.modelId },
            },
          ],
          default: { type: 'single', providerId: plan.providerId, modelId: plan.modelId },
        },
      };
    }
    case 'smart': {
      // Smart strategy config is built from dedicated form fields, not provider/model entries
      return { ok: false, message: 'Use buildSmartConfig() for smart strategy.' };
    }
    default:
      return { ok: false, message: 'Unknown strategy type.' };
  }
}

// Smart Strategy Helpers

export interface SmartFormState {
  routerProvider: string;
  routerModel: string;
  systemPrompt: string;
  temperature: string;
  maxTokens: string;
  timeoutMs: string;
  defaultProvider: string;
  defaultModel: string;
}

export const DEFAULT_SMART_SYSTEM_PROMPT = `You are an AI model router for an enterprise gateway. Select the best model for the user's request.

## Available Models
{modelCatalog}

## Selection Rules
1. Analyze the task: coding, analysis, creative writing, Q&A, translation, math, reasoning
2. Match capabilities: images → vision, tools → function_calling, long text → large context
3. Cost: simple tasks → cheapest capable model; complex tasks → most capable
4. If uncertain, prefer the most capable model

## Output Format
Return ONLY valid JSON: {"modelId": "<exact ID from list>", "reason": "<brief explanation>"}`;

export function parseSmartConfig(
  config: unknown,
  groups: AdminModelsByProvider[],
): SmartFormState {
  const empty: SmartFormState = {
    routerProvider: '',
    routerModel: '',
    systemPrompt: DEFAULT_SMART_SYSTEM_PROMPT,
    temperature: '0',
    maxTokens: '1024',
    timeoutMs: '10000',
    defaultProvider: '',
    defaultModel: '',
  };

  if (!config || typeof config !== 'object') return empty;
  const cfg = config as Record<string, unknown>;
  if (cfg.type !== 'smart') return empty;

  const routerUi = typeof cfg.routerProviderId === 'string' && typeof cfg.routerModelId === 'string'
    ? uiFromApiIds(groups, cfg.routerProviderId, cfg.routerModelId)
    : null;

  const defaultUi =
    typeof cfg.defaultProviderId === 'string' && typeof cfg.defaultModelId === 'string'
      ? uiFromApiIds(groups, cfg.defaultProviderId, cfg.defaultModelId)
      : null;

  return {
    routerProvider: routerUi?.provider ?? '',
    routerModel: routerUi?.model ?? '',
    systemPrompt: typeof cfg.systemPrompt === 'string' ? cfg.systemPrompt : DEFAULT_SMART_SYSTEM_PROMPT,
    temperature: String(cfg.temperature ?? '0'),
    maxTokens: String(cfg.maxTokens ?? '1024'),
    timeoutMs: String(cfg.timeoutMs ?? '10000'),
    defaultProvider: defaultUi?.provider ?? '',
    defaultModel: defaultUi?.model ?? '',
  };
}

export function buildSmartConfig(
  state: SmartFormState,
  providerGroups: AdminModelsByProvider[],
): { ok: true; config: unknown } | { ok: false; message: string } {
  const router = resolveProviderModelIds(providerGroups, state.routerProvider, state.routerModel);
  if (!router) return { ok: false, message: 'Select a router provider and model.' };

  if (!state.systemPrompt.trim()) return { ok: false, message: 'System prompt is required.' };

  let defaultProviderId: string | undefined;
  let defaultModelId: string | undefined;
  if (state.defaultProvider && state.defaultModel) {
    const def = resolveProviderModelIds(providerGroups, state.defaultProvider, state.defaultModel);
    if (def) {
      defaultProviderId = def.providerId;
      defaultModelId = def.modelId;
    }
  }

  return {
    ok: true,
    config: {
      type: 'smart',
      routerProviderId: router.providerId,
      routerModelId: router.modelId,
      systemPrompt: state.systemPrompt,
      temperature: Number(state.temperature) || 0,
      maxTokens: Number(state.maxTokens) || 1024,
      timeoutMs: Number(state.timeoutMs) || 10000,
      ...(defaultProviderId && defaultModelId ? { defaultProviderId, defaultModelId } : {}),
    },
  };
}

// Inline Fallback Chain Helpers

export interface FallbackEntry {
  provider: string;
  model: string;
}

/**
 * Parse an API fallbackChain into form-friendly entries.
 */
export function parseFallbackChain(
  chain: unknown,
  groups: AdminModelsByProvider[],
): FallbackEntry[] {
  if (!Array.isArray(chain)) return [];
  return chain
    .map((entry: unknown) => {
      const e = entry as { providerId?: string; modelId?: string };
      if (!e.providerId || !e.modelId) return null;
      const ui = uiFromApiIds(groups, e.providerId, e.modelId);
      return ui ? { provider: ui.provider, model: ui.model } : null;
    })
    .filter((e): e is FallbackEntry => e !== null);
}

/**
 * Build API fallbackChain from form entries.
 */
export function buildFallbackChainApi(
  entries: FallbackEntry[],
  groups: AdminModelsByProvider[],
): Array<{ providerId: string; modelId: string }> {
  return entries
    .filter(e => e.provider.trim() && e.model.trim())
    .map(e => resolveProviderModelIds(groups, e.provider.trim(), e.model.trim()))
    .filter((r): r is { providerId: string; modelId: string } => r !== null);
}
