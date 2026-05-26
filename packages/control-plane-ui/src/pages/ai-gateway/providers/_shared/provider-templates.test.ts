/**
 * Static provider-template JSON under `public/provider-templates/` must match
 * the Control Plane's CreateProvider contract: every template carries a
 * canonical `adapterType` and every `index.json` entry agrees with its detail
 * file. The wizard submits `adapterType` verbatim and the API rejects
 * anything outside `PROVIDER_ADAPTER_TYPES`, so drift here means
 * guaranteed-to-fail create attempts against a seeded backend.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

import { PROVIDER_ADAPTER_TYPES } from './adapterTypes';

const TEMPLATES_DIR = resolve(__dirname, '../../../../../public/provider-templates');

function readJson<T>(file: string): T {
  return JSON.parse(readFileSync(join(TEMPLATES_DIR, file), 'utf8')) as T;
}

interface IndexEntry {
  name: string;
  displayName: string;
  description: string;
  baseUrl: string;
  adapterType: string;
  modelCount?: number;
}

interface TemplateDetail {
  name: string;
  displayName: string;
  description: string;
  baseUrl: string;
  adapterType: string;
  models: unknown[];
}

describe('provider-templates (public/provider-templates)', () => {
  const indexEntries = readJson<{ templates: IndexEntry[] }>('index.json').templates;

  it('index.json lists at least one template', () => {
    expect(indexEntries.length).toBeGreaterThan(0);
  });

  it('every index.json entry has a canonical adapterType and no legacy "type" field', () => {
    for (const entry of indexEntries) {
      expect((PROVIDER_ADAPTER_TYPES as readonly string[]).includes(entry.adapterType)).toBe(true);
      expect(entry).not.toHaveProperty('type');
    }
  });

  it('every detail file agrees with its index entry and carries a canonical adapterType', () => {
    for (const entry of indexEntries) {
      const detail = readJson<TemplateDetail>(`${entry.name}.json`);
      expect(detail.name).toBe(entry.name);
      expect(detail.adapterType).toBe(entry.adapterType);
      expect((PROVIDER_ADAPTER_TYPES as readonly string[]).includes(detail.adapterType)).toBe(true);
      expect(detail).not.toHaveProperty('type');
    }
  });

  it('no stray template JSON under public/ is missing from index.json (except index itself)', () => {
    const files = readdirSync(TEMPLATES_DIR)
      .filter((f) => f.endsWith('.json') && f !== 'index.json');
    const names = new Set(indexEntries.map((e) => `${e.name}.json`));
    for (const f of files) {
      expect(names.has(f)).toBe(true);
    }
  });
});
