/**
 * OverrideEditorDrawer — right-side drawer that lets an admin add or edit a
 * single per-Thing config override. Mounted from ConfigurationTab when the
 * "+ Add override" / "Edit" actions fire.
 *
 * Layout (spec §8.3):
 *   - Right-aligned drawer, ~55vw / max 720px wide. Full-width on ≤900px.
 *   - Header: title `Override · <configKey> · <thingId>` + close (×).
 *   - Body: two stacked panes — read-only Template default (left/top) and
 *     editable JSON Override (right/bottom). The override pane has actions
 *     "Reset to template" + "Diff view ⇄ Editor view". When in diff mode the
 *     editor is replaced by a tiny line-level LCS diff (no external lib).
 *   - Footer: TTL preset select (with "Custom..." datetime-local fallback) +
 *     Reason input (≤500 chars; "break-glass:" prefix flags emergency) +
 *     Cancel/Save.
 *
 * Validation mirrors the server (Hub `setOverride`):
 *   - JSON must parse and the top-level value must be a plain object.
 *   - reason ≤ 500 chars.
 *   - expiresAt − NOW ∈ [5 minutes, 30 days] when set.
 *
 * Save flow:
 *   1. parse + validate locally (Save is gated on a clean state)
 *   2. `hubApi.setOverride(thingId, configKey, body)`
 *   3. on success → toast + onSaved() + onClose()
 *   4. on failure → surface server message inline; keep drawer open
 *
 * Keyboard:
 *   - Escape closes (matches existing drawers).
 *   - Cmd/Ctrl+Enter saves when the form is valid.
 */
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';
import { useTranslation } from 'react-i18next';
import { hubApi, type ThingOverride } from '@/api/services/infrastructure/nodes/hub';
import { Button } from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import styles from './OverrideEditorDrawer.module.css';

export type OverrideEditorMode = 'add' | 'edit';

export interface OverrideEditorDrawerProps {
  open: boolean;
  thingId: string;
  thingType: string;
  configKey: string;
  mode: OverrideEditorMode;
  templateState: unknown;
  templateVer: number;
  /** Existing override row; required when mode === 'edit'. */
  existingOverride?: ThingOverride;
  onClose: () => void;
  onSaved: () => void;
}

const DRAWER_MS = 240;

const FIVE_MINUTES_MS = 5 * 60 * 1000;
const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;
const REASON_MAX = 500;

interface TtlPreset {
  key: string;
  /** ms from NOW; null = permanent (no expiresAt). */
  durationMs: number | null;
}

const TTL_PRESETS: ReadonlyArray<TtlPreset> = [
  { key: 'permanent', durationMs: null },
  { key: 'h4',  durationMs: 4 * 60 * 60 * 1000 },
  { key: 'h8',  durationMs: 8 * 60 * 60 * 1000 },
  { key: 'h24', durationMs: 24 * 60 * 60 * 1000 },
  { key: 'd7',  durationMs: 7 * 24 * 60 * 60 * 1000 },
  { key: 'd30', durationMs: 30 * 24 * 60 * 60 * 1000 },
];

/** Returns the preset whose duration most closely matches `(expiresAt - NOW)`, or 'custom' if no preset is within ~10% tolerance. */
function bestFitTtlPresetKey(expiresAt: string | undefined): string {
  if (!expiresAt) return 'permanent';
  const ms = new Date(expiresAt).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return 'custom';
  let best: { key: string; diff: number } = { key: 'custom', diff: Infinity };
  for (const preset of TTL_PRESETS) {
    if (preset.durationMs === null) continue;
    const diff = Math.abs(ms - preset.durationMs);
    if (diff < preset.durationMs * 0.1 && diff < best.diff) {
      best = { key: preset.key, diff };
    }
  }
  return best.key;
}

/** Format a Date as a value compatible with `<input type="datetime-local">`: `YYYY-MM-DDTHH:mm`. */
function toDatetimeLocal(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

interface DiffLine {
  kind: 'same' | 'add' | 'remove';
  text: string;
}

/**
 * Tiny line-level diff using LCS (Longest Common Subsequence). Produces an
 * ordered list of {same|add|remove, text} entries — sufficient for a side-by-
 * side compare in v1. Quadratic in line count, fine for sub-1k-line JSON.
 */
function diffLines(a: string, b: string): DiffLine[] {
  const aLines = a.split('\n');
  const bLines = b.split('\n');
  const m = aLines.length;
  const n = bLines.length;
  // dp[i][j] = LCS length for aLines[i..] vs bLines[j..]
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array<number>(n + 1).fill(0));
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      if (aLines[i] === bLines[j]) {
        dp[i][j] = dp[i + 1][j + 1] + 1;
      } else {
        dp[i][j] = Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    if (aLines[i] === bLines[j]) {
      out.push({ kind: 'same', text: aLines[i] });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push({ kind: 'remove', text: aLines[i] });
      i++;
    } else {
      out.push({ kind: 'add', text: bLines[j] });
      j++;
    }
  }
  while (i < m) {
    out.push({ kind: 'remove', text: aLines[i++] });
  }
  while (j < n) {
    out.push({ kind: 'add', text: bLines[j++] });
  }
  return out;
}

