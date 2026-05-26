import type { ApiProviderTemplate } from './types';
import { FEATURED_PROVIDER_TEMPLATE_NAMES } from './types';

export function featuredTemplatesFirst(all: ApiProviderTemplate[]): ApiProviderTemplate[] {
  const byName = new Map(all.map((t) => [t.name, t]));
  const out: ApiProviderTemplate[] = [];
  for (const name of FEATURED_PROVIDER_TEMPLATE_NAMES) {
    const t = byName.get(name);
    if (t) out.push(t);
  }
  return out;
}

export function templateAccent(name: string): string {
  const map: Record<string, string> = {
    openai: '#10a37f',
    anthropic: '#d97757',
    'google-gemini': '#4285f4',
    'azure-openai': '#0078d4',
    deepseek: '#4d6bfe',
    minimax: '#6366f1',
    glm: '#0d9488',
    moonshot: '#f59e0b',
    xai: '#000000',
  };
  return map[name] ?? 'var(--color-primary)';
}

export function initials(displayName: string | undefined | null): string {
  if (!displayName) return 'AI';
  const parts = displayName.trim().split(/\s+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  const w = displayName.replace(/[^a-zA-Z0-9]/g, '');
  return w.slice(0, 2).toUpperCase() || 'AI';
}
