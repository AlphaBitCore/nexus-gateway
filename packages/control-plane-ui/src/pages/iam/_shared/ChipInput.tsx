/**
 * ChipInput — token/tag input for newline-delimited string values.
 *
 * The IAM policy editor stores Actions and Resources as newline-delimited
 * strings (the IamPolicy document's Action[] / Resource[] arrays are
 * split on \n at save time — see iam-policy-document.ts). Older
 * those fields were rendered as plain <textarea>s, which made typos
 * invisible until Save and gave no autocomplete from the canonical
 * action catalog.
 *
 * This component renders the same newline-string as a chip list (one
 * chip per non-empty line), with:
 *
 *   • Autocomplete dropdown sourced from `suggestions` (the catalog).
 *   • Per-chip validation badge (red border on invalid chips, hover
 *     tooltip via title attr).
 *   • Enter / comma to commit; Backspace on empty input to remove last.
 *   • onChange always emits the canonical newline-joined string so the
 *     form schema and serializer don't need to know chips exist.
 */
import { useMemo, useRef, useState } from 'react';
import styles from '../_shared/ChipInput.module.css';

interface ChipInputProps {
  /** Current value as a newline-delimited string. */
  value: string;
  /** Emits the next newline-delimited string. */
  onChange: (next: string) => void;
  /** Autocomplete candidates. Filtered as the user types. */
  suggestions?: string[];
  /** Returns true if the chip value is considered valid (e.g. matches the canonical regex). */
  validate?: (chip: string) => boolean;
  /** Placeholder shown when no chips and no input. */
  placeholder?: string;
  /** Optional ARIA label for the underlying text input. */
  ariaLabel?: string;
  /** Hover tooltip for invalid chips. */
  invalidHint?: string;
}

const MAX_SUGGESTIONS = 12;

export function ChipInput({
  value,
  onChange,
  suggestions = [],
  validate,
  placeholder,
  ariaLabel,
  invalidHint = 'Not in the canonical catalog',
}: ChipInputProps) {
  const chips = useMemo(
    () => value.split('\n').map((s) => s.trim()).filter(Boolean),
    [value],
  );
  const [input, setInput] = useState('');
  const [focused, setFocused] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const inputRef = useRef<HTMLInputElement | null>(null);

  const filtered = useMemo(() => {
    const q = input.trim().toLowerCase();
    if (!q) return [];
    return suggestions
      .filter((s) => s.toLowerCase().includes(q) && !chips.includes(s))
      .slice(0, MAX_SUGGESTIONS);
  }, [input, suggestions, chips]);

  const addChip = (chip: string) => {
    const trimmed = chip.trim();
    if (!trimmed) return;
    if (chips.includes(trimmed)) {
      setInput('');
      return;
    }
    onChange([...chips, trimmed].join('\n'));
    setInput('');
    setHighlight(0);
  };

  const removeChip = (idx: number) => {
    onChange(chips.filter((_, i) => i !== idx).join('\n'));
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (filtered.length > 0 && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      e.preventDefault();
      setHighlight((h) => {
        const next = e.key === 'ArrowDown' ? h + 1 : h - 1;
        return (next + filtered.length) % filtered.length;
      });
      return;
    }
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      if (filtered.length > 0) {
        addChip(filtered[highlight] ?? filtered[0]);
      } else if (input.trim()) {
        addChip(input);
      }
      return;
    }
    if (e.key === 'Backspace' && input.length === 0 && chips.length > 0) {
      e.preventDefault();
      removeChip(chips.length - 1);
    }
  };

  return (
    <div
      className={styles.wrapper}
      onClick={() => inputRef.current?.focus()}
      role="presentation"
    >
      <div className={styles.chipList}>
        {chips.map((chip, i) => {
          const ok = !validate || validate(chip);
          return (
            <span
              key={chip + ':' + i}
              className={ok ? styles.chip : styles.chipInvalid}
              title={ok ? chip : `${chip} — ${invalidHint}`}
            >
              <span className={styles.chipText}>{chip}</span>
              <button
                type="button"
                className={styles.chipRemove}
                onClick={(e) => {
                  e.stopPropagation();
                  removeChip(i);
                }}
                aria-label={`Remove ${chip}`}
              >
                ×
              </button>
            </span>
          );
        })}
        <input
          ref={inputRef}
          type="text"
          className={styles.field}
          value={input}
          onChange={(e) => {
            setInput(e.target.value);
            setHighlight(0);
          }}
          onKeyDown={onKeyDown}
          onFocus={() => setFocused(true)}
          // Delay blur so a click on a suggestion can fire its mousedown handler first.
          onBlur={() => window.setTimeout(() => setFocused(false), 150)}
          placeholder={chips.length === 0 ? placeholder : ''}
          aria-label={ariaLabel}
          spellCheck={false}
          autoCorrect="off"
          autoCapitalize="off"
        />
      </div>
      {focused && filtered.length > 0 && (
        <ul className={styles.suggestions} role="listbox">
          {filtered.map((s, i) => (
            <li key={s}>
              <button
                type="button"
                role="option"
                aria-selected={i === highlight}
                className={i === highlight ? styles.suggestionActive : styles.suggestion}
                // mousedown rather than click so it fires before input's blur handler.
                onMouseDown={(e) => {
                  e.preventDefault();
                  addChip(s);
                }}
                onMouseEnter={() => setHighlight(i)}
              >
                {s}
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
