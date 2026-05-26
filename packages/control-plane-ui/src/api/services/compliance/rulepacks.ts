/**
 * Rule Pack admin API service — bound to `/api/admin/rule-packs/*`,
 * `/api/admin/hooks/:hookId/rule-packs`, and
 * `/api/admin/rule-pack-installs/:installId/*`.
 *
 * The shared `api` client only supports JSON bodies, so the YAML-backed
 * preview/import endpoints use a local `fetch` helper with Bearer auth.
 */

import { getAccessToken } from '@/auth/tokens/tokenStore';
import { ApiError, api } from '../../client';

export interface RulePackMeta {
  id: string;
  name: string;
  version: string;
  maintainer: string;
  description?: string;
  signature?: string;
  createdAt: string;
}

export interface RulePackRule {
  id?: string;
  packId?: string;
  ruleId: string;
  category: string;
  severity: string;
  pattern: string;
  flags?: string;
  description?: string;
  labels?: string[];
}

export interface RulePack extends RulePackMeta {
  rules: RulePackRule[];
}

export interface RulePackMatch {
  pack: string;
  packVersion: string;
  ruleId: string;
  category: string;
  severity: string;
  labels: string[];
  matchedText?: string;
}

export interface RulePackPreviewResult {
  pack?: RulePack | null;
  warnings: string[];
  errors: string[];
}

export interface RulePackImportResult {
  packId: string;
  ruleCount: number;
  warnings: string[];
}

export interface RulePackInstall {
  id: string;
  packId: string;
  packName: string;
  pinVersion: string;
  boundHookId: string;
  enabled: boolean;
  installedAt: string;
}

export interface RulePackOverride {
  id?: string;
  installId?: string;
  ruleLocalId: string;
  disabled: boolean;
  severityOverride?: string;
}

export interface EffectiveRuleSet {
  install: RulePackInstall;
  pack: RulePack;
}

function errorMessage(errorBody: unknown, fallback: string): string {
  if (errorBody && typeof errorBody === 'object') {
    const asRecord = errorBody as Record<string, unknown>;
    if (typeof asRecord.error === 'string') return asRecord.error;
    if (asRecord.error && typeof asRecord.error === 'object') {
      const nested = asRecord.error as Record<string, unknown>;
      if (typeof nested.message === 'string') return nested.message;
    }
    if (typeof asRecord.detail === 'string') return asRecord.detail;
  }
  return fallback;
}

async function postYaml<T>(path: string, yaml: string): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'text/x-yaml' };
  const token = getAccessToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  const res = await fetch(new URL(path, window.location.origin).toString(), {
    method: 'POST',
    headers,
    body: yaml,
  });
  if (!res.ok) {
    const errorBody = await res.json().catch(() => null);
    throw new ApiError(res.status, 'UNKNOWN', errorMessage(errorBody, res.statusText));
  }
  return res.json() as Promise<T>;
}

export interface RulePackCreateInput {
  name: string;
  version: string;
  maintainer: string;
  description?: string;
  rules: RulePackRule[];
}

export interface RulePackUpdateInput {
  maintainer?: string;
  description?: string;
  signature?: string;
  rules?: RulePackRule[];
}

export const rulePacksApi = {
  list(): Promise<RulePackMeta[]> {
    return api.get('/api/admin/rule-packs');
  },

  get(id: string): Promise<RulePack> {
    return api.get(`/api/admin/rule-packs/${id}`);
  },

  create(body: RulePackCreateInput): Promise<RulePack> {
    return api.post('/api/admin/rule-packs', body);
  },

  update(id: string, body: RulePackUpdateInput): Promise<RulePack> {
    return api.patch(`/api/admin/rule-packs/${id}`, body);
  },

  delete(id: string): Promise<void> {
    return api.delete(`/api/admin/rule-packs/${id}`);
  },

  preview(yaml: string): Promise<RulePackPreviewResult> {
    return postYaml('/api/admin/rule-packs/preview', yaml);
  },

  import(yaml: string): Promise<RulePackImportResult> {
    return postYaml('/api/admin/rule-packs/import', yaml);
  },

  dryRun(id: string, content: string): Promise<{ matches: RulePackMatch[] }> {
    return api.post(`/api/admin/rule-packs/${id}/dry-run`, { content });
  },

  install(hookId: string, body: { packId: string; pinVersion: string; enabled?: boolean }): Promise<RulePackInstall> {
    return api.post(`/api/admin/hooks/${hookId}/rule-packs`, body);
  },

  listInstallsForHook(hookId: string): Promise<RulePackInstall[]> {
    return api.get(`/api/admin/hooks/${hookId}/rule-packs`);
  },

  patchInstall(installId: string, enabled: boolean): Promise<{ installId: string; enabled: boolean }> {
    return api.patch(`/api/admin/rule-pack-installs/${installId}`, { enabled });
  },

  uninstall(installId: string): Promise<void> {
    return api.delete(`/api/admin/rule-pack-installs/${installId}`);
  },

  upsertOverrides(installId: string, overrides: RulePackOverride[]): Promise<{ installId: string; overridesSaved: number }> {
    return api.patch(`/api/admin/rule-pack-installs/${installId}/overrides`, { overrides });
  },

  effectiveRules(installId: string): Promise<EffectiveRuleSet> {
    return api.get(`/api/admin/rule-pack-installs/${installId}/effective-rules`);
  },
};
