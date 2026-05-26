/**
 * Detail-renderer registry — maps a rule ID to the component that renders its
 * `alert.details` payload on AlertDetailDrawer.
 *
 * Adding a new renderer:
 *   1. Create `<Name>Renderer.tsx` implementing `DetailRendererProps`.
 *   2. Add an entry to `DETAIL_RENDERERS` keyed by the rule ID.
 *   3. Add i18n keys under `pages:alerts.detailRenderers.<name>.*` in all 6
 *      locale files.
 *
 * Rules not present in `DETAIL_RENDERERS` fall through to `GenericRenderer`,
 * which pretty-prints the raw JSON — the pre-registry behaviour.
 *
 * Mirrors `../ruleEditors/index.ts` in registry shape.
 */
import type { ComponentType } from 'react';
import type { DetailRendererProps } from './types';
import { QuotaThresholdRenderer } from './QuotaThresholdRenderer';
import { QuotaVkExpiringRenderer } from './QuotaVkExpiringRenderer';
import { ProxyHookFailureRateRenderer } from './ProxyHookFailureRateRenderer';
import { ProxyHighErrorRateRenderer } from './ProxyHighErrorRateRenderer';
import { GenericRenderer } from './GenericRenderer';

export type { DetailRendererProps } from './types';
export { GenericRenderer } from './GenericRenderer';

/** Registry: exact rule ID → component. Fallback: GenericRenderer. */
const DETAIL_RENDERERS: Record<string, ComponentType<DetailRendererProps>> = {
  'quota.threshold': QuotaThresholdRenderer,
  'quota.vk_expiring': QuotaVkExpiringRenderer,
  'proxy.hook_failure_rate': ProxyHookFailureRateRenderer,
  // proxy.hook_timeout_rate ships the same details shape — reuse the same
  // renderer (see ProxyHookFailureRateRenderer for the payload it accepts).
  'proxy.hook_timeout_rate': ProxyHookFailureRateRenderer,
  'proxy.high_error_rate': ProxyHighErrorRateRenderer,
};

/**
 * Return the renderer component bound to `ruleId`, or `GenericRenderer` as a
 * safe fallback. Never returns null — callers can render unconditionally.
 */
export function getDetailRenderer(
  ruleId: string,
): ComponentType<DetailRendererProps> {
  return DETAIL_RENDERERS[ruleId] ?? GenericRenderer;
}
