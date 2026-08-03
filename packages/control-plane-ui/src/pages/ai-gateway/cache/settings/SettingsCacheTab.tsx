/**
 * Provider prompt-cache configuration, rendered inside the /ai-gateway/cache page.
 *
 * Page layout:
 *   1. Adapter Defaults panel       — Tier 2 knobs for the 4 adapter families that have
 *                                     family-specific cache configuration (anthropic, bedrock,
 *                                     gemini, vertex). OpenAI-compat adapters have no
 *                                     adapter-level knobs, so they are intentionally absent
 *                                     from this panel's tab strip.
 *   2. Normalisation rules panel    — flat table of every bundled rule across every adapter
 *                                     family, grouped by adapter, with toggles. Source of truth
 *                                     for `BUNDLED_RULES` is `packages/shared/transport/wirerewrite/bundled.go`.
 *   3. Active Overrides panel       — Tier-3 listing.
 *
 * There is no Tier-1 "Global Defaults" panel: the cache master kill switch is retired
 * (emergency cache-off is the fleet disable-all on this page, or Emergency Passthrough's
 * time-boxed bypassCache), and the global normaliser gate is retired (the upstream rewrite
 * runs on demand — enabling a rule below, or a provider's marker injection, IS the demand).
 */
import { Stack } from '@/components/ui';
import { AdapterPanel } from './AdapterPanel';
import { NormalisationRulesPanel } from './NormalisationRulesPanel';
import { OverridesPanel } from './OverridesPanel';

export function SettingsCacheTab() {
  return (
    <Stack gap="lg">
      <AdapterPanel />
      <NormalisationRulesPanel />
      <OverridesPanel />
    </Stack>
  );
}
