import { useEffect, useId, useMemo, useRef, useState } from 'react';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { Checkbox } from '@/components/ui/Checkbox';
import styles from './MultiSelectDropdown.module.css';

export interface MultiSelectOption {
  value: string;
  label: string;
  /** Optional group name. Options with the same group are rendered under one header. */
  group?: string;
}

export interface MultiSelectDropdownProps {
  label: string;
  options: MultiSelectOption[];
  value: string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
  /** Placeholder when nothing selected */
  emptyLabel?: string;
  /** Render a search input at the top of the panel that filters options by label. */
  searchable?: boolean;
  /** Placeholder text for the search input. */
  searchPlaceholder?: string;
  className?: string;
}

/**
 * Dropdown multi-select with checkboxes (closes on outside click).
 * Supports optional inline search and group headers.
 */
export function MultiSelectDropdown({
  label,
  options,
  value,
  onChange,
  disabled,
  emptyLabel = 'Select…',
  searchable = false,
  searchPlaceholder = 'Search…',
  className,
}: MultiSelectDropdownProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const rootRef = useRef<HTMLDivElement>(null);
  const listId = useId();
  const btnId = useId();

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current?.contains(e.target as Node)) return;
      setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  useEffect(() => {
    if (!open) setQuery('');
  }, [open]);

  const toggle = (v: string) => {
    if (value.includes(v)) onChange(value.filter((x) => x !== v));
    else onChange([...value, v]);
  };

  const filtered = useMemo(() => {
    if (!searchable || !query.trim()) return options;
    const q = query.trim().toLowerCase();
    return options.filter((o) => o.label.toLowerCase().includes(q));
  }, [options, searchable, query]);

  const grouped = useMemo(() => {
    const hasGroups = filtered.some((o) => o.group);
    if (!hasGroups) return [{ group: undefined as string | undefined, items: filtered }];
    const map = new Map<string, MultiSelectOption[]>();
    const order: string[] = [];
    for (const o of filtered) {
      const g = o.group ?? '';
      if (!map.has(g)) {
        map.set(g, []);
        order.push(g);
      }
      map.get(g)!.push(o);
    }
    return order.map((g) => ({ group: g || undefined, items: map.get(g)! }));
  }, [filtered]);

  const summary =
    value.length === 0
      ? emptyLabel
      : value
          .map((v) => options.find((o) => o.value === v)?.label ?? v)
          .join(', ');

  return (
    <div ref={rootRef} className={clsx(styles.root, className)}>
      <label htmlFor={btnId} className={styles.label}>
        {label}
      </label>
      <button data-design-system-escape="primitive-internal"
        id={btnId}
        type="button"
        disabled={disabled}
        aria-expanded={open}
        aria-haspopup="listbox"
        aria-controls={listId}
        onClick={(e) => {
          e.stopPropagation();
          if (!disabled) setOpen((o) => !o);
        }}
        className={styles.trigger}
      >
        <span className={clsx(styles.triggerText, value.length === 0 && styles.triggerPlaceholder)}>{summary}</span>
        <span aria-hidden className={styles.triggerArrow}>
          &#x25BC;
        </span>
      </button>
      {open && !disabled && (
        <div
          id={listId}
          role="listbox"
          aria-multiselectable
          className={styles.panel}
          onMouseDown={(e) => e.preventDefault()}
        >
          {searchable && (
            <div className={styles.searchWrap}>
              <input
                type="text"
                className={styles.searchInput}
                placeholder={searchPlaceholder}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onMouseDown={(e) => e.stopPropagation()}
                aria-label={searchPlaceholder}
              />
            </div>
          )}
          {filtered.length === 0 ? (
            <div className={styles.emptyMessage}>{t('common:multiSelect.noOptions')}</div>
          ) : (
            grouped.map((section) => (
              <div key={section.group ?? '__nogroup'}>
                {section.group && (
                  <div className={styles.groupHeader}>{section.group}</div>
                )}
                {section.items.map((opt) => {
                  const checked = value.includes(opt.value);
                  return (
                    <label
                      key={opt.value}
                      role="option"
                      aria-selected={checked}
                      className={clsx(
                        styles.optionRow,
                        checked && styles.optionRowSelected,
                      )}
                    >
                      <Checkbox
                        checked={checked}
                        onCheckedChange={() => toggle(opt.value)}
                      />
                      <span>{opt.label}</span>
                    </label>
                  );
                })}
              </div>
            ))
          )}
        </div>
      )}
    </div>
  );
}
