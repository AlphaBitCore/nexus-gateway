/**
 * ProviderModelCapabilitiesPanel — inline editor for Model.capabilityJson.
 *
 * Renders conditionally based on model type:
 *   - embedding → EmbeddingsCapability fields
 *   - chat       → contextLimit / maxOutputTokens / supportedFeatures
 * Other model types (image, audio) show nothing (no capability schema yet).
 *
 * The panel is controlled: the parent supplies the current value and an
 * onChange callback. Validation errors are surfaced inline next to each
 * field — the parent still owns the final save decision.
 */
import { useState, useEffect, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import type { ModelCapabilityJson, ModelEmbeddingsCapability } from '@/api/types';
import { Input, Button, Stack } from '@/components/ui';
import styles from './ProviderDetail.module.css';
import capStyles from './ProviderModelCapabilitiesPanel.module.css';

// ── Option sets (canonical) ────────────────────────────────────────────────

/** Encoding formats the OpenAI-compatible embeddings wire supports. */
export const ENCODING_FORMAT_OPTIONS: string[] = [
  'float', 'base64', 'int8', 'uint8', 'binary', 'ubinary',
];

/** Input types per the Cohere / Voyage taxonomy. */
export const INPUT_TYPE_OPTIONS: string[] = [
  'search_document', 'search_query', 'classification',
  'clustering', 'query', 'document',
];

/** Task types per the Gemini taxonomy. */
export const TASK_TYPE_OPTIONS: string[] = [
  'semantic_similarity', 'classification', 'clustering',
  'retrieval_document', 'retrieval_query',
];

// ── Validation helpers ─────────────────────────────────────────────────────

function validateDimension(v: number): string | null {
  if (!Number.isInteger(v) || v < 1) return 'validationErrors.dimensionsRange';
  if (v > 65536) return 'validationErrors.dimensionsRange';
  return null;
}

function validateBatchSize(v: number): string | null {
  if (!Number.isInteger(v) || v < 1) return 'validationErrors.batchSizeRange';
  if (v > 65536) return 'validationErrors.batchSizeRange';
  return null;
}

// ── Types ──────────────────────────────────────────────────────────────────

export interface CapabilitiesPanelProps {
  /** 'embedding' | 'chat' — drives which fields are shown. */
  modelType: string;
  /**
   * Current capabilityJson value. Pass undefined / null to start from
   * an empty document; the panel will initialise with no values set.
   */
  value: ModelCapabilityJson | null | undefined;
  /** Called whenever any field changes. Passes the full updated document. */
  onChange: (next: ModelCapabilityJson) => void;
  /** When false, all inputs are read-only (view mode). Default: true. */
  editable?: boolean;
}

// ── Chip multi-select ──────────────────────────────────────────────────────

interface ChipSelectProps {
  options: string[];
  selected: string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
}

function ChipSelect({ options, selected, onChange, disabled }: ChipSelectProps) {
  const set = new Set(selected);
  return (
    <div className={capStyles.chipRow}>
      {options.map((opt) => {
        const active = set.has(opt);
        return (
          <button
            key={opt}
            type="button"
            disabled={disabled}
            data-design-system-escape="chip toggle — not a navigation/action button"
            onClick={() => {
              if (disabled) return;
              const next = active
                ? selected.filter((v) => v !== opt)
                : [...selected, opt];
              onChange(next);
            }}
            className={active ? capStyles.chipActive : capStyles.chip}
          >
            {opt}
          </button>
        );
      })}
    </div>
  );
}

// ── Key-value editor (RequiredExtensions) ──────────────────────────────────

interface KVEntry {
  key: string;
  value: string;
}

interface KVEditorProps {
  entries: KVEntry[];
  onChange: (next: KVEntry[]) => void;
  disabled?: boolean;
  addLabel: string;
}

function KVEditor({ entries, onChange, disabled, addLabel }: KVEditorProps) {
  const { t } = useTranslation('pages');
  // Local row state so a freshly-added blank row survives. The parent serialises
  // required_extensions and drops empty entries for the persisted value, which
  // would otherwise strip a new blank row on the round-trip and make "Add"
  // appear to do nothing. We propagate a row upstream only once it has content.
  const [rows, setRows] = useState<KVEntry[]>(entries);

  // Re-seed when the upstream (persisted) value genuinely changes — e.g. a
  // different model — but not from the echo of our own propagation, so a blank
  // row being filled in isn't clobbered. Compare the JSON of upstream entries
  // against the JSON of our non-empty local projection (what we propagate);
  // JSON.stringify is collision-free, so no magic separator is needed.
  const upstreamKey = JSON.stringify(entries);
  const localNonEmptyKey = JSON.stringify(rows.filter((r) => r.key.trim() !== ''));
  useEffect(() => {
    if (upstreamKey !== localNonEmptyKey) setRows(entries);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [upstreamKey]);

  const propagate = useCallback((next: KVEntry[]) => {
    setRows(next);
    onChange(next);
  }, [onChange]);
  const handleChange = useCallback(
    (idx: number, field: 'key' | 'value', v: string) => {
      propagate(rows.map((e, i) => (i === idx ? { ...e, [field]: v } : e)));
    },
    [rows, propagate],
  );
  const handleAdd = useCallback(() => {
    setRows((prev) => [...prev, { key: '', value: '' }]);
  }, []);
  const handleRemove = useCallback(
    (idx: number) => {
      propagate(rows.filter((_, i) => i !== idx));
    },
    [rows, propagate],
  );

  return (
    <div className={capStyles.kvEditorRoot}>
      {rows.map((e, i) => (
        <div key={i} className={capStyles.kvEditorRow}>
          <Input
            value={e.key}
            onChange={(ev) => handleChange(i, 'key', ev.target.value)}
            placeholder={t('pages:providers.capKeyPlaceholder')}
            disabled={disabled}
            className={capStyles.kvInput}
          />
          <span className={capStyles.kvEquals}>=</span>
          <Input
            value={e.value}
            onChange={(ev) => handleChange(i, 'value', ev.target.value)}
            placeholder={t('pages:providers.capValuePlaceholder')}
            disabled={disabled}
            className={capStyles.kvInput}
          />
          {!disabled && (
            <button
              type="button"
              data-design-system-escape="kv-row remove — small ×-button beside inline editor row"
              onClick={() => handleRemove(i)}
              className={capStyles.kvRemove}
            >
              ×
            </button>
          )}
        </div>
      ))}
      {!disabled && (
        <Button type="button" variant="secondary" size="sm" onClick={handleAdd}>
          {addLabel}
        </Button>
      )}
    </div>
  );
}

// ── DimensionEditor ────────────────────────────────────────────────────────

interface DimensionEditorProps {
  dimensions: number[];
  onChange: (next: number[]) => void;
  disabled?: boolean;
  addLabel: string;
  errorLabel: string;
}

function DimensionEditor({ dimensions, onChange, disabled, addLabel, errorLabel }: DimensionEditorProps) {
  const [draft, setDraft] = useState('');
  const [err, setErr] = useState<string | null>(null);

  const commit = useCallback(() => {
    const v = parseInt(draft, 10);
    if (Number.isNaN(v)) { setErr(errorLabel); return; }
    const e = validateDimension(v);
    if (e) { setErr(errorLabel); return; }
    if (dimensions.includes(v)) { setDraft(''); return; }
    onChange([...dimensions, v].sort((a, b) => a - b));
    setDraft('');
    setErr(null);
  }, [draft, dimensions, onChange, errorLabel]);

  const remove = useCallback(
    (v: number) => onChange(dimensions.filter((d) => d !== v)),
    [dimensions, onChange],
  );

  return (
    <div className={capStyles.dimRoot}>
      <div className={capStyles.chipRow}>
        {dimensions.map((d) => (
          <span key={d} className={capStyles.chipActive}>
            {d}
            {!disabled && (
              <button
                type="button"
                data-design-system-escape="dimension chip remove — ×-button inside a chip pill"
                onClick={() => remove(d)}
                className={capStyles.chipXBtn}
              >
                ×
              </button>
            )}
          </span>
        ))}
      </div>
      {!disabled && (
        <Stack direction="horizontal" gap="xs">
          <Input
            value={draft}
            onChange={(e) => { setDraft(e.target.value); setErr(null); }}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); commit(); } }}
            placeholder="new dimension"
            type="number"
            className={capStyles.dimInput}
          />
          <Button type="button" variant="secondary" size="sm" onClick={commit}>{addLabel}</Button>
        </Stack>
      )}
      {err && <span className={capStyles.fieldError}>{err}</span>}
    </div>
  );
}

