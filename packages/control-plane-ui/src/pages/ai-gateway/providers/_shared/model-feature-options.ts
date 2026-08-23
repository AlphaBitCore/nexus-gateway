/**
 * Known model capability flags (aligned with gateway seed/catalog usage).
 * Additional values from the API are still shown when editing.
 *
 * `reasoning` is the one name for the capability vendors call reasoning,
 * extended thinking, or thinking budgets. The catalogue carried both
 * `reasoning` and `thinking` on disjoint provider sets — no row had both — so
 * a rule or a picker keyed on either saw a partial answer and nothing said so.
 * The canonical layer had already settled on `reasoning` for the response
 * content type and the token count; the catalogue simply never followed.
 *
 * `prompt_caching` and `search` were offered by no picker while dozens of rows
 * carried them, so an admin editing such a row could not see what it declared.
 *
 * `structured_outputs` is a separate answer from `json_mode`, not a stronger
 * wording of it: `json_mode` is `response_format: {type: json_object}`, which
 * every target honours or is instructed into, while this one is a caller-
 * supplied JSON Schema that a model either holds its answer to or does not.
 * Probed per model 2026-08-19 and the two disagree on exactly the rows that
 * matter — gpt-4-turbo carries json_mode and answers 400 to a schema. It is an
 * eligibility tag: an untagged row is EXCLUDED from `auto` for schema requests,
 * so leaving it off this picker meant an admin could not describe a model the
 * router would then refuse to use.
 *
 * `vision` is deliberately absent. It said "accepts images", which is what the
 * Accepts row of the modalities field says — and offering both let an admin
 * tick one without the other, which is how 34 production rows came to
 * advertise vision beside inputModalities ["text"]. The gateway still derives
 * the string onto GET /v1/models for SDK callers; it is no longer something
 * anyone states. mergeModelFeatureOptions keeps rendering it for a row that
 * still carries it, so an un-migrated row stays editable rather than silently
 * losing the value on the next save.
 */
export const MODEL_FEATURE_OPTIONS: { value: string; label: string }[] = [
  { value: 'function_calling', label: 'Function calling' },
  { value: 'streaming', label: 'Streaming' },
  { value: 'json_mode', label: 'JSON mode' },
  { value: 'structured_outputs', label: 'Structured outputs (JSON Schema)' },
  { value: 'reasoning', label: 'Reasoning / extended thinking' },
  { value: 'prompt_caching', label: 'Prompt caching' },
  { value: 'search', label: 'Built-in web search' },
];

export function mergeModelFeatureOptions(selected: string[]): { value: string; label: string }[] {
  const known = new Set(MODEL_FEATURE_OPTIONS.map((o) => o.value));
  const extras = selected
    .filter((v) => !known.has(v))
    .map((v) => ({ value: v, label: v }));
  return [...MODEL_FEATURE_OPTIONS, ...extras];
}
