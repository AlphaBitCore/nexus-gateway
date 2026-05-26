/**
 * Rule-editor registry — maps a rule ID to the component that renders its
 * `params` form on AlertRuleEditPage.
 *
 * Adding a new editor:
 *   1. Create `<Name>Editor.tsx` implementing `RuleEditorProps`.
 *   2. Add an entry to `RULE_EDITORS` keyed by the rule ID.
 *   3. Add i18n keys under `pages:alerts.ruleEditors.<name>.*` in all 6
 *      locale files.
 *
 * Rules not present in `RULE_EDITORS` fall through to `GenericJsonEditor`,
 * which renders a schema-driven form (integer/number/string/array/boolean)
 * and falls back to a raw-JSON textarea when the schema is unusable.
 */
import type { ComponentType } from 'react';
import type { RuleEditorProps } from './types';
import { QuotaThresholdEditor } from './QuotaThresholdEditor';
import { QuotaVkExpiringEditor } from './QuotaVkExpiringEditor';
import { ProxyHookFailureRateEditor } from './ProxyHookFailureRateEditor';
import { ProxyHookTimeoutRateEditor } from './ProxyHookTimeoutRateEditor';
import { GenericJsonEditor } from './GenericJsonEditor';

export type { RuleEditorProps } from './types';
export { GenericJsonEditor } from './GenericJsonEditor';

/** Registry: exact rule ID → component. Fallback: GenericJsonEditor. */
const RULE_EDITORS: Record<string, ComponentType<RuleEditorProps>> = {
  'quota.threshold': QuotaThresholdEditor,
  'quota.vk_expiring': QuotaVkExpiringEditor,
  'proxy.hook_failure_rate': ProxyHookFailureRateEditor,
  'proxy.hook_timeout_rate': ProxyHookTimeoutRateEditor,
};

/**
 * Return the editor component bound to `ruleId`, or `GenericJsonEditor` as
 * a schema-driven fallback for unknown rules. Never returns null — callers
 * can render the result unconditionally.
 */
export function getRuleEditor(ruleId: string): ComponentType<RuleEditorProps> {
  return RULE_EDITORS[ruleId] ?? GenericJsonEditor;
}