interface ParsedJson {
  value: Record<string, unknown> | null;
  /** i18n key for the inline error, or undefined when JSON is valid + object. */
  errorKey?: string;
}

function parseJsonObject(text: string): ParsedJson {
  if (text.trim() === '') {
    return { value: null, errorKey: 'pages:infrastructure.editor.errors.invalidJson' };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { value: null, errorKey: 'pages:infrastructure.editor.errors.invalidJson' };
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { value: null, errorKey: 'pages:infrastructure.editor.errors.notObject' };
  }
  return { value: parsed as Record<string, unknown> };
}

export function OverrideEditorDrawer(props: OverrideEditorDrawerProps) {
  const {
    open,
    thingId,
    configKey,
    mode,
    templateState,
    templateVer,
    existingOverride,
    onClose,
    onSaved,
  } = props;

  const { t } = useTranslation();
  const { addToast } = useToast();

  const templateText = useMemo(
    () => JSON.stringify(templateState ?? {}, null, 2),
    [templateState],
  );

  const initialEditorText = useMemo(() => {
    if (mode === 'edit' && existingOverride) {
      return JSON.stringify(existingOverride.state ?? {}, null, 2);
    }
    return templateText;
  }, [mode, existingOverride, templateText]);

  const [editorText, setEditorText] = useState<string>(initialEditorText);
  const [reason, setReason] = useState<string>(existingOverride?.reason ?? '');
  const [ttlPreset, setTtlPreset] = useState<string>(() =>
    mode === 'edit' ? bestFitTtlPresetKey(existingOverride?.expiresAt) : 'permanent',
  );
  const [customExpiresAt, setCustomExpiresAt] = useState<string>(() => {
    if (mode === 'edit' && existingOverride?.expiresAt) {
      return toDatetimeLocal(new Date(existingOverride.expiresAt));
    }
    return '';
  });
  const [showDiff, setShowDiff] = useState<boolean>(false);
  const [saving, setSaving] = useState<boolean>(false);
  const [serverError, setServerError] = useState<string | null>(null);

  // Reset internal state every time the drawer opens fresh (covers the same
  // component being reused for a different key without an unmount cycle).
  const lastOpenKey = useRef<string>('');
  useEffect(() => {
    if (!open) return;
    const sig = `${mode}|${configKey}|${existingOverride?.expiresAt ?? ''}`;
    if (sig === lastOpenKey.current) return;
    lastOpenKey.current = sig;
    setEditorText(initialEditorText);
    setReason(existingOverride?.reason ?? '');
    setTtlPreset(mode === 'edit' ? bestFitTtlPresetKey(existingOverride?.expiresAt) : 'permanent');
    setCustomExpiresAt(
      mode === 'edit' && existingOverride?.expiresAt
        ? toDatetimeLocal(new Date(existingOverride.expiresAt))
        : '',
    );
    setShowDiff(false);
    setSaving(false);
    setServerError(null);
  }, [open, mode, configKey, existingOverride, initialEditorText]);

  const parsed = useMemo(() => parseJsonObject(editorText), [editorText]);

  // Compute a candidate `expiresAt` ISO string from the current TTL choice. We
  // also derive the validation error here so Save can be gated cleanly.
  const ttlInfo = useMemo<{ expiresAt?: string; errorKey?: string }>(() => {
    if (ttlPreset === 'permanent') return {};
    if (ttlPreset === 'custom') {
      if (!customExpiresAt) {
        return { errorKey: 'pages:infrastructure.editor.errors.ttlOutOfRange' };
      }
      const ms = new Date(customExpiresAt).getTime() - Date.now();
      if (!Number.isFinite(ms) || ms < FIVE_MINUTES_MS || ms > THIRTY_DAYS_MS) {
        return { errorKey: 'pages:infrastructure.editor.errors.ttlOutOfRange' };
      }
      return { expiresAt: new Date(customExpiresAt).toISOString() };
    }
    const preset = TTL_PRESETS.find((p) => p.key === ttlPreset);
    if (!preset || preset.durationMs === null) return {};
    return { expiresAt: new Date(Date.now() + preset.durationMs).toISOString() };
  }, [ttlPreset, customExpiresAt]);

  const reasonTooLong = reason.length > REASON_MAX;
  const isBreakGlass = reason.trimStart().toLowerCase().startsWith('breakglass:')
    || reason.trimStart().toLowerCase().startsWith('break-glass:');

  // Save is gated on JSON validity + reason length + TTL validity. (Reason is
  // not strictly required by the server, only ≤500; killswitch or break-glass
  // prefix surfaces as a visual hint, not a hard block.)
  const canSave = !parsed.errorKey && !reasonTooLong && !ttlInfo.errorKey && !saving;

  const handleSave = useCallback(async () => {
    if (!canSave || !parsed.value) return;
    setSaving(true);
    setServerError(null);
    try {
      await hubApi.setOverride(thingId, configKey, {
        state: parsed.value,
        reason: reason.trim() === '' ? undefined : reason,
        expiresAt: ttlInfo.expiresAt,
      });
      addToast(t('pages:infrastructure.editor.saveSuccess'), 'success');
      onSaved();
      onClose();
    } catch (err) {
      const message =
        err instanceof Error && err.message
          ? err.message
          : t('pages:infrastructure.editor.errors.serverFailure');
      setServerError(message);
    } finally {
      setSaving(false);
    }
  }, [canSave, parsed.value, thingId, configKey, reason, ttlInfo.expiresAt, addToast, t, onSaved, onClose]);

  // Escape closes; Cmd/Ctrl+Enter saves when valid.
  useEffect(() => {
    if (!open) return;
    const handler = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') {
        ev.stopPropagation();
        onClose();
        return;
      }
      if ((ev.metaKey || ev.ctrlKey) && ev.key === 'Enter' && canSave) {
        ev.preventDefault();
        void handleSave();
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [open, onClose, canSave, handleSave]);

  const handleResetToTemplate = useCallback(() => {
    setEditorText(templateText);
  }, [templateText]);

  const ttlOptions = useMemo(
    () => [
      ...TTL_PRESETS.map((p) => ({
        value: p.key,
        label:
          p.key === 'permanent'
            ? t('pages:infrastructure.editor.ttlPermanent')
            : t(`pages:infrastructure.editor.ttl.${p.key}`),
      })),
      { value: 'custom', label: t('pages:infrastructure.editor.ttlCustom') },
    ],
    [t],
  );

  const diffEntries = useMemo<DiffLine[]>(
    () => (showDiff ? diffLines(templateText, editorText) : []),
    [showDiff, templateText, editorText],
  );

  return (
    <>
      <div
        role="presentation"
        onClick={onClose}
        className={styles.backdrop}
        style={{
          opacity: open ? 1 : 0,
          transition: `opacity ${DRAWER_MS}ms cubic-bezier(0.4, 0, 0.2, 1)`,
          pointerEvents: open ? 'auto' : 'none',
        }}
        aria-hidden
      />
      <aside
        role="dialog"
        aria-modal="true"
        aria-labelledby="override-editor-title"
        className={styles.drawer}
        style={{
          transform: open ? 'translateX(0)' : 'translateX(100%)',
          transition: `transform ${DRAWER_MS}ms cubic-bezier(0.4, 0, 0.2, 1)`,
        }}
      >
        <div className={styles.header}>
          <h2 id="override-editor-title" className={styles.title}>
            {t('pages:infrastructure.editor.title', { configKey, thingId })}
          </h2>
          <button
            type="button"
            onClick={onClose}
            aria-label={t('pages:infrastructure.configuration.cancel', { defaultValue: 'Close' })}
            className={styles.closeBtn}
          >
            &times;
          </button>
        </div>

        <div className={styles.body}>
          {/* Template (read-only) */}
          <div className={styles.pane}>
            <div className={styles.paneHeader}>
              <span className={styles.paneLabel}>
                {t('pages:infrastructure.editor.templateLabel', { ver: templateVer })}
              </span>
            </div>
            <pre className={styles.templatePre} data-testid="override-editor-template">
              {templateText}
            </pre>
          </div>

          {/* Override (editable) */}
          <div className={styles.pane}>
            <div className={styles.paneHeader}>
              <span className={styles.paneLabel}>
                {t('pages:infrastructure.editor.overrideLabel')}
              </span>
              <div className={styles.paneActions}>
                <button
                  type="button"
                  onClick={handleResetToTemplate}
                  className={styles.linkBtn}
                  data-testid="override-editor-reset"
                >
                  {t('pages:infrastructure.editor.resetToTemplate')}
                </button>
                <button
                  type="button"
                  onClick={() => setShowDiff((v) => !v)}
                  className={styles.linkBtn}
                  data-testid="override-editor-toggle-diff"
                >
                  {showDiff
                    ? t('pages:infrastructure.editor.editorView')
                    : t('pages:infrastructure.editor.diffView')}
                </button>
              </div>
            </div>

            {showDiff ? (
              <div className={styles.diffView} data-testid="override-editor-diff">
                {diffEntries.length === 0 ? (
                  <span className={styles.diffSame}>
                    {t('pages:infrastructure.editor.diffSame')}
                  </span>
                ) : (
                  diffEntries.map((line, idx) => (
                    <div
                      key={`${line.kind}-${idx}-${line.text.slice(0, 32)}`}
                      className={
                        line.kind === 'add'
                          ? styles.diffAdd
                          : line.kind === 'remove'
                            ? styles.diffRemove
                            : styles.diffSame
                      }
                    >
                      <span className={styles.diffSign}>
                        {line.kind === 'add' ? '+' : line.kind === 'remove' ? '-' : ' '}
                      </span>
                      <span>{line.text || ' '}</span>
                    </div>
                  ))
                )}
              </div>
            ) : (
              <textarea
                className={styles.editor}
                value={editorText}
                onChange={(ev) => setEditorText(ev.target.value)}
                spellCheck={false}
                aria-label={t('pages:infrastructure.editor.overrideLabel')}
                data-testid="override-editor-textarea"
              />
            )}

            {parsed.errorKey && (
              <div className={styles.error} data-testid="override-editor-json-error">
                {t(parsed.errorKey)}
              </div>
            )}
          </div>
        </div>

        <div className={styles.footer}>
          <div className={styles.footerRow}>
            <label className={styles.fieldLabel} htmlFor="override-editor-ttl">
              {t('pages:infrastructure.editor.ttlLabel')}
            </label>
            <select
              id="override-editor-ttl"
              className={styles.ttlSelect}
              value={ttlPreset}
              onChange={(ev) => setTtlPreset(ev.target.value)}
              data-testid="override-editor-ttl"
            >
              {ttlOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
            {ttlPreset === 'custom' && (
              <input
                type="datetime-local"
                className={styles.ttlCustomInput}
                value={customExpiresAt}
                onChange={(ev) => setCustomExpiresAt(ev.target.value)}
                data-testid="override-editor-ttl-custom"
              />
            )}
          </div>

          <div className={styles.footerRow}>
            <label className={styles.fieldLabel} htmlFor="override-editor-reason">
              {t('pages:infrastructure.editor.reasonLabel')}
            </label>
            <input
              id="override-editor-reason"
              type="text"
              className={styles.reasonInput}
              value={reason}
              maxLength={REASON_MAX + 1 /* allow user to overshoot 1 char so guard message can render */}
              onChange={(ev) => setReason(ev.target.value)}
              placeholder={t('pages:infrastructure.editor.reasonPlaceholder')}
              aria-invalid={reasonTooLong || undefined}
              data-testid="override-editor-reason"
            />
            <span className={styles.reasonCounter}>
              {reason.length}/{REASON_MAX}
            </span>
          </div>

          {isBreakGlass && (
            <div className={styles.breakGlassHint}>
              {t('pages:infrastructure.editor.breakGlassHint')}
            </div>
          )}

          {reasonTooLong && (
            <div className={styles.error}>
              {t('pages:infrastructure.editor.errors.reasonTooLong')}
            </div>
          )}

          {ttlInfo.errorKey && (
            <div className={styles.error}>{t(ttlInfo.errorKey)}</div>
          )}

          {serverError && (
            <div className={styles.error} data-testid="override-editor-server-error">
              {serverError}
            </div>
          )}

          <div className={styles.footerActions}>
            <Button variant="ghost" size="sm" onClick={onClose}>
              {t('pages:infrastructure.editor.cancel')}
            </Button>
            <Button
              variant="primary"
              size="sm"
              loading={saving}
              disabled={!canSave}
              onClick={handleSave}
              data-testid="override-editor-save"
            >
              {t('pages:infrastructure.editor.save')}
            </Button>
          </div>
        </div>
      </aside>
    </>
  );
}
