import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import styles from './ModelModalitiesField.module.css';

/**
 * The modalities a model accepts and returns.
 *
 * This is the one fact about a model the admin UI could not state. `type`
 * answers which endpoint serves it and `features` carries the non-modality
 * capabilities, so "this model accepts audio" had nowhere to live — while the
 * arrays themselves were already in the database and already served to every
 * SDK caller on GET /v1/models.
 *
 * Collapsed by default, on purpose. Almost every model's modalities follow
 * from its type, and a form that opens with two more multi-selects is a worse
 * product than the gap it closes. The summary line states what the model
 * declares; the editor appears only when an admin chooses to diverge.
 *
 * It does NOT compute defaults. The backend fills them from the row's type and
 * features when a caller sends none (modelstore.defaultModalities), and a
 * second derivation here would be exactly the two-vocabularies-for-one-fact
 * problem that made `features.vision` and `inputModalities ∋ image` disagree on
 * 34 production rows. What this renders is what the API returned.
 */
export const MODALITY_OPTIONS = ['text', 'image', 'audio', 'video', 'file', 'embedding'] as const;

export interface ModelModalitiesFieldProps {
  /** Absent is a real state — a row saved before these columns existed, or a
      form that has not loaded one. It renders as "nothing declared" rather
      than crashing the drawer, and it is distinct from an empty array only in
      provenance, not in what the admin sees. */
  input?: string[];
  output?: string[];
  /** The FLOOR: what a request must carry for this model to serve it at all.
      Distinct from `input`, which is the ceiling — a model can accept audio
      without requiring it, and one that requires audio refuses a plain text
      chat even though it is a chat model. Neither derives the other, so an
      admin who can set one and not the other holds half a control, and the
      missing half is the protective one. Unlike the ceiling pair the backend
      fills no default here, so a floor left unset stays unset. */
  required?: string[];
  onChange: (next: { input: string[]; output: string[]; required: string[] }) => void;
  disabled?: boolean;
}

function Chips({
  options,
  selected,
  onToggle,
  disabled,
}: {
  options: readonly string[];
  selected: string[];
  onToggle: (v: string) => void;
  disabled?: boolean;
}) {
  const set = new Set(selected);
  return (
    <div className={styles.chipRow}>
      {options.map((opt) => (
        <button
          key={opt}
          type="button"
          disabled={disabled}
          aria-pressed={set.has(opt)}
          data-design-system-escape="chip toggle — not a navigation/action button"
          className={set.has(opt) ? styles.chipActive : styles.chip}
          onClick={() => !disabled && onToggle(opt)}
        >
          {opt}
        </button>
      ))}
    </div>
  );
}

export function ModelModalitiesField({
  input: inputProp,
  output: outputProp,
  required: requiredProp,
  onChange,
  disabled,
}: ModelModalitiesFieldProps) {
  const { t } = useTranslation(['pages']);
  const [open, setOpen] = useState(false);
  const input = inputProp ?? [];
  const output = outputProp ?? [];
  const required = requiredProp ?? [];

  const toggle = (which: 'input' | 'output' | 'required', v: string) => {
    const current = which === 'input' ? input : which === 'output' ? output : required;
    const next = current.includes(v) ? current.filter((x) => x !== v) : [...current, v];
    if (which === 'output') return onChange({ input, output: next, required });
    if (which === 'required') return onChange({ input, output, required: next });
    // Narrowing the ceiling below the floor would leave the model requiring
    // something it cannot accept — a contradiction the API rejects. Drop the
    // orphaned floor entries with it rather than saving a row that cannot
    // serve any request.
    return onChange({ input: next, output, required: required.filter((r) => next.includes(r)) });
  };

  const summary = t('pages:providers.capabilities.modalitiesSummary', {
    input: input.length ? input.join(', ') : t('pages:providers.capabilities.modalitiesNone'),
    output: output.length ? output.join(', ') : t('pages:providers.capabilities.modalitiesNone'),
  });
  // Stated only when there IS one. Almost every model has no floor, and a line
  // reading "requires: none" on fifty rows is noise that makes the two rows
  // that do carry one harder to notice.
  const floor = required.length
    ? t('pages:providers.capabilities.modalitiesRequiredSummary', { required: required.join(', ') })
    : '';

  return (
    <div className={styles.root}>
      <div className={styles.summaryRow}>
        <span className={styles.summary}>
          {summary}
          {floor ? ` · ${floor}` : ''}
        </span>
        {!disabled && (
          <button type="button" className={styles.toggle} onClick={() => setOpen((o) => !o)}>
            {open
              ? t('pages:providers.capabilities.modalitiesDone')
              : t('pages:providers.capabilities.modalitiesEdit')}
          </button>
        )}
      </div>
      {open && (
        <div className={styles.editor}>
          <label className={styles.label}>{t('pages:providers.capabilities.modalitiesInput')}</label>
          <Chips
            options={MODALITY_OPTIONS}
            selected={input}
            onToggle={(v) => toggle('input', v)}
            disabled={disabled}
          />
          <label className={styles.label}>{t('pages:providers.capabilities.modalitiesOutput')}</label>
          <Chips
            options={MODALITY_OPTIONS}
            selected={output}
            onToggle={(v) => toggle('output', v)}
            disabled={disabled}
          />
          {/* The floor, offered only over what the model accepts. Requiring
              something outside the ceiling is a row that can serve no request
              at all, and the API rejects it — better said here, where the admin
              is choosing, than as a 400 after they save. */}
          <label className={styles.label}>
            {t('pages:providers.capabilities.modalitiesRequired')}
          </label>
          <Chips
            options={MODALITY_OPTIONS.filter((o) => input.includes(o))}
            selected={required}
            onToggle={(v) => toggle('required', v)}
            disabled={disabled}
          />
          {!input.length && (
            <p className={styles.hint}>
              {t('pages:providers.capabilities.modalitiesRequiredNeedsAccepts')}
            </p>
          )}
          <p className={styles.hint}>{t('pages:providers.capabilities.modalitiesHint')}</p>
          <p className={styles.hint}>
            {t('pages:providers.capabilities.modalitiesRequiredHint')}
          </p>
        </div>
      )}
    </div>
  );
}
