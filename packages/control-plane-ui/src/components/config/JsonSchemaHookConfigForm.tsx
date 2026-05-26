/**
 * Renders hook `config` fields from a gateway `configSchema` (JSON Schema draft-07 subset).
 */

import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { FormField, Input, Select, Switch, Textarea } from '@/components/ui';
import styles from './JsonSchemaHookConfigForm.module.css';

interface KeyVal {
  key: string;
  value: string;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function schemaProperty(schema: Record<string, unknown>, key: string): Record<string, unknown> | null {
  const props = schema.properties;
  if (!isPlainObject(props) || !isPlainObject(props[key])) return null;
  return props[key] as Record<string, unknown>;
}

function defaultForProperty(sub: Record<string, unknown>): unknown {
  if ('default' in sub) return sub.default;
  if (Array.isArray(sub.enum) && sub.enum.length > 0) return sub.enum[0];
  const t = sub.type;
  if (t === 'boolean') return false;
  if (t === 'number' || t === 'integer') return undefined;
  if (t === 'array') return [];
  if (t === 'object') return {};
  return '';
}

export function buildDefaultsFromSchema(schema: Record<string, unknown>): Record<string, unknown> {
  const props = schema.properties;
  if (!isPlainObject(props)) return {};
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(props)) {
    const sub = props[key];
    if (isPlainObject(sub)) {
      const d = defaultForProperty(sub);
      if (d !== undefined) out[key] = d;
    }
  }
  return out;
}

interface JsonSchemaHookConfigFormProps {
  schema: Record<string, unknown>;
  value: Record<string, unknown>;
  onChange: (next: Record<string, unknown>) => void;
}

