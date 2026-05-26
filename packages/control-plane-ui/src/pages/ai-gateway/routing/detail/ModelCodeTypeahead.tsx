import { useEffect, useRef, useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { systemApi } from '@/api/services';
import type { Model } from '@/api/types';
import styles from './ModelCodeTypeahead.module.css';

interface ModelSuggestion extends Model {
  providerDisplay: string;
}

export interface ModelCodeTypeaheadProps {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  ariaLabel?: string;
}

const SUGGESTION_LIMIT = 8;

// Free-text input with model-code suggestions. Free-text is the source of
// truth (so operators can type "auto" or any code the catalog doesn't yet
// know about); suggestions are a convenience that fill the input with
// Model.code on click.
export function ModelCodeTypeahead({ value, onChange, placeholder, ariaLabel }: ModelCodeTypeaheadProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [suggestions, setSuggestions] = useState<ModelSuggestion[]>([]);
  const [loading, setLoading] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  const trimmed = value.trim();
  const skipSuggest = trimmed === '' || trimmed.toLowerCase() === 'auto';
  const debouncedQuery = useDebouncedValue(skipSuggest ? '' : trimmed, 200);

  useEffect(() => {
    if (!debouncedQuery) {
      setSuggestions([]);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    systemApi
      .listModelsFlat({ q: debouncedQuery, limit: String(SUGGESTION_LIMIT), offset: '0' })
      .then((res) => {
        if (cancelled) return;
        setSuggestions(res.data ?? []);
      })
      .catch(() => {
        if (cancelled) return;
        setSuggestions([]);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [debouncedQuery]);

  useEffect(() => {
    const onDocMouseDown = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDocMouseDown);
    return () => document.removeEventListener('mousedown', onDocMouseDown);
  }, []);

  const handlePick = useCallback(
    (s: ModelSuggestion) => {
      onChange(s.code);
      setOpen(false);
    },
    [onChange],
  );

  const showPopover = open && !skipSuggest && (loading || suggestions.length > 0);

  return (
    <div ref={wrapRef} className={styles.wrap}>
      <Input
        value={value}
        onChange={(e) => {
          onChange(e.target.value);
          setOpen(true);
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') setOpen(false);
        }}
        placeholder={placeholder}
        aria-label={ariaLabel}
        autoComplete="off"
      />
      {showPopover && (
        <div className={styles.popover} role="listbox">
          {loading && suggestions.length === 0 ? (
            <div className={styles.statusRow}>{t('pages:routing.simModelLoading')}</div>
          ) : (
            suggestions.map((s) => (
              <button
                key={s.id}
                type="button"
                className={styles.row}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => handlePick(s)}
                role="option"
                aria-selected={value === s.code}
              >
                <span className={styles.code}>{s.code}</span>
                <span className={styles.name}>{s.name}</span>
                <span className={styles.provider}>{s.providerDisplay}</span>
              </button>
            ))
          )}
        </div>
      )}
    </div>
  );
}
