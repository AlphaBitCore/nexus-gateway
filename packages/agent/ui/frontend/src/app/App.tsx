import { useCallback, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Routes, Route, Navigate } from 'react-router-dom';
import { agentApi, NoBridgeError } from '@/api/agent';
import { Shell } from '@/layout/Shell';
import { AgentNotRunning } from '@/pages/diagnostics/AgentNotRunning';
import { Onboarding } from '@/pages/onboarding/Onboarding';
import { Reconnecting } from '@/pages/diagnostics/Reconnecting';
import { Overview } from '@/pages/overview/Overview';
import { Activity } from '@/pages/activity/Activity';
import { Traffic } from '@/pages/traffic/Traffic';
import { PoliciesOverview } from '@/pages/policies/Overview';
import { DomainsList } from '@/pages/policies/DomainsList';
import { DomainDetail } from '@/pages/policies/DomainDetail';
import { HooksList } from '@/pages/policies/HooksList';
import { HookDetail } from '@/pages/policies/HookDetail';
import { ExemptionsList } from '@/pages/policies/ExemptionsList';
import { RulePacksList } from '@/pages/policies/RulePacksList';
import { RulePackDetail } from '@/pages/policies/RulePackDetail';
import { UpdateBanner } from '@/components/UpdateBanner';
import { Stats } from '@/pages/activity/Stats';
import { Diagnostics } from '@/pages/diagnostics/Diagnostics';
import { Settings } from '@/pages/settings/Settings';

/** How long after an enrollment attempt to show "Finishing setup"
 *  instead of "Agent not running". Window is generous enough to
 *  cover launchd's typical respawn latency (few hundred ms) plus
 *  the agent's full-stack startup. */
const ENROLLMENT_GRACE_MS = 30_000;

/**
 * Root component. Drives three top-level branches:
 *
 *  1. Bridge unavailable / daemon unreachable → AgentNotRunning
 *  2. Daemon reports empty deviceID → Onboarding
 *  3. Otherwise → Shell with the steady-state pages
 *
 * "Finishing setup" grace screen is triggered **explicitly** by the
 * Onboarding component when the user submits a token / starts SSO
 * — not by passive observation of the daemon's pending state. That
 * avoids false-positives where an unrelated daemon crash incorrectly
 * shows "Finishing setup" on a fresh install.
 */
export function App() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['agent', 'status'],
    queryFn: () => agentApi.getStatus(),
    refetchInterval: 2_000,
    retry: (failureCount, err) => {
      // Don't retry NoBridgeError — it's a structural failure, not a
      // transient network blip. Surface the fallback immediately so
      // the Retry button has a clear handle.
      if (err instanceof NoBridgeError) return false;
      return failureCount < 2;
    },
  });

  const enrolled = !!data?.agent.deviceID;

  // Set when Onboarding kicks off a real enrollment attempt. Used to
  // hide AgentNotRunning behind a "Finishing setup" screen during the
  // window the daemon is exiting + being respawned by launchd.
  const [enrollmentAttemptedAt, setEnrollmentAttemptedAt] = useState<number | null>(null);
  const markEnrollmentAttempted = useCallback(() => {
    setEnrollmentAttemptedAt(Date.now());
  }, []);

  const inGraceWindow =
    enrollmentAttemptedAt !== null &&
    Date.now() - enrollmentAttemptedAt < ENROLLMENT_GRACE_MS;

  // Daemon is unreachable (error) or has never replied (no data) AND
  // the user just initiated enrollment → show Reconnecting instead
  // of AgentNotRunning. Once the daemon is back and enrolled we
  // clear the timestamp so the Shell renders cleanly.
  const showReconnecting = inGraceWindow && (Boolean(error) || !data);
  if (enrollmentAttemptedAt !== null && enrolled) {
    // Clear the grace marker — we don't need it anymore and any
    // future failure should fall through to AgentNotRunning.
    setEnrollmentAttemptedAt(null);
  }

  if (showReconnecting) {
    return <Reconnecting />;
  }
  if (error) {
    return <AgentNotRunning onRetry={() => refetch()} />;
  }
  if (isLoading || !data) {
    return <Shell><div /></Shell>;
  }
  if (!enrolled) {
    return <Onboarding status={data} onEnrollmentAttempted={markEnrollmentAttempted} />;
  }

  return (
    <Shell>
      <UpdateBanner status={data} />
      <Routes>
        <Route path="/" element={<Navigate to="/overview" replace />} />
        <Route path="/overview" element={<Overview status={data} />} />
        <Route path="/activity" element={<Activity />} />
        <Route path="/traffic" element={<Traffic />} />
        <Route path="/policies" element={<PoliciesOverview />} />
        <Route path="/policies/domains" element={<DomainsList />} />
        <Route path="/policies/domains/:id" element={<DomainDetail />} />
        <Route path="/policies/hooks" element={<HooksList />} />
        <Route path="/policies/hooks/:id" element={<HookDetail />} />
        <Route path="/policies/exemptions" element={<ExemptionsList />} />
        <Route path="/policies/rule-packs" element={<RulePacksList />} />
        <Route path="/policies/rule-packs/:id" element={<RulePackDetail />} />
        <Route path="/stats" element={<Stats />} />
        {/* /identity is gone — content folded into /settings (AccountPanel). */}
        <Route path="/identity" element={<Navigate to="/settings" replace />} />
        <Route path="/diagnostics" element={<Diagnostics />} />
        <Route path="/settings" element={<Settings status={data} />} />
        <Route path="*" element={<Navigate to="/overview" replace />} />
      </Routes>
    </Shell>
  );
}

