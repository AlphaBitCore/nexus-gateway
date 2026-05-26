/**
 * Compliance-tag chip and chip-input helpers.
 *
 * Shared between the traffic + proxy audit UI:
 *   - `ComplianceTagChipList` — read-only chips for detail drawers / tables.
 *   - `ComplianceTagChipInput` — controlled chip input for filter bars.
 *
 * The backend produces namespaced tags (e.g. `severity:confidential`,
 * `compliance:pii`, `detector:content-safety`, `category:violence`). The
 * namespace prefix drives the chip color family (see CSS module); tags with
 * an unknown prefix render with the neutral fallback.
 */
import { useState, useCallback, type KeyboardEvent, type ChangeEvent } from 'react';
import clsx from 'clsx';
import css from './ComplianceTagChips.module.css';

function classForTag(tag: string): string {
  const prefix = tag.split(':', 1)[0]?.toLowerCase();
  switch (prefix) {
    case 'severity':
      return css.chipSeverity;
    case 'compliance':
      return css.chipCompliance;
    case 'detector':
      return css.chipDetector;
    case 'category':
      return css.chipCategory;
    default:
      return '';
  }
}

interface ComplianceTagChipListProps {
  tags: readonly string[];
  emptyLabel?: string;
  /** When provided, each chip shows an `x` remove button that calls this. */
  onRemove?: (tag: string) => void;
}

export function ComplianceTagChipList({ tags, emptyLabel, onRemove }: ComplianceTagChipListProps) {
  if (!tags || tags.length === 0) {
    return emptyLabel ? <span className={css.emptyHint}>{emptyLabel}</span> : null;
  }
  return (
    <div className={css.chipList}>
      {tags.map((tag) => (
        <span key={tag} className={clsx(css.chip, classForTag(tag))} title={tag}>
          {tag}
          {onRemove && (
            <button
              type="button"
              onClick={() => onRemove(tag)}
              aria-label={`Remove tag ${tag}`}
              className={css.removeBtn}
            >
              &times;
            </button>
          )}
        </span>
      ))}
    </div>
  );
}

interface ComplianceTagChipInputProps {
  /** Current tags (controlled). */
  value: readonly string[];
  /** Replace the full tag array. */
  onChange: (tags: string[]) => void;
  placeholder?: string;
  ariaLabel?: string;
  inputClassName?: string;
}

/**
 * Chip-input: type a tag, press Enter or comma to commit; press Backspace in
 * an empty field to pop the last chip. Deduplicates and trims on commit.
 */
export function ComplianceTagChipInput({
  value,
  onChange,
  placeholder,
  ariaLabel,
  inputClassName,
}: ComplianceTagChipInputProps) {
  const [draft, setDraft] = useState('');

  const commit = useCallback(
    (raw: string) => {
      const trimmed = raw.trim();
      if (!trimmed) return;
      if (value.includes(trimmed)) {
        setDraft('');
        return;
      }
      onChange([...value, trimmed]);
      setDraft('');
    },
    [value, onChange],
  );

  const onKeyDown = (ev: KeyboardEvent<HTMLInputElement>) => {
    if (ev.key === 'Enter' || ev.key === ',') {
      ev.preventDefault();
      commit(draft);
    } else if (ev.key === 'Backspace' && draft.length === 0 && value.length > 0) {
      ev.preventDefault();
      onChange(value.slice(0, -1));
    }
  };

  const onChangeInput = (ev: ChangeEvent<HTMLInputElement>) => setDraft(ev.target.value);

  const onBlur = () => {
    if (draft.trim()) commit(draft);
  };

  const remove = useCallback(
    (tag: string) => onChange(value.filter((t) => t !== tag)),
    [value, onChange],
  );

  return (
    <div className={css.inputWrapper}>
      <input
        type="text"
        value={draft}
        onChange={onChangeInput}
        onKeyDown={onKeyDown}
        onBlur={onBlur}
        placeholder={placeholder}
        aria-label={ariaLabel}
        className={inputClassName ?? css.input}
      />
      {value.length > 0 && <ComplianceTagChipList tags={value} onRemove={remove} />}
    </div>
  );
}
