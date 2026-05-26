// src/pages/setup/useSetupWizard.ts
import { useState, useEffect, useCallback, useMemo } from 'react';
import {
  systemApi, organizationApi, providerApi, credentialApi,
  projectApi, virtualKeyApi, routingApi,
} from '@/api/services';
import type {
  Organization, Provider, Credential, Project, VirtualKey, RoutingRule,
} from '@/api/types';

export type StepStatus = 'loading' | 'complete' | 'incomplete' | 'error' | 'skipped';

export interface StepResult {
  status: StepStatus;
  data?: unknown;
  error?: string;
}

export interface HealthCheckData {
  status: string;
  checks?: Record<string, string>;
}

export interface ProviderStepData {
  providers: Provider[];
  credentials: Credential[];
}

export const STEP_IDS = [
  'health_check',
  'organization',
  'provider',
  'project',
  'virtual_key',
  'routing_rule',
  'compliance',
] as const;

export type StepId = (typeof STEP_IDS)[number];

export const TOTAL_STEPS = STEP_IDS.length;

/** 0..6 = steps, 7 = summary page */
export type StepIndex = number;

async function detectHealthCheck(): Promise<StepResult> {
  try {
    const res = await systemApi.checkReady();
    const checks = res.checks ?? {};
    const allHealthy = res.status === 'ready' ||
      Object.values(checks).every((v) => v === 'ok' || v === 'ready');
    return {
      status: allHealthy ? 'complete' : 'incomplete',
      data: res as HealthCheckData,
    };
  } catch (e) {
    return { status: 'error', error: (e as Error).message };
  }
}

async function detectOrganizations(): Promise<StepResult> {
  try {
    const res = await organizationApi.list();
    const orgs = (res.data ?? []) as Organization[];
    return {
      status: orgs.length > 0 ? 'complete' : 'incomplete',
      data: orgs,
    };
  } catch (e) {
    return { status: 'error', error: (e as Error).message };
  }
}

async function detectProviders(): Promise<StepResult> {
  try {
    const [provRes, credRes] = await Promise.all([
      providerApi.list(),
      credentialApi.list(),
    ]);
    const providers = (provRes.data ?? []) as Provider[];
    const credentials = (credRes.data ?? []) as Credential[];
    const hasEnabledWithCred = providers.some(
      (p) => p.enabled && credentials.some((c) => c.providerId === p.id),
    );
    return {
      status: hasEnabledWithCred ? 'complete' : 'incomplete',
      data: { providers, credentials } as ProviderStepData,
    };
  } catch (e) {
    return { status: 'error', error: (e as Error).message };
  }
}

async function detectProjects(): Promise<StepResult> {
  try {
    const res = await projectApi.list();
    const projects = (res.data ?? []) as Project[];
    return {
      status: projects.length > 0 ? 'complete' : 'incomplete',
      data: projects,
    };
  } catch (e) {
    return { status: 'error', error: (e as Error).message };
  }
}

async function detectVirtualKeys(): Promise<StepResult> {
  try {
    const res = await virtualKeyApi.list();
    const keys = (res.data ?? []) as VirtualKey[];
    return {
      status: keys.length > 0 ? 'complete' : 'incomplete',
      data: keys,
    };
  } catch (e) {
    return { status: 'error', error: (e as Error).message };
  }
}

async function detectRoutingRules(): Promise<StepResult> {
  try {
    const res = await routingApi.list();
    const rules = (res.data ?? []) as RoutingRule[];
    return {
      status: rules.length > 0 ? 'complete' : 'incomplete',
      data: rules,
    };
  } catch (e) {
    return { status: 'error', error: (e as Error).message };
  }
}

const DETECTORS: Record<StepId, () => Promise<StepResult>> = {
  health_check: detectHealthCheck,
  organization: detectOrganizations,
  provider: detectProviders,
  project: detectProjects,
  virtual_key: detectVirtualKeys,
  routing_rule: detectRoutingRules,
  compliance: async () => ({ status: 'incomplete' }),
};

