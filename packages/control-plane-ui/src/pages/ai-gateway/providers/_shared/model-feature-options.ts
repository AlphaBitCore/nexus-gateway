/**
 * Known model capability flags (aligned with gateway seed/catalog usage).
 * Additional values from the API are still shown when editing.
 */
export const MODEL_FEATURE_OPTIONS: { value: string; label: string }[] = [
  { value: 'vision', label: 'Vision' },
  { value: 'function_calling', label: 'Function calling' },
  { value: 'streaming', label: 'Streaming' },
  { value: 'json_mode', label: 'JSON mode' },
  { value: 'thinking', label: 'Thinking / extended reasoning' },
];

export function mergeModelFeatureOptions(selected: string[]): { value: string; label: string }[] {
  const known = new Set(MODEL_FEATURE_OPTIONS.map((o) => o.value));
  const extras = selected
    .filter((v) => !known.has(v))
    .map((v) => ({ value: v, label: v }));
  return [...MODEL_FEATURE_OPTIONS, ...extras];
}
