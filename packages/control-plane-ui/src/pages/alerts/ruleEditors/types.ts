/**
 * Shared types for the rule-editor registry.
 *
 * A rule editor is a small React component that renders an edit form for the
 * `params` JSON blob of a specific alert rule. Each editor is bound to a rule
 * ID (or a fallback category) by the registry in `./index.ts`.
 *
 * The editor receives the current `value` (AlertRule.params) plus the JSON
 * Schema that describes it (`schema` = AlertRule.paramsSchema), and calls
 * `onChange` with a new `params` object whenever the user edits a field. Hub
 * validates the submitted body against `paramsSchema` server-side, so the
 * editor's job is to steer the user toward a valid shape — not to block save.
 *
 * `onValidate` is optional: editors may report whether their current local
 * state parses into a valid payload so the enclosing page can gate the Save
 * button. Editors that always emit a well-formed object can skip this.
 */
export interface RuleEditorProps {
  /** Current `params` value for the rule being edited. */
  value: Record<string, unknown>;
  /** JSON Schema that describes the expected `params` shape. */
  schema: Record<string, unknown>;
  /** Fired when the user produces a new params object. */
  onChange: (next: Record<string, unknown>) => void;
  /** Optional: let the editor report validity back to the host. */
  onValidate?: (ok: boolean) => void;
}
