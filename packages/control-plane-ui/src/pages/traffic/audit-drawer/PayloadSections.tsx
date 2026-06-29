import type { SpillRef } from '@/api/types';
import { Stack } from '@/components/ui';
import { CopyJsonButton } from '../../governance/adminAuditLogShared';
import css from './trafficAuditDrawer.module.css';

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 2 : 1)} ${units[i]}`;
}

// ── JSON / payload sections ──────────────────────────────────────────────────

export function JsonSection({ label, value }: { label: string; value: unknown }) {
  if (value == null) return null;
  if (Array.isArray(value) && value.length === 0) return null;
  if (typeof value === 'object' && !Array.isArray(value) && Object.keys(value as object).length === 0) return null;
  const text = JSON.stringify(value, null, 2);
  if (!text || text === '{}' || text === '[]') return null;
  return (
    <div className={css.jsonSectionWrap}>
      <Stack direction="horizontal" justify="between" align="center" className={css.jsonSectionHeader}>
        <strong className={css.jsonSectionLabel}>{label}</strong>
        <CopyJsonButton json={text} />
      </Stack>
      <pre className={css.preBlockLarge}>{text}</pre>
    </div>
  );
}

// PayloadSection renders request/response bodies. Bodies are captured as
// raw bytes (BYTEA) on the server and surfaced through the detail API, so
// they may arrive as JSON objects/arrays, JSON-encoded strings,
// numbers, or null. We don't assume any particular shape — strings are
// shown verbatim (without surrounding JSON quotes); structured values
// are pretty-printed; nullish / empty values are skipped silently.
//
// When `spillRef` is non-null the body was originally stored out-of-band
// (large captured payload). The CP detail handler resolves the ref and
// inlines the bytes onto `value`, but the ref metadata (backend, key,
// size, sha256) is also threaded through so the drawer can show a
// "Stored externally" badge — matters for ops who want to know whether
// a body sits in a shared bucket vs inline in the database.
export function PayloadSection({ label, value, spillRef }: { label: string; value: unknown; spillRef?: SpillRef | null }) {
  const hasValue = value != null && value !== '' &&
    !(typeof value === 'object' && !Array.isArray(value) && Object.keys(value as object).length === 0);
  if (!hasValue && !spillRef) return null;

  let display = '';
  if (hasValue) {
    if (typeof value === 'string') {
      display = value;
    } else {
      display = JSON.stringify(value, null, 2);
    }
  }

  return (
    <div className={css.jsonSectionWrap}>
      <Stack direction="horizontal" justify="between" align="center" className={css.jsonSectionHeader}>
        <Stack direction="horizontal" gap="sm" align="center">
          <strong className={css.jsonSectionLabel}>{label}</strong>
          {spillRef ? (
            <span title={`Backend: ${spillRef.backend}\nKey: ${spillRef.key}${spillRef.sha256 ? `\nsha256: ${spillRef.sha256}` : ''}`} className={css.mono}>
              [externally stored · {formatBytes(spillRef.size)} · {spillRef.backend}]
            </span>
          ) : null}
        </Stack>
        {display ? <CopyJsonButton json={display} /> : null}
      </Stack>
      {display ? <pre className={css.preBlockLarge}>{display}</pre> : (
        <pre className={css.preBlockLarge}>{spillRef ? '(spill body unresolved — SpillStore unreachable or not configured on Control Plane)' : ''}</pre>
      )}
    </div>
  );
}
