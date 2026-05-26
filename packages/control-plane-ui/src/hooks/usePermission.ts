/**
 * Frontend permission hook — maps UI permission keys to IAM action strings
 * and checks against the permissions array populated by AuthContext from
 * GET /api/admin/me/permissions. This mirrors the backend iamMW() evaluation
 * exactly — the same policy engine, the same action names.
 */

import { useAuth } from '../auth/context/AuthContext';

/**
 * Maps UI-facing permission keys to canonical IAM action strings.
 *
 * The canonical action format is `admin:<resource>.<verb>` (kebab-case
 * lowercase), derived from `packages/shared/iam.Catalog` on the backend.
 * Every entry must resolve to an action produced by `iam.AllActions()`.
 * The regex shape is enforced by usePermission.coverage.test.ts.
 *
 * Exported for the regression test in usePermission.coverage.test.ts, which
 * grep-scans src/ for usePermission('...') call sites and asserts each key is
 * present here. Do not import this map from production code — call
 * `usePermission(key)` instead so the auth state is respected.
 */
export const ACTION_MAP: Record<string, string> = {
  'provider:create': 'admin:provider.create',
  'provider:update': 'admin:provider.update',
  'provider:delete': 'admin:provider.delete',
  'model:create': 'admin:model.create',
  'model:update': 'admin:model.update',
  'model:delete': 'admin:model.delete',
  'policy:create': 'admin:iam-policy.create',
  'policy:update': 'admin:iam-policy.update',
  'policy:delete': 'admin:iam-policy.delete',
  'hook:create': 'admin:hook.create',
  'hook:update': 'admin:hook.update',
  'hook:delete': 'admin:hook.delete',
  'virtual-key:create': 'admin:virtual-key.create',
  'virtual-key:update': 'admin:virtual-key.update',
  'virtual-key:delete': 'admin:virtual-key.delete',
  'routing-rule:create': 'admin:routing-rule.create',
  'routing-rule:update': 'admin:routing-rule.update',
  'routing-rule:delete': 'admin:routing-rule.delete',
  'routing-rule:simulate': 'admin:routing-rule.simulate',
  'quota:create': 'admin:quota-override.create',
  'quota:update': 'admin:quota-override.update',
  'quota:delete': 'admin:quota-override.delete',
  'quota:reset': 'admin:quota-override.update',
  'quotaPolicy:create': 'admin:quota-policy.create',
  'quotaPolicy:update': 'admin:quota-policy.update',
  'quotaPolicy:delete': 'admin:quota-policy.delete',
  'virtualKey:approve': 'admin:virtual-key.approve',
  'credential:create': 'admin:credential.create',
  'credential:update': 'admin:credential.update',
  'credential:delete': 'admin:credential.delete',
  'cache:flush': 'admin:settings.update',
  'cache:invalidate': 'admin:settings.update',
  'api-key:create': 'admin:api-key.create',
  'api-key:delete': 'admin:api-key.delete',
  'config:export': 'admin:settings.read',
  'config:import': 'admin:settings.update',
  'config:rollback': 'admin:settings.update',
  'devices:read': 'admin:agent-device.read',
  'devices:create': 'admin:agent-device.create',
  'devices:delete': 'admin:agent-device.delete',
  'agent-devices:read': 'admin:agent-device.read',
  'agent-devices:update': 'admin:agent-device.update',
  'agent-devices:delete': 'admin:agent-device.delete',
  'agent-devices:force-resync': 'admin:agent-device.force-resync',
  'agent-devices:rotate': 'admin:agent-device.rotate',
  'diagnostic-mode:read': 'admin:diagnostic-mode.read',
  'diagnostic-mode:update': 'admin:diagnostic-mode.update',
  'device-groups:read': 'admin:device-group.read',
  'device-groups:create': 'admin:device-group.create',
  'device-groups:update': 'admin:device-group.update',
  'device-groups:delete': 'admin:device-group.delete',
  'settings:read': 'admin:settings.read',
  'settings:update': 'admin:settings.update',
  'health:reset': 'admin:settings.write',
  'audit:export': 'admin:audit-log.export',
  'iam:read': 'admin:iam-policy.read',
  'iam:write': 'admin:iam-policy.create',
  'iam:update': 'admin:iam-policy.update',
  'iam:delete': 'admin:iam-policy.delete',
  'user:read': 'admin:user.read',
  'user:write': 'admin:user.create',
  'user:create': 'admin:user.create',
  'user:update': 'admin:user.update',
  'user:delete': 'admin:user.delete',
  'project:read': 'admin:project.read',
  'project:create': 'admin:project.create',
  'project:update': 'admin:project.update',
  'project:delete': 'admin:project.delete',
  'organization:read': 'admin:organization.read',
  'organization:create': 'admin:organization.create',
  'organization:update': 'admin:organization.update',
  'organization:delete': 'admin:organization.delete',
  'kill-switch:toggle': 'admin:kill-switch.toggle',
  'node:override': 'admin:node.write-override',
  'node:resync': 'admin:node.force-resync',
  'ai-guard:read': 'admin:ai-guard-config.read',
  'ai-guard:write': 'admin:ai-guard-config.update',
  'semantic-cache:read': 'admin:semantic-cache.read',
  'semantic-cache:update': 'admin:semantic-cache.update',
  'prompt-cache:read': 'admin:prompt-cache.read',
  'prompt-cache:update': 'admin:prompt-cache.update',
  'extract-cache:read': 'admin:extract-cache.read',
  'extract-cache:update': 'admin:extract-cache.update',
  'observability:read': 'admin:observability.read',
  'observability:write': 'admin:observability.write',
  'observability-dlq:read': 'admin:observability-dlq.read',
  'observability-dlq:manage': 'admin:observability-dlq.manage',
  'passthrough:read': 'admin:passthrough.read',
  'passthrough:write': 'admin:passthrough.write',
  'passthrough:emergencyEnable': 'admin:passthrough.emergency-enable',
};

const warnedUnmappedKeys = new Set<string>();

/**
 * Returns true when the authenticated principal is allowed to perform the
 * action mapped to `permission`. Falls back to false when the key is unknown
 * or permissions have not been loaded yet.
 *
 * In dev builds, logs a console warning the first time an unmapped key is
 * seen — this surfaces invisible-button bugs (missing ACTION_MAP entry hides
 * the affordance from every user including super-admin) at runtime rather
 * than letting them ship silently.
 */
export function usePermission(permission: string): boolean {
  const { permissions } = useAuth();
  if (!permissions.length) return false;
  const action = ACTION_MAP[permission];
  if (!action) {
    if (import.meta.env.DEV && !warnedUnmappedKeys.has(permission)) {
      warnedUnmappedKeys.add(permission);
      // eslint-disable-next-line no-console
      console.warn(
        `[usePermission] unmapped key "${permission}" — add it to ACTION_MAP ` +
        `in src/hooks/usePermission.ts. Returning false; the corresponding ` +
        `UI affordance will be hidden from every user.`,
      );
    }
    return false;
  }
  return permissions.includes(action);
}
