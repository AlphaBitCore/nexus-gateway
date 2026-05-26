/**
 * GenericJsonEditor — schema-driven fallback editor for rules that don't
 * have (or don't need) a bespoke editor.
 *
 * Behaviour:
 *   - If `schema.type === 'object'` and `schema.properties` is a
 *     `Record<string, { type: string; ... }>`, render one input per property
 *     based on the declared `type`:
 *       integer / number → number input (coerce to Number on change)
 *       string           → text input
 *       boolean          → checkbox
 *       array            → comma-separated text input; strings-of-strings
 *                          for `items.type === 'string'`, numbers otherwise
 *       anything else    → JSON textarea (bound to JSON.stringify(value))
 *   - If the schema is unusable (empty, missing, wrong shape, or schema lists
 *     no properties), fall back to a single JSON textarea bound to
 *     `JSON.stringify(value, null, 2)`. onBlur: try `JSON.parse` and only
 *     call `onChange` if parse succeeds.
 *
 * This is deliberately conservative: Hub does the authoritative validation
 * server-side via `validateParamsAgainstSchema`. We just try to keep the
 * user's input within the declared shape.
 */
import { useCallback, useEffect, useMemo, useState, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { FormField, Input, Stack, Checkbox, Textarea } from '@/components/ui';
import type { RuleEditorProps } from './types';

type PropDef = {
  type?: string;
  items?: { type?: string };
  minimum?: number;
  maximum?: number;
};

function isPropsMap(obj: unknown): obj is Record<string, PropDef> {
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return false;
  return Object.values(obj).every(
    (v) => v && typeof v === 'object' && !Array.isArray(v),
  );
}

function parseArrayItems(raw: string, itemType: string | undefined): unknown[] {
  const tokens = raw
    .split(',')
    .map((t) => t.trim())
    .filter((t) => t.length > 0);
  if (itemType === 'integer' || itemType === 'number') {
    return tokens.map((t) => Number(t)).filter((n) => Number.isFinite(n));
  }
  return tokens;
}

function formatArrayItems(arr: unknown): string {
  if (!Array.isArray(arr)) return '';
  return arr.map((v) => String(v)).join(', ');
}

export function GenericJsonEditor({ value, schema, onChange, onValidate }: RuleEditorProps) {
  const { t } = useTranslation();

  const properties = useMemo<Record<string, PropDef> | null>(() => {
    if (schema.type !== 'object') return null;
    const props = (schema as { properties?: unknown }).properties;
    if (!isPropsMap(props)) return null;
    if (Object.keys(props).length === 0) return null;
    return props;
  }, [schema]);

  // Schema-driven path.
  if (properties) {
    return (
      <Stack gap="md">
        {Object.entries(properties).map(([key, def]) => (
          <PropertyField
            key={key}
            name={key}
            def={def}
            value={value[key]}
            onFieldChange={(next) => {
              onChange({ ...value, [key]: next });
              onValidate?.(true);
            }}
          />
        ))}
      </Stack>
    );
  }

  // Fallback: raw JSON textarea.
  return (
    <JsonFallback value={value} onChange={onChange} onValidate={onValidate} label={t('pages:alerts.ruleEditors.generic.jsonLabel')} helpText={t('pages:alerts.ruleEditors.generic.jsonHelp')} />
  );
}

/* ── Per-property renderer ──────────────────────────────────────────────── */

function PropertyField({
  name,
  def,
  value,
  onFieldChange,
}: {
  name: string;
  def: PropDef;
  value: unknown;
  onFieldChange: (next: unknown) => void;
}) {
  const type = def.type;

  if (type === 'integer' || type === 'number') {
    return (
      <FormField label={name}>
        <Input
          type="number"
          min={def.minimum}
          max={def.maximum}
          step={type === 'integer' ? 1 : undefined}
          value={typeof value === 'number' ? String(value) : ''}
          onChange={(e: ChangeEvent<HTMLInputElement>) => {
            const raw = e.target.value;
            if (raw === '') {
              onFieldChange(undefined);
              return;
            }
            const n = Number(raw);
            if (Number.isFinite(n)) onFieldChange(n);
          }}
          aria-label={name}
        />
      </FormField>
    );
  }

  if (type === 'boolean') {
    return (
      <FormField label={name}>
        <Checkbox
          checked={value === true}
          onCheckedChange={(checked) => onFieldChange(checked === true)}
        />
      </FormField>
    );
  }

  if (type === 'array') {
    const itemType = def.items?.type;
    return (
      <FormField label={name}>
        <Input
          type="text"
          value={formatArrayItems(value)}
          onChange={(e: ChangeEvent<HTMLInputElement>) =>
            onFieldChange(parseArrayItems(e.target.value, itemType))
          }
          aria-label={name}
        />
      </FormField>
    );
  }

  // Default: string.
  return (
    <FormField label={name}>
      <Input
        type="text"
        value={typeof value === 'string' ? value : ''}
        onChange={(e: ChangeEvent<HTMLInputElement>) => onFieldChange(e.target.value)}
        aria-label={name}
      />
    </FormField>
  );
}

/* ── Raw-JSON fallback ──────────────────────────────────────────────────── */

function JsonFallback({
  value,
  onChange,
  onValidate,
  label,
  helpText,
}: {
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
  onValidate?: (ok: boolean) => void;
  label: string;
  helpText: string;
}) {
  const [raw, setRaw] = useState(() => JSON.stringify(value, null, 2));
  const [err, setErr] = useState<string | null>(null);
  const { t } = useTranslation();

  // Keep local state in sync if `value` changes from outside (e.g. after Reset).
  useEffect(() => {
    setRaw(JSON.stringify(value, null, 2));
    setErr(null);
  }, [value]);

  const onBlur = useCallback(() => {
    try {
      const parsed = JSON.parse(raw);
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        setErr(t('pages:alerts.ruleEditors.generic.jsonMustBeObject'));
        onValidate?.(false);
        return;
      }
      setErr(null);
      onChange(parsed as Record<string, unknown>);
      onValidate?.(true);
    } catch (e) {
      setErr(e instanceof Error ? e.message : t('pages:alerts.ruleEditors.generic.jsonParseError'));
      onValidate?.(false);
    }
  }, [onChange, onValidate, raw, t]);

  return (
    <FormField label={label} helpText={helpText} error={err ?? undefined}>
      <Textarea
        value={raw}
        onChange={(e) => setRaw(e.target.value)}
        onBlur={onBlur}
        rows={8}
        aria-label={label}
      />
    </FormField>
  );
}
