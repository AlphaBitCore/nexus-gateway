/**
 * Client-side diff between a provider's Model rows and the built-in catalog
 * template that describes the same upstream vendor.
 *
 * Model rows are created once through the provider wizard and then drift from
 * the catalog as vendors correct token ceilings and prices. A stale
 * `maxOutputTokens` is not cosmetic: the gateway fills the upstream
 * `max_tokens` from that column when a caller omits it, so a row above the
 * vendor's real ceiling makes every such request fail upstream.
 */
import type {
  ApiProviderTemplate,
  ApiTemplateModel,
  CreateModelInput,
  Model,
  UpdateModelInput,
} from '@/api/types';

const norm = (s: string | null | undefined): string => (s ?? '').trim().toLowerCase();

/** Base URLs compare equal regardless of case or a trailing slash. */
const normBaseUrl = (s: string | null | undefined): string => norm(s).replace(/\/+$/, '');

/**
 * Catalog-owned fields the diff compares. `code` is excluded on purpose: it is
 * a local name the admin may rename freely. `providerModelId` is excluded
 * because it is the identity the match is keyed on, not a value to overwrite.
 */
const SCALAR_FIELDS = [
  'name',
  'description',
  'type',
  'inputPricePerMillion',
  'outputPricePerMillion',
  'cachedInputReadPricePerMillion',
  'cachedInputWritePricePerMillion',
  'maxContextTokens',
  'maxOutputTokens',
] as const;

export type CatalogField = (typeof SCALAR_FIELDS)[number] | 'features';

export type CatalogValue = string | number | string[];

export interface CatalogFieldDiff {
  field: CatalogField;
  /** The provider row's value. `undefined` = the row has no value at all. */
  current: CatalogValue | undefined;
  /** The catalog's value. Never empty — a field the catalog has no fact for is not diffed. */
  catalog: CatalogValue;
}

export interface CatalogSyncNewRow {
  key: string;
  template: ApiTemplateModel;
}

export interface CatalogSyncChangedRow {
  key: string;
  model: Model;
  template: ApiTemplateModel;
  diffs: CatalogFieldDiff[];
}

export interface CatalogSyncProviderOnlyRow {
  key: string;
  model: Model;
}

export interface CatalogSyncDiff {
  newRows: CatalogSyncNewRow[];
  changedRows: CatalogSyncChangedRow[];
  providerOnlyRows: CatalogSyncProviderOnlyRow[];
}

/**
 * Every upstream name a catalog entry answers to, normalised.
 *
 * `providerModelId` is the vendor's own id and the strongest key: a wizard
 * created row copies it verbatim. `code` is included because for some
 * providers it is the only distinct local name the catalog carries, and
 * `aliases` because the catalog renames a model to a floating id while keeping
 * the pinned one it replaced — a row still on the old id is the same model.
 */
export function templateIdentityKeys(tm: ApiTemplateModel): string[] {
  const keys = [tm.providerModelId, tm.code, ...(tm.aliases ?? [])].map(norm).filter(Boolean);
  return [...new Set(keys)];
}

const sameFeatureSet = (a: string[], b: string[]): boolean => {
  const sa = [...new Set(a.map(norm))].sort();
  const sb = [...new Set(b.map(norm))].sort();
  return sa.length === sb.length && sa.every((v, i) => v === sb[i]);
};

/**
 * Per-field differences between one provider row and its catalog entry.
 *
 * A field the catalog has no fact for — absent, null, empty string, or empty
 * feature list — yields no diff. The catalog corrects what it knows; it never
 * proposes erasing a value the admin supplied.
 */
export function diffModelAgainstTemplate(model: Model, tm: ApiTemplateModel): CatalogFieldDiff[] {
  const diffs: CatalogFieldDiff[] = [];

  for (const field of SCALAR_FIELDS) {
    const catalog = tm[field];
    if (catalog === undefined || catalog === null || catalog === '') continue;
    const current = model[field] ?? undefined;
    if (current !== catalog) diffs.push({ field, current, catalog });
  }

  const catalogFeatures = tm.features ?? [];
  if (catalogFeatures.length > 0) {
    const currentFeatures = model.features ?? [];
    if (!sameFeatureSet(currentFeatures, catalogFeatures)) {
      diffs.push({ field: 'features', current: [...currentFeatures], catalog: [...catalogFeatures] });
    }
  }

  return diffs;
}