// ── Main panel ─────────────────────────────────────────────────────────────

export function ProviderModelCapabilitiesPanel({
  modelType,
  value,
  onChange,
  editable = true,
}: CapabilitiesPanelProps) {
  const { t } = useTranslation('pages');

  const cap = useMemo(() => value ?? {}, [value]);
  const emb = useMemo<ModelEmbeddingsCapability>(() => cap.embeddings ?? {}, [cap]);

  // Required-extensions are serialised as opaque strings on the wire
  // (per EmbeddingsCapability.RequiredExtensions in Go capability/types.go).
  // Typical values: "nexus.ext.cohere.input_type", "nexus.ext.gemini.taskType".
  // We render them as key-only chips since the values are admin-declared
  // (not value-pinned at this layer; the routing pre-filter validates the
  // per-request value against SupportedInputTypes / SupportedTaskTypes).
  const reqExtsRaw: string[] = emb.required_extensions ?? [];
  const kvEntries: KVEntry[] = reqExtsRaw.map((r) => {
    const idx = r.indexOf('=');
    return idx === -1
      ? { key: r, value: '' }
      : { key: r.slice(0, idx), value: r.slice(idx + 1) };
  });

  const handleEmbeddingsChange = useCallback(
    (patch: Partial<ModelEmbeddingsCapability>) => {
      onChange({ ...cap, embeddings: { ...emb, ...patch } });
    },
    [cap, emb, onChange],
  );

  const handleRequiredExtensionsChange = useCallback(
    (next: KVEntry[]) => {
      const serialised = next
        .map((e) => (e.value === '' ? e.key : `${e.key}=${e.value}`))
        .filter((s) => s.trim() !== '');
      handleEmbeddingsChange({ required_extensions: serialised });
    },
    [handleEmbeddingsChange],
  );

  // ── Embedding model panel ────────────────────────────────────────────────
  if (modelType === 'embedding') {
    return (
      <div className={`${capStyles.panel} ${capStyles.editorPanel}`}>
        <div className={styles.sectionTitle}>
          {t('providers.capabilities.sectionTitle')}
        </div>
        <p className={capStyles.subtitle}>
          {t('providers.capabilities.embeddingsSubtitle')}
        </p>

        {/* Supported Dimensions */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.supportedDimensionsLabel')}
          </label>
          <DimensionEditor
            dimensions={emb.supported_dimensions ?? []}
            onChange={(v) => handleEmbeddingsChange({ supported_dimensions: v })}
            disabled={!editable}
            addLabel={t('providers.capabilities.addDimensionButton')}
            errorLabel={t('providers.capabilities.validationErrors.dimensionsRange')}
          />
        </div>

        {/* Default Dimension */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.defaultDimensionLabel')}
          </label>
          <Input
            type="number"
            value={emb.default_dimension ?? ''}
            onChange={(e) => {
              const v = parseInt(e.target.value, 10);
              handleEmbeddingsChange({
                default_dimension: Number.isNaN(v) ? undefined : v,
              });
            }}
            placeholder="e.g. 1536"
            disabled={!editable}
            className={capStyles.numberInput}
          />
        </div>

        {/* Max Batch Size */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.maxBatchSizeLabel')}
          </label>
          <Input
            type="number"
            value={emb.max_batch_size ?? ''}
            onChange={(e) => {
              const v = parseInt(e.target.value, 10);
              const err = Number.isNaN(v) ? null : validateBatchSize(v);
              if (!err) {
                handleEmbeddingsChange({
                  max_batch_size: Number.isNaN(v) ? undefined : v,
                });
              }
            }}
            placeholder="e.g. 96"
            disabled={!editable}
            className={capStyles.numberInput}
          />
        </div>

        {/* Max Input Tokens */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.maxInputTokensLabel')}
          </label>
          <Input
            type="number"
            value={emb.max_input_tokens ?? ''}
            onChange={(e) => {
              const v = parseInt(e.target.value, 10);
              handleEmbeddingsChange({
                max_input_tokens: Number.isNaN(v) ? undefined : v,
              });
            }}
            placeholder="e.g. 8192"
            disabled={!editable}
            className={capStyles.numberInput}
          />
        </div>

        {/* Supported Encoding Formats */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.supportedEncodingFormatsLabel')}
          </label>
          <ChipSelect
            options={ENCODING_FORMAT_OPTIONS}
            selected={emb.supported_encoding_formats ?? []}
            onChange={(v) => handleEmbeddingsChange({ supported_encoding_formats: v })}
            disabled={!editable}
          />
        </div>

        {/* Supported Input Types (Cohere/Voyage) */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.supportedInputTypesLabel')}
          </label>
          <ChipSelect
            options={INPUT_TYPE_OPTIONS}
            selected={emb.supported_input_types ?? []}
            onChange={(v) => handleEmbeddingsChange({ supported_input_types: v })}
            disabled={!editable}
          />
        </div>

        {/* Supported Task Types (Gemini) */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.supportedTaskTypesLabel')}
          </label>
          <ChipSelect
            options={TASK_TYPE_OPTIONS}
            selected={emb.supported_task_types ?? []}
            onChange={(v) => handleEmbeddingsChange({ supported_task_types: v })}
            disabled={!editable}
          />
        </div>

        {/* Required Extensions key-value (e.g. cohere.input_type=true) */}
        <div className={capStyles.field}>
          <label className={styles.inlineLabel}>
            {t('providers.capabilities.requiredExtensionsLabel')}
          </label>
          <KVEditor
            entries={kvEntries}
            onChange={handleRequiredExtensionsChange}
            disabled={!editable}
            addLabel={t('providers.capabilities.addExtensionButton')}
          />
          <span className={capStyles.fieldHint}>
            {t('providers.capabilities.requiredExtensionsHint')}
          </span>
        </div>
      </div>
    );
  }

  // ── Chat model panel ─────────────────────────────────────────────────────
  if (modelType === 'chat') {
    return (
      <div className={capStyles.infoPanel}>
        <div className={capStyles.infoContent}>
          <p className={capStyles.infoDescription}>
            {t('providers.capabilities.chatSubtitle')}
          </p>
          <p className={capStyles.infoHint}>
            {t('providers.capabilities.chatCapabilitiesHint')}
          </p>
        </div>
      </div>
    );
  }

  // No capabilities panel for image / audio models yet.
  return null;
}
