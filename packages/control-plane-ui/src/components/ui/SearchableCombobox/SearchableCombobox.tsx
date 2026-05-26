import {
  useState,
  useEffect,
  useRef,
  useCallback,
  useId,
  type KeyboardEvent,
} from 'react';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import styles from './SearchableCombobox.module.css';

export interface ComboboxOption {
  id: string;
  label: string;
  /** Extra payload (e.g. virtual key name alongside id). */
  meta?: { name?: string };
}

export interface SearchableComboboxProps {
  valueId: string;
  valueLabel: string;
  placeholder: string;
  ariaLabel: string;
  disabled?: boolean;
  /** Load options for the current search string (debounced). */
  fetchOptions: (query: string) => Promise<ComboboxOption[]>;
  /** `null` when the user clears. */
  onSelect: (option: ComboboxOption | null) => void;
  /** When true, empty query still fetches (e.g. initial open). */
  allowEmptyQueryFetch?: boolean;
  className?: string;
}

export function SearchableCombobox({
  valueId,
  valueLabel,
  placeholder,
  ariaLabel,
  disabled = false,
  fetchOptions,
  onSelect,
  allowEmptyQueryFetch = false,
  className,
}: SearchableComboboxProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState(valueLabel);
  const [options, setOptions] = useState<ComboboxOption[]>([]);
  const [loading, setLoading] = useState(false);
  const [highlightIndex, setHighlightIndex] = useState(-1);
  const wrapRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  const listboxId = useId();

  useEffect(() => {
    setQuery(valueLabel);
  }, [valueLabel, valueId]);

  useEffect(() => {
    const onDoc = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node))
        setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, []);

  // Reset highlight when options change
  useEffect(() => {
    setHighlightIndex(-1);
  }, [options]);

  const runFetch = useCallback(
    async (q: string) => {
      if (!allowEmptyQueryFetch && !q.trim()) {
        setOptions([]);
        return;
      }
      setLoading(true);
      try {
        const rows = await fetchOptions(q.trim());
        setOptions(rows);
      } catch {
        setOptions([]);
      } finally {
        setLoading(false);
      }
    },
    [fetchOptions, allowEmptyQueryFetch],
  );

  useEffect(() => {
    if (!open) return;
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      void runFetch(query);
    }, 200);
    return () => clearTimeout(debounceRef.current);
  }, [open, query, runFetch]);

  // Scroll highlighted option into view
  useEffect(() => {
    if (highlightIndex < 0 || !listRef.current) return;
    const items = listRef.current.querySelectorAll('[role="option"]');
    items[highlightIndex]?.scrollIntoView({ block: 'nearest' });
  }, [highlightIndex]);

  const selectOption = (opt: ComboboxOption) => {
    onSelect(opt);
    setQuery(opt.label);
    setOpen(false);
    setHighlightIndex(-1);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    switch (e.key) {
      case 'Escape':
        setOpen(false);
        setHighlightIndex(-1);
        break;
      case 'ArrowDown':
        e.preventDefault();
        if (!open) {
          setOpen(true);
        } else {
          setHighlightIndex((prev) =>
            prev < options.length - 1 ? prev + 1 : 0,
          );
        }
        break;
      case 'ArrowUp':
        e.preventDefault();
        if (open) {
          setHighlightIndex((prev) =>
            prev > 0 ? prev - 1 : options.length - 1,
          );
        }
        break;
      case 'Home':
        if (open && options.length > 0) {
          e.preventDefault();
          setHighlightIndex(0);
        }
        break;
      case 'End':
        if (open && options.length > 0) {
          e.preventDefault();
          setHighlightIndex(options.length - 1);
        }
        break;
      case 'Enter':
        if (open && highlightIndex >= 0 && options[highlightIndex]) {
          e.preventDefault();
          selectOption(options[highlightIndex]);
        }
        break;
    }
  };

  const showInlineClear = Boolean((valueId || query.trim()) && !disabled);
  const activeDescendant =
    open && highlightIndex >= 0 && options[highlightIndex]
      ? `${listboxId}-opt-${highlightIndex}`
      : undefined;

  return (
    <div ref={wrapRef} className={clsx(styles.root, className)}>
      <div className={styles.inputWrapper}>
        <input
          type="text"
          enterKeyHint="search"
          autoComplete="one-time-code"
          name={listboxId}
          aria-label={ariaLabel}
          aria-expanded={open}
          aria-autocomplete="list"
          aria-controls={open ? listboxId : undefined}
          aria-activedescendant={activeDescendant}
          role="combobox"
          disabled={disabled}
          placeholder={placeholder}
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onKeyDown={onKeyDown}
          className={clsx(styles.input, showInlineClear && styles.inputWithClear)}
        />
        {showInlineClear && (
          <button data-design-system-escape="primitive-internal"
            type="button"
            aria-label={t('common:clear')}
            title={t('common:clear')}
            onMouseDown={(ev) => ev.preventDefault()}
            onClick={() => {
              setQuery('');
              onSelect(null);
              setOptions([]);
              setHighlightIndex(-1);
            }}
            className={styles.clearButton}
          >
            &times;
          </button>
        )}
      </div>
      {open && !disabled && (
        <div ref={listRef} id={listboxId} className={styles.dropdown} role="listbox">
          {loading ? (
            <div className={styles.statusMessage} aria-live="polite">{t('common:loading')}</div>
          ) : options.length === 0 ? (
            <div className={styles.statusMessage} aria-live="polite">
              {allowEmptyQueryFetch || query.trim()
                ? 'No matches'
                : 'Type to search'}
            </div>
          ) : (
            options.map((opt, idx) => (
              <button data-design-system-escape="primitive-internal"
                key={opt.id}
                id={`${listboxId}-opt-${idx}`}
                type="button"
                role="option"
                aria-selected={opt.id === valueId}
                onMouseDown={(ev) => ev.preventDefault()}
                onMouseEnter={() => setHighlightIndex(idx)}
                onClick={() => selectOption(opt)}
                className={clsx(
                  styles.option,
                  opt.id === valueId && styles.optionSelected,
                  idx === highlightIndex && styles.optionHighlighted,
                )}
              >
                {opt.label}
              </button>
            ))
          )}
        </div>
      )}
    </div>
  );
}
