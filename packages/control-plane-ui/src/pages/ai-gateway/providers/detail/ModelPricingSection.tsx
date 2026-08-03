// Pricing-section presentation shared by the model create/edit drawer:
// the type-aware section header + hint, and the base-column labels.
// Extracted from ModelFormDrawer.tsx along the pricing seam.
import { useTranslation } from 'react-i18next';
import styles from './ModelFormDrawer.module.css';

// Image/audio/rerank models are not necessarily token-priced: the price unit
// is the model's own billable unit (tokens for usage-reporting models such as
// gpt-image-1 / Gemini image or Voyage rerank; images / characters / seconds
// for per-unit models such as dall-e-3 / tts-1 / whisper-1; search units for
// Cohere rerank). The header and hint keep the admin from configuring a
// per-unit rate against a token label. Realtime models are token-priced but
// bill six components at separate rates — the hint says the base columns
// carry the TEXT rates and the audio columns carry the audio rates.
export function PricingSectionHeader({ modelType }: { modelType: string }) {
  const { t } = useTranslation();
  if (modelType === 'realtime') {
    return (
      <>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionPricing', 'Pricing (per 1M tokens)')}</h3>
        <p className={styles.pricingHint}>{t('pages:providers.pricingRealtimeHint', 'Realtime models bill text and audio tokens at separate rates — all six fields are USD per 1M tokens. When "Cached Audio Read $/M" is not set, it falls back to the audio input rate.')}</p>
      </>
    );
  }
  const isUnitPriced =
    modelType === 'image' || modelType === 'audio' || modelType === 'tts' ||
    modelType === 'stt' || modelType === 'video' || modelType === 'rerank';
  if (!isUnitPriced) {
    return <h3 className={styles.sectionHeader}>{t('pages:providers.sectionPricing', 'Pricing (per 1M tokens)')}</h3>;
  }
  return (
    <>
      <h3 className={styles.sectionHeader}>{t('pages:providers.sectionPricingUnits', 'Pricing (per 1M billable units)')}</h3>
      <p className={styles.pricingHint}>{t('pages:providers.pricingUnitsHint', 'Models that report token usage are priced per 1M tokens; per-unit models are priced per 1M of their own unit — e.g. $0.04/image → 40000.')}</p>
    </>
  );
}

// Labels for the three base price columns. For realtime models the base
// columns carry the TEXT-token rates (audio rates live in their own
// columns), so the labels must say "Text …" — a generic "Input $/M" label
// invites pasting the far higher audio-in rate into the text column.
export function basePriceLabels(t: ReturnType<typeof useTranslation>['t'], isRealtime: boolean) {
  return {
    input: isRealtime ? t('pages:providers.textInputPricePerM', 'Text Input $/M') : t('pages:providers.inputPricePerM'),
    output: isRealtime ? t('pages:providers.textOutputPricePerM', 'Text Output $/M') : t('pages:providers.outputPricePerM'),
    cachedRead: isRealtime ? t('pages:providers.cachedTextReadPricePerM', 'Cached Text Read $/M') : t('pages:providers.cachedInputReadPricePerM'),
  };
}