export function useSetupWizard() {
  const [currentStep, setCurrentStep] = useState<StepIndex>(0);
  const [results, setResults] = useState<Record<StepId, StepResult>>(() => {
    const init: Partial<Record<StepId, StepResult>> = {};
    for (const id of STEP_IDS) init[id] = { status: 'loading' };
    return init as Record<StepId, StepResult>;
  });
  const [initialLoading, setInitialLoading] = useState(true);

  // Run all detections in parallel on mount
  useEffect(() => {
    let cancelled = false;
    async function run() {
      const entries = await Promise.all(
        STEP_IDS.map(async (id) => {
          const result = await DETECTORS[id]();
          return [id, result] as const;
        }),
      );
      if (cancelled) return;
      const next: Partial<Record<StepId, StepResult>> = {};
      for (const [id, result] of entries) next[id] = result;
      setResults(next as Record<StepId, StepResult>);

      // Jump to first incomplete step, or summary if all done
      const firstIncomplete = STEP_IDS.findIndex(
        (id) => next[id]!.status !== 'complete' && next[id]!.status !== 'skipped',
      );
      setCurrentStep(firstIncomplete === -1 ? TOTAL_STEPS : firstIncomplete);
      setInitialLoading(false);
    }
    void run();
    return () => { cancelled = true; };
  }, []);

  const refreshStep = useCallback(async (stepId: StepId) => {
    setResults((prev) => ({ ...prev, [stepId]: { status: 'loading' } }));
    const result = await DETECTORS[stepId]();
    setResults((prev) => ({ ...prev, [stepId]: result }));
    return result;
  }, []);

  const goNext = useCallback(() => {
    setCurrentStep((prev) => Math.min(prev + 1, TOTAL_STEPS));
  }, []);

  const goBack = useCallback(() => {
    setCurrentStep((prev) => Math.max(prev - 1, 0));
  }, []);

  const goToStep = useCallback((index: StepIndex) => {
    if (index >= 0 && index <= TOTAL_STEPS) setCurrentStep(index);
  }, []);

  const skipCompliance = useCallback(() => {
    setResults((prev) => ({ ...prev, compliance: { status: 'skipped' } }));
    setCurrentStep((prev) => Math.min(prev + 1, TOTAL_STEPS));
  }, []);

  const completeCompliance = useCallback(() => {
    setResults((prev) => ({ ...prev, compliance: { status: 'complete' } }));
    setCurrentStep((prev) => Math.min(prev + 1, TOTAL_STEPS));
  }, []);

  const allRequiredComplete = useMemo(() => {
    return STEP_IDS.filter((id) => id !== 'compliance').every(
      (id) => results[id].status === 'complete',
    );
  }, [results]);

  const isOnSummary = currentStep === TOTAL_STEPS;

  return {
    currentStep,
    results,
    initialLoading,
    allRequiredComplete,
    isOnSummary,
    goNext,
    goBack,
    goToStep,
    refreshStep,
    skipCompliance,
    completeCompliance,
  };
}

/** Lightweight check used by SetupBanner — are all required steps satisfied? */
export async function checkAllSetupComplete(): Promise<boolean> {
  try {
    const [orgRes, provRes, credRes, projRes, vkRes, routeRes] = await Promise.all([
      organizationApi.list(),
      providerApi.list(),
      credentialApi.list(),
      projectApi.list(),
      virtualKeyApi.list(),
      routingApi.list(),
    ]);
    const orgs = orgRes.data ?? [];
    const providers = (provRes.data ?? []) as Provider[];
    const credentials = (credRes.data ?? []) as Credential[];
    const projects = projRes.data ?? [];
    const vks = vkRes.data ?? [];
    const rules = routeRes.data ?? [];
    const hasProviderWithCred = providers.some(
      (p) => p.enabled && credentials.some((c) => c.providerId === p.id),
    );
    return orgs.length > 0 && hasProviderWithCred && projects.length > 0 &&
      vks.length > 0 && rules.length > 0;
  } catch {
    return false;
  }
}
