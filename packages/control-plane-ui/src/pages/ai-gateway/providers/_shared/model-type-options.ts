import type { useTranslation } from 'react-i18next';

/**
 * The model-type vocabulary the gateway routing engine and catalog share
 * (Go: validModelTypes / typology.EndpointKindAcceptsModelType). Audio is
 * carried both as the coarse `audio` and the precise sub-types `tts`/`stt`/
 * `realtime`; the catalog types its speech models precisely. Kept in one place
 * so the create wizard and the edit drawer never drift out of sync.
 */
export function modelTypeOptions(
  t: ReturnType<typeof useTranslation>['t'],
): { value: string; label: string }[] {
  return [
    { value: 'chat', label: t('pages:providers.modelTypeChat') },
    { value: 'embedding', label: t('pages:providers.modelTypeEmbedding') },
    { value: 'image', label: t('pages:providers.modelTypeImage') },
    { value: 'audio', label: t('pages:providers.modelTypeAudio', 'Audio') },
    { value: 'tts', label: t('pages:providers.modelTypeTts', 'TTS (Text-to-Speech)') },
    { value: 'stt', label: t('pages:providers.modelTypeStt', 'STT (Speech-to-Text)') },
    { value: 'rerank', label: t('pages:providers.modelTypeRerank') },
    { value: 'video', label: t('pages:providers.modelTypeVideo') },
    { value: 'realtime', label: t('pages:providers.modelTypeRealtime') },
  ];
}
