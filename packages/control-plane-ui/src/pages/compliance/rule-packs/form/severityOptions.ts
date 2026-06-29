/**
 * Canonical rule severities accepted by the rule-pack engine.
 *
 * Severity is a classification signal only — `hard`/`soft` denote criticality
 * tiers and `warn` is advisory. The enforced action (block / redact / flag) is
 * decided by the bound hook's onMatch Action policy, never by severity alone.
 */
export const SEVERITY_OPTIONS = [
  {
    value: 'hard',
    labelKey: 'pages:hooks.rulePacks.severityHard',
    fallback: 'hard — critical',
  },
  {
    value: 'soft',
    labelKey: 'pages:hooks.rulePacks.severitySoft',
    fallback: 'soft — elevated',
  },
  {
    value: 'warn',
    labelKey: 'pages:hooks.rulePacks.severityWarn',
    fallback: 'warn — advisory',
  },
] as const;