export function JsonSchemaHookConfigForm({ schema, value, onChange }: JsonSchemaHookConfigFormProps) {
  const { t } = useTranslation();
  const required = Array.isArray(schema.required)
    ? new Set((schema.required as unknown[]).map((x) => String(x)))
    : new Set<string>();

  const patch = useCallback(
    (key: string, v: unknown) => {
      onChange({ ...value, [key]: v });
    },
    [onChange, value],
  );

  const props = schema.properties;
  if (!isPlainObject(props)) {
    return (
      <FormField label={t('pages:hooks.configJsonLabel', 'Config JSON')}>
        <Textarea
          name="config-fallback"
          value={JSON.stringify(value, null, 2)}
          onChange={(e) => {
            try {
              const parsed = JSON.parse(e.target.value) as Record<string, unknown>;
              if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) onChange(parsed);
            } catch {
              /* keep typing */
            }
          }}
          className={styles.monoTextarea}
        />
      </FormField>
    );
  }

  const keys = Object.keys(props);

  const renderOnMatch = (key: string, desc: string | undefined) => {
    const obj = isPlainObject(value[key]) ? (value[key] as Record<string, unknown>) : {};
    const inflight = typeof obj.inflightAction === 'string' && obj.inflightAction !== '' ? obj.inflightAction : 'block-hard';
    const storage = typeof obj.storageAction === 'string' && obj.storageAction !== '' ? obj.storageAction : 'keep';
    const replacement = typeof obj.replacement === 'string' ? obj.replacement : '';
    const showReplacement = inflight === 'redact' || storage === 'redact';
    const update = (patchKey: 'inflightAction' | 'storageAction' | 'replacement', val: string) => {
      const next: Record<string, unknown> = { ...obj, [patchKey]: val };
      // Drop empty replacement so we don't persist an empty string when not used.
      if (patchKey === 'replacement' && val === '') delete next.replacement;
      patch(key, next);
    };
    return (
      <fieldset key={key} className={styles.onMatchSection}>
        <legend className={styles.onMatchLegend}>
          {t('pages:hooks.form.onMatchSection')}
        </legend>
        <p className={styles.onMatchHelp}>{desc || t('pages:hooks.form.onMatchHelp')}</p>
        <FormField label={t('pages:hooks.form.inflightAction')} helpText={undefined}>
          <Select
            value={inflight}
            onValueChange={(v) => update('inflightAction', v)}
            options={[
              { value: 'approve', label: t('pages:hooks.form.inflightApprove') },
              { value: 'block-hard', label: t('pages:hooks.form.inflightBlockHard') },
              { value: 'block-soft', label: t('pages:hooks.form.inflightBlockSoft') },
              { value: 'redact', label: t('pages:hooks.form.inflightRedact') },
            ]}
          />
        </FormField>
        <FormField label={t('pages:hooks.form.storageAction')} helpText={undefined}>
          <Select
            value={storage}
            onValueChange={(v) => update('storageAction', v)}
            options={[
              { value: 'keep', label: t('pages:hooks.form.storageKeep') },
              { value: 'redact', label: t('pages:hooks.form.storageRedact') },
              { value: 'drop-content', label: t('pages:hooks.form.storageDropContent') },
            ]}
          />
        </FormField>
        {showReplacement ? (
          <FormField
            label={t('pages:hooks.form.replacement')}
            helpText={t('pages:hooks.form.replacementHelp')}
          >
            <Input
              name={`${key}-replacement`}
              value={replacement}
              onChange={(e) => update('replacement', e.target.value)}
              placeholder="[REDACTED_<RULE_ID>]"
            />
          </FormField>
        ) : null}
      </fieldset>
    );
  };

  return (
    <>
      {keys.map((key) => {
        const sub = schemaProperty(schema, key);
        if (!sub) return null;
        const label = (typeof sub.title === 'string' ? sub.title : key);
        const isReq = required.has(key);
        const desc = typeof sub.description === 'string' ? sub.description : undefined;
        const propType = sub.type;
        const enumVals = Array.isArray(sub.enum) ? (sub.enum as unknown[]).map((x) => String(x)) : null;

        // onMatch — render structured editor instead of a raw JSON textarea
        // so operators can select inflight/storage actions from a dropdown.
        if (key === 'onMatch' && propType === 'object') {
          return renderOnMatch(key, desc);
        }

        if (enumVals && enumVals.length > 0) {
          const cur = value[key] !== undefined && value[key] !== null ? String(value[key]) : String(enumVals[0] ?? '');
          return (
            <FormField key={key} label={label} required={isReq} helpText={desc}>
              <Select
                value={cur}
                onValueChange={(v) => patch(key, v)}
                options={enumVals.map((ev) => ({ value: ev, label: ev }))}
              />
            </FormField>
          );
        }

        if (propType === 'boolean') {
          const cur = value[key] === true;
          return (
            <FormField key={key} label={label} helpText={desc}>
              <Switch
                checked={cur}
                onCheckedChange={(c) => patch(key, c)}
              />
            </FormField>
          );
        }

        if (propType === 'number' || propType === 'integer') {
          const cur = value[key] !== undefined && value[key] !== null ? String(value[key]) : '';
          return (
            <FormField key={key} label={label} required={isReq} helpText={desc}>
              <Input
                name={key}
                type="number"
                value={cur}
                onChange={(e) => patch(key, e.target.value === '' ? undefined : Number(e.target.value))}
              />
            </FormField>
          );
        }

        if (propType === 'array' && isPlainObject(sub.items) && sub.items.type === 'string') {
          const arr = Array.isArray(value[key]) ? (value[key] as unknown[]).filter((x): x is string => typeof x === 'string') : [];
          const text = arr.join('\n');
          return (
            <FormField key={key} label={label} helpText={desc}>
              <Textarea
                name={key}
                value={text}
                onChange={(e) => {
                  const lines = e.target.value
                    .split('\n')
                    .map((s) => s.trim())
                    .filter(Boolean);
                  patch(key, lines);
                }}
                placeholder={t('pages:hooks.form.placeholderOneValuePerLine')}
                className={styles.textarea}
              />
            </FormField>
          );
        }

        if (propType === 'array') {
          const raw =
            value[key] !== undefined
              ? JSON.stringify(value[key], null, 2)
              : '[]';
          return (
            <FormField key={key} label={`${label} (JSON)`} helpText={desc}>
              <Textarea
                name={key}
                value={raw}
                onChange={(e) => {
                  try {
                    patch(key, JSON.parse(e.target.value) as unknown);
                  } catch {
                    /* ignore */
                  }
                }}
                className={styles.monoTextarea}
              />
            </FormField>
          );
        }

        if (
          propType === 'object' &&
          isPlainObject(sub.additionalProperties) &&
          (sub.additionalProperties as { type?: string }).type === 'string'
        ) {
          const obj = isPlainObject(value[key]) ? (value[key] as Record<string, string>) : {};
          const pairs: KeyVal[] = Object.entries(obj).map(([k, v]) => ({ key: k, value: String(v) }));
          const list = pairs.length > 0 ? pairs : [{ key: '', value: '' }];

          const sync = (rows: KeyVal[]) => {
            const out: Record<string, string> = {};
            for (const r of rows) {
              if (r.key.trim()) out[r.key.trim()] = r.value;
            }
            patch(key, out);
          };

          return (
            <FormField key={key} label={label} helpText={desc}>
              <div>
                {list.map((row, idx) => (
                  <div key={idx} className={styles.inputRow}>
                    <Input
                      placeholder={t('pages:hooks.form.placeholderName')}
                      value={row.key}
                      onChange={(e) => {
                        const next = [...list];
                        next[idx] = { ...row, key: e.target.value };
                        sync(next);
                      }}
                      className={styles.kvInput}
                    />
                    <Input
                      placeholder={t('pages:hooks.form.placeholderValue')}
                      value={row.value}
                      onChange={(e) => {
                        const next = [...list];
                        next[idx] = { ...row, value: e.target.value };
                        sync(next);
                      }}
                      className={styles.kvInput}
                    />
                    <button data-design-system-escape="primitive-internal"
                      type="button"
                      onClick={() => sync(list.filter((_, i) => i !== idx))}
                      className={styles.removeBtn}
                    >
                      Remove
                    </button>
                  </div>
                ))}
                <button data-design-system-escape="primitive-internal"
                  type="button"
                  onClick={() => sync([...list, { key: '', value: '' }])}
                  className={styles.smallBtn}
                >
                  + Add
                </button>
              </div>
            </FormField>
          );
        }

        if (propType === 'object') {
          const raw =
            value[key] !== undefined ? JSON.stringify(value[key], null, 2) : '{}';
          return (
            <FormField key={key} label={`${label} (JSON)`} helpText={desc}>
              <Textarea
                name={key}
                value={raw}
                onChange={(e) => {
                  try {
                    patch(key, JSON.parse(e.target.value) as unknown);
                  } catch {
                    /* ignore */
                  }
                }}
                className={styles.monoTextarea}
              />
            </FormField>
          );
        }

        // string (default)
        const cur = value[key] !== undefined && value[key] !== null ? String(value[key]) : '';
        const inputType = sub.format === 'uri' || sub.format === 'url' ? 'url' : 'text';
        return (
          <FormField key={key} label={label} required={isReq} helpText={desc}>
            <Input
              name={key}
              type={inputType}
              value={cur}
              onChange={(e) => patch(key, e.target.value)}
            />
          </FormField>
        );
      })}
    </>
  );
}
