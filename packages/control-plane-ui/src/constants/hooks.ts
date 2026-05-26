/**
 * Hook-related UI and API string values. Keep literals here instead of scattering across components.
 */

/** Em dash for empty / N/A table cells (Unicode U+2014). */
export const HOOK_UI_EMPTY = '\u2014';

/** Hooks list: resolved category (registry or DB override). */
export const HOOK_UI_TABLE_EFFECTIVE_CATEGORY = 'Effective category';

/** Hooks list: persisted `HookConfig.category` (null/empty → em dash; use effective column for UX). */
export const HOOK_UI_TABLE_RAW_CATEGORY_DB = 'Raw category (DB)';

export const HOOK_PAGE_SUBTITLE =
  'Built-in logic lives in gateway source; which hooks run, their order (priority), enablement, and labels are configured in the database and apply immediately after save (hook cache invalidated).';

export const HOOK_STAGE = {
  REQUEST: 'request',
  RESPONSE: 'response',
} as const;

export type HookStage = (typeof HOOK_STAGE)[keyof typeof HOOK_STAGE];

export const HOOK_ROW_TYPE = {
  BUILTIN: 'builtin',
  WEBHOOK: 'webhook',
  SCRIPT: 'script',
} as const;

export type HookRowType = (typeof HOOK_ROW_TYPE)[keyof typeof HOOK_ROW_TYPE];

export const HOOK_FAIL_BEHAVIOR = {
  FAIL_OPEN: 'fail-open',
  FAIL_CLOSED: 'fail-closed',
} as const;

export type HookFailBehavior = (typeof HOOK_FAIL_BEHAVIOR)[keyof typeof HOOK_FAIL_BEHAVIOR];

export const HOOK_CATEGORY = {
  COMPLIANCE: 'compliance',
  TRAFFIC_CONTROL: 'traffic_control',
  QUALITY: 'quality',
  OBSERVABILITY: 'observability',
  CUSTOM: 'custom',
} as const;

export type HookCategory = (typeof HOOK_CATEGORY)[keyof typeof HOOK_CATEGORY];

/** Select value for “use registry default” (persisted as DB null). */
export const HOOK_CATEGORY_AUTO_VALUE = '' as const;

export const HOOK_CATEGORY_LABEL: Record<HookCategory, string> = {
  [HOOK_CATEGORY.COMPLIANCE]: 'Compliance & content safety',
  [HOOK_CATEGORY.TRAFFIC_CONTROL]: 'Traffic & limits',
  [HOOK_CATEGORY.QUALITY]: 'Quality & signals',
  [HOOK_CATEGORY.OBSERVABILITY]: 'Observability',
  [HOOK_CATEGORY.CUSTOM]: 'Custom / other',
};

export const HOOK_CLASSIFICATION_SOURCE = {
  DATABASE: 'database',
  REGISTRY: 'registry',
} as const;

export type HookClassificationSource =
  (typeof HOOK_CLASSIFICATION_SOURCE)[keyof typeof HOOK_CLASSIFICATION_SOURCE];

export const HOOK_EXECUTION_FLOW_KIND = {
  MILESTONE: 'milestone',
  HOOK_SEGMENT: 'hook_segment',
} as const;

export type HookExecutionFlowKind =
  (typeof HOOK_EXECUTION_FLOW_KIND)[keyof typeof HOOK_EXECUTION_FLOW_KIND];

export const HOOK_FORM_DEFAULTS = {
  PRIORITY: 0,
  TIMEOUT_MS: 5000,
} as const;

/** Short summary when a hook row is not dual-phase capable. */
export const HOOK_UI_PHASE_SCOPE_SINGLE = 'single';

/** Short summary when the same implementation can run on both stages (separate rows). */
export const HOOK_UI_PHASE_SCOPE_DUAL = `${HOOK_STAGE.REQUEST} + ${HOOK_STAGE.RESPONSE}`;

/** Reorder controls (execution chain panel). */
export const HOOK_UI_REORDER_UP = 'Up';
export const HOOK_UI_REORDER_DOWN = 'Down';

export const HOOK_TOAST_REORDER_SUCCESS = 'Hook execution order updated';

export const HOOK_PIPELINE_REORDER_HINT =
  'Use Up/Down to change priority for this phase; the gateway reloads hook order on save (cache invalidated).';

export const HOOK_SELECT_TYPE_OPTIONS: { value: HookRowType; label: string }[] = [
  { value: HOOK_ROW_TYPE.BUILTIN, label: 'Built-in' },
  { value: HOOK_ROW_TYPE.WEBHOOK, label: 'Webhook' },
  { value: HOOK_ROW_TYPE.SCRIPT, label: 'Script' },
];

export const HOOK_SELECT_STAGE_OPTIONS: { value: HookStage; label: string }[] = [
  { value: HOOK_STAGE.REQUEST, label: 'Request' },
  { value: HOOK_STAGE.RESPONSE, label: 'Response' },
];

export const HOOK_SELECT_FAIL_OPTIONS: { value: HookFailBehavior; label: string }[] = [
  { value: HOOK_FAIL_BEHAVIOR.FAIL_OPEN, label: 'Fail Open' },
  { value: HOOK_FAIL_BEHAVIOR.FAIL_CLOSED, label: 'Fail Closed' },
];

const HOOK_CATEGORY_SELECT_ORDER: readonly HookCategory[] = [
  HOOK_CATEGORY.COMPLIANCE,
  HOOK_CATEGORY.TRAFFIC_CONTROL,
  HOOK_CATEGORY.QUALITY,
  HOOK_CATEGORY.OBSERVABILITY,
  HOOK_CATEGORY.CUSTOM,
];

export const HOOK_SELECT_CATEGORY_OPTIONS: { value: string; label: string }[] = [
  { value: HOOK_CATEGORY_AUTO_VALUE, label: 'Auto (built-in registry default)' },
  ...HOOK_CATEGORY_SELECT_ORDER.map((c) => ({ value: c, label: HOOK_CATEGORY_LABEL[c] })),
];

/**
 * Ingress codes understood by packages/shared/policy/pipeline/policy.go#matchesIngress.
 * The backend treats an empty `applicableIngress` as "match all" and the admin
 * API rejects an explicit empty array, so the form omits the field entirely
 * when no box is checked (server default = `["ALL"]`).
 */
export const HOOK_APPLICABLE_INGRESS = {
  ALL: 'ALL',
  AI_GATEWAY: 'AI_GATEWAY',
  COMPLIANCE_PROXY: 'COMPLIANCE_PROXY',
  AGENT: 'AGENT',
} as const;

export type HookApplicableIngress =
  (typeof HOOK_APPLICABLE_INGRESS)[keyof typeof HOOK_APPLICABLE_INGRESS];

export const HOOK_APPLICABLE_INGRESS_ORDER: readonly HookApplicableIngress[] = [
  HOOK_APPLICABLE_INGRESS.ALL,
  HOOK_APPLICABLE_INGRESS.AI_GATEWAY,
  HOOK_APPLICABLE_INGRESS.COMPLIANCE_PROXY,
  HOOK_APPLICABLE_INGRESS.AGENT,
];