/**
 * Sort a provider's rows and a template's entries into three buckets.
 *
 * A row matches an entry when the row's `providerModelId` is one of the
 * entry's identity keys. Two rows may legitimately match one entry (an admin
 * holding both the pinned and the floating id); both are then offered, and the
 * entry is not additionally proposed as new.
 */
export function buildCatalogSyncDiff(
  models: Model[],
  templateModels: ApiTemplateModel[],
): CatalogSyncDiff {
  const byIdentity = new Map<string, ApiTemplateModel>();
  for (const tm of templateModels) {
    for (const key of templateIdentityKeys(tm)) {
      if (!byIdentity.has(key)) byIdentity.set(key, tm);
    }
  }

  const matched = new Set<ApiTemplateModel>();
  const changedRows: CatalogSyncChangedRow[] = [];
  const providerOnlyRows: CatalogSyncProviderOnlyRow[] = [];

  for (const model of models) {
    const tm = byIdentity.get(norm(model.providerModelId));
    if (!tm) {
      providerOnlyRows.push({ key: model.id, model });
      continue;
    }
    matched.add(tm);
    const diffs = diffModelAgainstTemplate(model, tm);
    if (diffs.length > 0) changedRows.push({ key: model.id, model, template: tm, diffs });
  }

  const newRows: CatalogSyncNewRow[] = templateModels
    .filter((tm) => !matched.has(tm))
    .map((tm) => ({ key: `new:${tm.code}`, template: tm }));

  return { newRows, changedRows, providerOnlyRows };
}

/**
 * Create payload for a catalog entry absent from the provider.
 *
 * `code` is sent from the catalog rather than left to default: model codes are
 * globally unique, and the catalog's code is the value chosen to stay clear of
 * other providers carrying the same upstream id.
 */
export function catalogCreateInput(tm: ApiTemplateModel): CreateModelInput {
  return {
    name: tm.name,
    code: tm.code,
    providerModelId: tm.providerModelId,
    type: tm.type,
    ...(tm.description ? { description: tm.description } : {}),
    ...(tm.features?.length ? { features: [...tm.features] } : {}),
    ...(tm.aliases?.length ? { aliases: [...tm.aliases] } : {}),
    ...(tm.inputPricePerMillion != null ? { inputPricePerMillion: tm.inputPricePerMillion } : {}),
    ...(tm.outputPricePerMillion != null ? { outputPricePerMillion: tm.outputPricePerMillion } : {}),
    ...(tm.cachedInputReadPricePerMillion != null
      ? { cachedInputReadPricePerMillion: tm.cachedInputReadPricePerMillion }
      : {}),
    ...(tm.cachedInputWritePricePerMillion != null
      ? { cachedInputWritePricePerMillion: tm.cachedInputWritePricePerMillion }
      : {}),
    ...(tm.maxContextTokens != null ? { maxContextTokens: tm.maxContextTokens } : {}),
    ...(tm.maxOutputTokens != null ? { maxOutputTokens: tm.maxOutputTokens } : {}),
  };
}

/** Update payload carrying only the fields that actually differ. */
export function catalogUpdateInput(diffs: CatalogFieldDiff[]): UpdateModelInput {
  const payload: Record<string, CatalogValue> = {};
  for (const d of diffs) payload[d.field] = d.catalog;
  return payload as UpdateModelInput;
}

/**
 * The built-in template describing the same upstream vendor as this provider,
 * or null when nothing matches confidently.
 *
 * The wizard seeds a new provider's name from the template's, so the name is
 * the direct key. A renamed provider falls back to its wire adapter, but only
 * together with a matching base URL — adapter alone would resolve an
 * OpenAI-compatible endpoint that merely speaks the protocol (a self-hosted
 * runtime) to the real vendor's catalog and offer its whole model list.
 */
export function resolveTemplateName(
  provider: { name: string; adapterType: string; baseUrl: string },
  templates: ApiProviderTemplate[],
): string | null {
  const byName = templates.find((t) => norm(t.name) === norm(provider.name));
  if (byName) return byName.name;

  const byAdapter = templates.find(
    (t) =>
      norm(t.adapterType) === norm(provider.adapterType) &&
      normBaseUrl(t.baseUrl) === normBaseUrl(provider.baseUrl),
  );
  return byAdapter ? byAdapter.name : null;
}
