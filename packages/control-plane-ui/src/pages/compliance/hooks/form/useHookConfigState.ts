import { useState, useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import type { HookConfig, HookImplementationSummary } from '@/api/types';
import { buildDefaultsFromSchema } from '@/components/config/JsonSchemaHookConfigForm';
import { validateDataAgainstJsonSchema } from '@/lib/validate-json-schema';
import { asConfigRecord, implementationsForRow } from './hookFormModel';

interface UseHookConfigStateArgs {
  hook?: HookConfig;
  implementations: HookImplementationSummary[];
  type: string;
  stage: string;
  addToast: (message: string, type?: 'success' | 'error' | 'warning' | 'info') => void;
}

/**
 * Owns the implementation-selection + config-editor state for the hook form:
 * which implementation is selected, the structured config object, the manual
 * JSON fallback, and the toggle between them. Keeps the auto-select effect and
 * the config-payload builder/validator co-located with that state.
 */
export function useHookConfigState({
  hook,
  implementations,
  type,
  stage,
  addToast,
}: UseHookConfigStateArgs) {
  const { t } = useTranslation();
  const existingCfg = asConfigRecord(hook?.config);

  const [selectedImplementationId, setSelectedImplementationId] = useState(hook?.implementationId ?? '');
  const [configObject, setConfigObject] = useState<Record<string, unknown>>(() => ({ ...existingCfg }));
  const [manualConfigJson, setManualConfigJson] = useState(() =>
    hook ? JSON.stringify(existingCfg, null, 2) : '{}',
  );
  const [useManualConfigEditor, setUseManualConfigEditor] = useState(false);

  const filteredImplementations = useMemo(
    () => implementationsForRow(implementations, type, stage),
    [implementations, type, stage],
  );

  const selectedMeta = useMemo(
    () => filteredImplementations.find((i) => i.implementationId === selectedImplementationId),
    [filteredImplementations, selectedImplementationId],
  );

  useEffect(() => {
    if (!filteredImplementations.length) return;
    const ids = new Set(filteredImplementations.map((i) => i.implementationId));
    if (selectedImplementationId && ids.has(selectedImplementationId)) return;

    const first = filteredImplementations[0];
    setSelectedImplementationId(first.implementationId);
    const sch = first.configSchema as Record<string, unknown> | undefined;
    const sameRow = hook?.implementationId === first.implementationId;
    if (sch) {
      setUseManualConfigEditor(false);
      setConfigObject(
        sameRow ? { ...buildDefaultsFromSchema(sch), ...asConfigRecord(hook?.config) } : { ...buildDefaultsFromSchema(sch) },
      );
    } else {
      setUseManualConfigEditor(true);
      setManualConfigJson(sameRow ? JSON.stringify(asConfigRecord(hook?.config), null, 2) : '{}');
    }
  }, [filteredImplementations, selectedImplementationId, hook]);

  const handleImplementationChange = (id: string) => {
    setSelectedImplementationId(id);
    const impl = implementations.find((i) => i.implementationId === id);
    const sch = impl?.configSchema as Record<string, unknown> | undefined;
    if (sch) {
      setConfigObject({ ...buildDefaultsFromSchema(sch) });
      setUseManualConfigEditor(false);
    } else {
      setManualConfigJson('{}');
      setUseManualConfigEditor(true);
    }
  };

  const buildConfigPayload = (): Record<string, unknown> | null => {
    const schema = selectedMeta?.configSchema as Record<string, unknown> | undefined;
    if (schema && !useManualConfigEditor) {
      const err = validateDataAgainstJsonSchema(schema, configObject);
      if (err) {
        addToast(t('common:validation.configSchemaMismatch', { error: err }), 'error');
        return null;
      }
      return { ...configObject };
    }
    try {
      const parsed = JSON.parse(manualConfigJson) as Record<string, unknown>;
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        addToast(t('common:validation.configMustBeObject'), 'error');
        return null;
      }
      if (schema) {
        const err = validateDataAgainstJsonSchema(schema, parsed);
        if (err) {
          addToast(t('common:validation.configSchemaMismatch', { error: err }), 'error');
          return null;
        }
      }
      return parsed;
    } catch {
      addToast(t('common:validation.invalidConfigJson'), 'error');
      return null;
    }
  };

  return {
    selectedImplementationId,
    configObject,
    setConfigObject,
    manualConfigJson,
    setManualConfigJson,
    useManualConfigEditor,
    setUseManualConfigEditor,
    filteredImplementations,
    selectedMeta,
    handleImplementationChange,
    buildConfigPayload,
  };
}
