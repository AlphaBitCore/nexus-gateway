import { useTranslation } from 'react-i18next';
import { ModelModalitiesField } from './ModelModalitiesField';
import { ProviderModelCapabilitiesPanel } from './ProviderModelCapabilitiesPanel';
import type { ModelCapabilityJson } from '@/api/types';
import styles from './ModelFormDrawer.module.css';

/**
 * The Capabilities section of the model form, shared by create and edit.
 *
 * It renders for EVERY model type. It used to render only for chat and
 * embedding, and for chat it rendered a paragraph explaining that it had no
 * content — so an audio or image model, precisely the kind whose modalities
 * differ from the type default, had no section at all and nowhere to say what
 * it accepts. Modalities apply to all types; the numeric-limits panel keeps
 * its own narrower condition because only chat and embedding have limits it
 * knows how to render.
 *
 * Create and edit differ only in which form keys they write and whether the
 * admin may edit, so they share one component rather than two copies that
 * drift.
 */
export interface ModelCapabilitiesSectionProps {
  modelType: string;
  inputModalities?: string[];
  outputModalities?: string[];
  requiredModalities?: string[];
  onModalitiesChange: (next: {
    input: string[];
    output: string[];
    required: string[];
  }) => void;
  /** null is the loaded-but-unset case the edit form carries; the panel
      treats it the same as absent. */
  capability?: ModelCapabilityJson | null;
  onCapabilityChange: (next: ModelCapabilityJson) => void;
  editable: boolean;
}

export function ModelCapabilitiesSection({
  modelType,
  inputModalities,
  outputModalities,
  requiredModalities,
  onModalitiesChange,
  capability,
  onCapabilityChange,
  editable,
}: ModelCapabilitiesSectionProps) {
  const { t } = useTranslation();
  return (
    <section className={styles.section}>
      <h3 className={styles.sectionHeader}>
        {t('pages:providers.capabilities.sectionTitle', 'Capabilities')}
      </h3>
      <ModelModalitiesField
        input={inputModalities}
        output={outputModalities}
        required={requiredModalities}
        onChange={onModalitiesChange}
        disabled={!editable}
      />
      {(modelType === 'embedding' || modelType === 'chat') && (
        <ProviderModelCapabilitiesPanel
          modelType={modelType}
          value={capability}
          onChange={onCapabilityChange}
          editable={editable}
        />
      )}
    </section>
  );
}
