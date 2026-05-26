/**
 * Shared types for the alert detail-renderer registry.
 *
 * A detail renderer is a small React component that presents the rule-specific
 * `alert.details` payload in a human-readable form within
 * `AlertDetailDrawer`. Each renderer is bound to a rule ID by the registry in
 * `./index.ts`. Rules without a bespoke renderer fall through to
 * `GenericRenderer`, which pretty-prints the raw JSON — the same shape the
 * drawer used to render before the registry landed.
 *
 * Renderers pull fields out of `alert.details` (typed as
 * `Record<string, unknown>` on the wire) with runtime narrowing
 * (`typeof x === 'number'` / `'string'`) and gracefully skip or render an em
 * dash for fields that are missing or of the wrong type. No `any` leakage.
 */
import type { Alert } from '@/api/services';

export interface DetailRendererProps {
  /** The alert being rendered. `alert.details` is the rule-specific blob. */
  alert: Alert;
}
