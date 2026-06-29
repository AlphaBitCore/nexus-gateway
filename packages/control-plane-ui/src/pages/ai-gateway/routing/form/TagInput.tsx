/**
 * TagInput — free-text string-array editor.
 *
 * The user types a value, presses Enter (or comma), and the input commits
 * as a chip. Backspace on an empty input removes the last chip. Designed
 * for routing-rule MatchConditions fields that accept arbitrary strings
 * (requestedModelLiterals, virtualKeys glob patterns) — see
 * MatchConditionExtraFields.
 *
 * Lives under `form/` rather than `components/ui/` because it's only
 * used by routing today. Promote upward once a second consumer needs it.
 */
import { useState, useCallback, type KeyboardEvent } from 'react';
import styles from './TagInput.module.css';

export interface TagInputProps {
  value: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  ariaLabel?: string;
  className?: string;
}

export function TagInput({ value, onChange, placeholder, ariaLabel, className }: TagInputProps) {
  const [draft, setDraft] = useState('');

  const commit = useCallback(
    (raw: string) => {
      const t = raw.trim();
      if (!t) return;
      // De-dup: ignore exact duplicates so a careless Enter doesn't bloat
      // the rule. Order is preserved, first occurrence wins.
      if (value.includes(t)) {
        setDraft('');
        return;
      }
      onChange([...value, t]);
      setDraft('');
    },
    [value, onChange],
  );

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commit(draft);
      return;
    }
    if (e.key === 'Backspace' && draft === '' && value.length > 0) {
      // Remove the last chip when the user backspaces into an empty input —
      // standard chip-input affordance, no confirmation prompt needed.
      onChange(value.slice(0, -1));
    }
  };

  const removeAt = (idx: number) => {
    onChange(value.filter((_, i) => i !== idx));
  };

  return (
    <div className={`${styles.tagInput} ${className ?? ''}`}>
      {value.map((tag, idx) => (
        <span key={`${tag}-${idx}`} className={styles.tag}>
          {tag}
          <button
            type="button"
            className={styles.removeBtn}
            onClick={() => removeAt(idx)}
            aria-label={`Remove ${tag}`}
          >
            ×
          </button>
        </span>
      ))}
      <input
        className={styles.input}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => commit(draft)}
        placeholder={value.length === 0 ? placeholder : ''}
        aria-label={ariaLabel}
      />
    </div>
  );
}
