import React, { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { PageHeader, Stack, Card, Button } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { serviceUrlsApi, type ServicePublicURLs } from '@/api/services';
import { devicesApi, type MyAgentDevice } from '@/api/services/devices/devices';
import styles from './InfraAgentSetupPage.module.css';

type Platform = 'macos' | 'windows' | 'linux';
type FaqCategory = 'trust' | 'coverage' | 'network' | 'performance' | 'logs' | 'lifecycle';

const PLATFORMS: Platform[] = ['macos', 'windows', 'linux'];
const CATEGORIES: FaqCategory[] = ['trust', 'coverage', 'network', 'performance', 'logs', 'lifecycle'];

const PLATFORM_LABEL_KEYS: Record<Platform, string> = {
  macos: 'infrastructure.platformMacOS',
  windows: 'infrastructure.platformWindows',
  linux: 'infrastructure.platformLinux',
};

// Per-platform installation step keys. Step 1 for Linux is rendered
// inline as two code blocks (deb + rpm) so it's not in this array.
const INSTALL_STEPS: Record<Platform, string[]> = {
  macos: [
    'infrastructure.agentSetup.installMacOSStep1',
    'infrastructure.agentSetup.installMacOSStep2',
    'infrastructure.agentSetup.installMacOSStep3',
    'infrastructure.agentSetup.installMacOSStep4',
  ],
  windows: [
    'infrastructure.agentSetup.installWindowsStep1',
    'infrastructure.agentSetup.installWindowsStep2',
    'infrastructure.agentSetup.installWindowsStep3',
    'infrastructure.agentSetup.installWindowsStep4',
  ],
  linux: [
    'infrastructure.agentSetup.installLinuxStep2',
    'infrastructure.agentSetup.installLinuxStep3',
    'infrastructure.agentSetup.installLinuxStep4',
  ],
};

const ENROLL_SSO_HINT_KEY: Record<Platform, string> = {
  macos: 'infrastructure.agentSetup.enrollSSOMacOS',
  windows: 'infrastructure.agentSetup.enrollSSOWindows',
  linux: 'infrastructure.agentSetup.enrollSSOLinux',
};

const DOWNLOAD_HINT_KEY: Record<Platform, string> = {
  macos: 'infrastructure.agentSetup.downloadHintMacOS',
  windows: 'infrastructure.agentSetup.downloadHintWindows',
  linux: 'infrastructure.agentSetup.downloadHintLinux',
};

const DOWNLOAD_FILENAME_KEY: Record<Platform, string> = {
  macos: 'infrastructure.agentSetup.downloadFilenameMacOS',
  windows: 'infrastructure.agentSetup.downloadFilenameWindows',
  linux: 'infrastructure.agentSetup.downloadFilenameLinux',
};

// Direct download URLs hosted at /downloads/ on the same origin as
// the admin UI:
//   - macOS: signed + notarized .pkg
//   - Windows: WiX v4 MSI — registers Windows Service + WinDivert driver +
//     sets NEXUS_DEVICE_CA_PEM / NODE_EXTRA_CA_CERTS / REQUESTS_CA_BUNDLE /
//     SSL_CERT_FILE system-wide so Node / Python / Go clients trust the agent.
//   - Linux: raw Go binary; operators run it under their own systemd unit.
const DOWNLOAD_URL: Record<Platform, string> = {
  macos: '/downloads/NexusAgent-latest.pkg',
  windows: '/downloads/NexusAgent-windows-latest.msi',
  linux: '/downloads/nexus-agent-linux-latest',
};

const DOWNLOAD_AVAILABLE: Record<Platform, boolean> = {
  macos: true,
  windows: true,
  linux: true,
};

// Category → emoji icon. Used as a single-character visual anchor in the
// FAQ accordion summary so scanning a long list is fast (per Nielsen,
// icon+label scan rate ~3x text-only). Keep these in sync with the six
// IDs declared in `infrastructure.agentSetup.faq.categories.*` (i18n).
const CATEGORY_ICON: Record<FaqCategory, string> = {
  trust: '🔐',
  coverage: '🎯',
  network: '🌐',
  performance: '⚡',
  logs: '📋',
  lifecycle: '🚀',
};

// Common FAQ catalog — the answers apply to all three platforms (text
// usually contains per-OS subsections). Keep the order roughly aligned
// with the user journey: lifecycle (upgrade/uninstall/start) →
// trust (SSL errors) → coverage (what's intercepted) →
// network → performance → logs.
type CommonFaqMeta = { id: string; category: FaqCategory };
const COMMON_FAQ: CommonFaqMeta[] = [
  { id: 'upgrade', category: 'lifecycle' },
  { id: 'uninstall', category: 'lifecycle' },
  { id: 'serviceWontStart', category: 'lifecycle' },
  { id: 'nodeSelfSigned', category: 'trust' },
  { id: 'pythonCertVerify', category: 'trust' },
  { id: 'browserCertWarn', category: 'trust' },
  { id: 'browserAINotCaptured', category: 'coverage' },
  { id: 'cursorBumpFailed', category: 'coverage' },
  { id: 'sshGitTransparent', category: 'coverage' },
  { id: 'hubConnLost', category: 'network' },
  { id: 'performance', category: 'performance' },
  { id: 'auditQueueHigh', category: 'performance' },
  { id: 'logsWhere', category: 'logs' },
];

// Per-platform FAQ items shown only when the matching platform tab is
// selected. These are genuinely OS-specific (macOS NE toggle, Windows
// kernel-driver issues, Linux systemd / iptables nuances) — not just
// platform-flavored answers to a common question.
type PlatformFaqMeta = { id: string; category: FaqCategory };
const PLATFORM_FAQ: Record<Platform, PlatformFaqMeta[]> = {
  macos: [
    { id: 'systemExtensionBlocked', category: 'lifecycle' },
  ],
  windows: [
    { id: 'windivertFailed', category: 'lifecycle' },
    { id: 'msiHang', category: 'lifecycle' },
  ],
  linux: [
    { id: 'whyRoot', category: 'lifecycle' },
    { id: 'firewallReload', category: 'lifecycle' },
  ],
};

export default function InfraAgentSetupPage() {
  const { t } = useTranslation('pages');
  const [platform, setPlatform] = useState<Platform>('macos');
  const [searchQuery, setSearchQuery] = useState('');
  const [activeCategories, setActiveCategories] = useState<Set<FaqCategory>>(new Set());

  // Source the download host from the Control Plane's own publicURL as
  // reported via thing-staticInfo. Fall back to window.origin if the
  // lookup is in flight or the registered Thing left publicURL blank.
  const { data: services } = useApi<ServicePublicURLs>(
    serviceUrlsApi.publicURLs,
    ['admin', 'services', 'public-urls'],
  );
  const downloadBase =
    services?.controlPlane?.replace(/\/+$/, '') ||
    (typeof window !== 'undefined' ? window.location.origin : '');

  // Filter logic: search query (case-insensitive substring on q + a) AND
  // category selection (if any chips active). Empty filters = show all.
  // We memo against the t() function indirectly via the language tag so
  // the filtered lists update on locale change.
  const commonFaqFiltered = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    return COMMON_FAQ.filter((item) => {
      if (activeCategories.size > 0 && !activeCategories.has(item.category)) return false;
      if (q) {
        const text = (
          t(`infrastructure.agentSetup.faq.common.${item.id}.q`) +
          ' ' +
          t(`infrastructure.agentSetup.faq.common.${item.id}.a`)
        ).toLowerCase();
        if (!text.includes(q)) return false;
      }
      return true;
    });
  }, [searchQuery, activeCategories, t]);

  const platformFaqFiltered = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    return PLATFORM_FAQ[platform].filter((item) => {
      if (activeCategories.size > 0 && !activeCategories.has(item.category)) return false;
      if (q) {
        const text = (
          t(`infrastructure.agentSetup.faq.platform.${platform}.${item.id}.q`) +
          ' ' +
          t(`infrastructure.agentSetup.faq.platform.${platform}.${item.id}.a`)
        ).toLowerCase();
        if (!text.includes(q)) return false;
      }
      return true;
    });
  }, [platform, searchQuery, activeCategories, t]);

  const toggleCategory = (c: FaqCategory) => {
    setActiveCategories((prev) => {
      const next = new Set(prev);
      if (next.has(c)) next.delete(c);
      else next.add(c);
      return next;
    });
  };

  const noResults = commonFaqFiltered.length === 0 && platformFaqFiltered.length === 0;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.agentSetupTitle')}
        subtitle={t('infrastructure.agentSetupDescription')}
      />

      {/* ─── Install card — platform-scoped install/enroll/verify flow ─── */}
      <Card>
        <Stack gap="md">
          <div className={styles.platformRow}>
            {PLATFORMS.map((p) => (
              <Button
                key={p}
                variant={platform === p ? 'primary' : 'secondary'}
                size="sm"
                onClick={() => setPlatform(p)}
              >
                {t(PLATFORM_LABEL_KEYS[p])}
              </Button>
            ))}
          </div>

          {/* 1. Download */}
          <section>
            <h3 className={styles.sectionHeading}>
              {t('infrastructure.agentSetup.downloadTitle')}
            </h3>
            <p>{t(DOWNLOAD_HINT_KEY[platform])}</p>
            <div className={styles.downloadRow}>
              {DOWNLOAD_AVAILABLE[platform] ? (
                <>
                  <Button
                    variant="primary"
                    size="sm"
                    onClick={() => {
                      window.location.href = DOWNLOAD_URL[platform];
                    }}
                  >
                    {t('infrastructure.agentSetup.downloadButton')}
                  </Button>
                  <span className={styles.downloadUrl}>{DOWNLOAD_URL[platform]}</span>
                </>
              ) : (
                <>
                  <span className={styles.unavailableBadge}>
                    {t('infrastructure.agentSetup.downloadUnavailable')}
                  </span>
                  <code className={styles.filename}>
                    {t(DOWNLOAD_FILENAME_KEY[platform])}
                  </code>
                </>
              )}
            </div>
          </section>

          {/* 2. Install */}
          <section>
            <h3 className={styles.sectionHeading}>
              {t('infrastructure.agentSetup.installTitle')}
            </h3>
            <ol className={styles.steps}>
              {platform === 'linux' && (
                <li>
                  <pre className={styles.codeBlock}>
                    <code>{t('infrastructure.agentSetup.installLinuxStep1Deb', { downloadBase })}</code>
                  </pre>
                  <pre className={styles.codeBlock}>
                    <code>{t('infrastructure.agentSetup.installLinuxStep1Rpm', { downloadBase })}</code>
                  </pre>
                </li>
              )}
              {INSTALL_STEPS[platform].map((key) => (
                <li key={key}>{t(key, { downloadBase })}</li>
              ))}
            </ol>
          </section>

          {/* 3. Enroll — SSO only. Headless servers run `nexus-agent enroll-sso`
              which prints a URL to open on a workstation. The legacy token flow
              was removed because it was misleading new operators. */}
          <section>
            <h3 className={styles.sectionHeading}>
              {t('infrastructure.agentSetup.enrollTitle')}
            </h3>
            <p>{t(ENROLL_SSO_HINT_KEY[platform])}</p>
          </section>

          {/* 4. Verify — live device status panel. Polls every 5 s while
              the page is mounted so a fresh enrollment appears within
              one cycle. Falls back to the original static hint when
              no devices are bound to this admin user yet. */}
          <section>
            <h3 className={styles.sectionHeading}>
              {t('infrastructure.agentSetup.verifyTitle')}
            </h3>
            <p>{t('infrastructure.agentSetup.verifyHint')}</p>
            <VerifyLiveStatusPanel />
          </section>
        </Stack>
      </Card>

      {/* ─── Troubleshooting card — full-width, NOT tab-scoped (common QAs
              are visible regardless of which platform tab is active; the
              platform-specific subsection follows the selected tab). ─── */}
      <Card>
        <Stack gap="md">
          <div>
            <h3 className={styles.sectionHeading}>
              {t('infrastructure.agentSetup.faqTitle')}
            </h3>
            <p className={styles.faqSubtitle}>
              {t('infrastructure.agentSetup.faqSubtitle')}
            </p>
          </div>

          {/* Search input */}
          <input
            type="search"
            className={styles.faqSearch}
            placeholder={t('infrastructure.agentSetup.faqSearchPlaceholder')}
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            aria-label={t('infrastructure.agentSetup.faqSearchPlaceholder')}
          />

          {/* Category filter chips */}
          <div className={styles.faqChipRow} role="group" aria-label="Filter by category">
            {CATEGORIES.map((cat) => {
              const isActive = activeCategories.has(cat);
              return (
                <button
                  key={cat}
                  type="button"
                  className={`${styles.faqChip} ${isActive ? styles.faqChipActive : ''}`}
                  onClick={() => toggleCategory(cat)}
                  aria-pressed={isActive}
                >
                  <span aria-hidden="true">{CATEGORY_ICON[cat]}</span>
                  {t(`infrastructure.agentSetup.faq.categories.${cat}`)}
                </button>
              );
            })}
            {activeCategories.size > 0 && (
              <button
                type="button"
                className={styles.faqChipClear}
                onClick={() => setActiveCategories(new Set())}
              >
                {t('infrastructure.agentSetup.faqFilterAll')}
              </button>
            )}
          </div>

          {/* Common questions subsection */}
          {commonFaqFiltered.length > 0 && (
            <section>
              <h4 className={styles.faqSubheading}>
                {t('infrastructure.agentSetup.faqCommonSectionTitle')}
                <span className={styles.faqCount}>({commonFaqFiltered.length})</span>
              </h4>
              <div className={styles.faqList}>
                {commonFaqFiltered.map((item) => (
                  <details key={item.id} className={styles.faqItem}>
                    <summary className={styles.faqSummary}>
                      <span aria-hidden="true" className={styles.faqCategoryIcon}>
                        {CATEGORY_ICON[item.category]}
                      </span>
                      <span className={styles.faqQuestion}>
                        {t(`infrastructure.agentSetup.faq.common.${item.id}.q`)}
                      </span>
                      <span className={styles.faqBadgeAll}>
                        {t('infrastructure.agentSetup.faqPlatformBadgeAll')}
                      </span>
                    </summary>
                    <div className={styles.faqBody}>
                      {t(`infrastructure.agentSetup.faq.common.${item.id}.a`)}
                    </div>
                  </details>
                ))}
              </div>
            </section>
          )}

          {/* Platform-specific questions subsection — only the selected
              platform's items render. */}
          {platformFaqFiltered.length > 0 && (
            <section>
              <h4 className={styles.faqSubheading}>
                {t('infrastructure.agentSetup.faqPlatformSectionTitle', {
                  platform: t(PLATFORM_LABEL_KEYS[platform]),
                })}
                <span className={styles.faqCount}>({platformFaqFiltered.length})</span>
              </h4>
              <div className={styles.faqList}>
                {platformFaqFiltered.map((item) => (
                  <details key={`${platform}-${item.id}`} className={styles.faqItem}>
                    <summary className={styles.faqSummary}>
                      <span aria-hidden="true" className={styles.faqCategoryIcon}>
                        {CATEGORY_ICON[item.category]}
                      </span>
                      <span className={styles.faqQuestion}>
                        {t(`infrastructure.agentSetup.faq.platform.${platform}.${item.id}.q`)}
                      </span>
                      <span className={`${styles.faqBadgePlatform} ${styles[`faqBadge_${platform}`]}`}>
                        {t(PLATFORM_LABEL_KEYS[platform])}
                      </span>
                    </summary>
                    <div className={styles.faqBody}>
                      {t(`infrastructure.agentSetup.faq.platform.${platform}.${item.id}.a`)}
                    </div>
                  </details>
                ))}
              </div>
            </section>
          )}

          {/* Empty state */}
          {noResults && (
            <div className={styles.faqEmpty}>
              {t('infrastructure.agentSetup.faqEmptyState')}
            </div>
          )}
        </Stack>
      </Card>
    </Stack>
  );
}

// Live device status for the Agent Setup page's Verify step.
//
// Calls GET /api/admin/me/agent-devices on a 5 s refetch interval.
// Endpoint is self-scoped to the caller (no IAM gate needed for
// listing your own enrolled devices). When the user has not yet
// enrolled anything we render a friendly "waiting for first
// enrollment" hint instead of an empty table.
//
// Status decoration: ✅ online (last heartbeat < 30 s) — ⏳ enrolled
// but not yet seen — ❌ offline / drift / revoked. The 30 s threshold
// matches the agent's heartbeat cadence (paths_windows.go &
// linux.go use 15 s, so 30 s = 2 missed heartbeats; gives the agent
// one slack interval before flipping to ❌).
function VerifyLiveStatusPanel() {
  const { t } = useTranslation('pages');
  const { data, error, loading } = useApi<{ data: MyAgentDevice[]; total: number }>(
    devicesApi.listMine,
    ['admin', 'me', 'agent-devices'],
    { refetchInterval: 5000 },
  );

  if (loading && !data) {
    return (
      <div className={styles.verifyPanelLoading}>
        {t('infrastructure.agentSetup.verifyLoading')}
      </div>
    );
  }

  if (error) {
    return (
      <div className={styles.verifyPanelError}>
        {t('infrastructure.agentSetup.verifyError')}
      </div>
    );
  }

  const devices = data?.data ?? [];
  if (devices.length === 0) {
    return (
      <div className={styles.verifyPanelEmpty}>
        <div className={styles.verifyPanelEmptyTitle}>
          {t('infrastructure.agentSetup.verifyEmptyTitle')}
        </div>
        <div className={styles.verifyPanelEmptyHint}>
          {t('infrastructure.agentSetup.verifyEmptyHint')}
        </div>
      </div>
    );
  }

  return (
    <div className={styles.verifyPanel}>
      <table className={styles.verifyTable}>
        <thead>
          <tr>
            <th>{t('infrastructure.agentSetup.verifyColStatus')}</th>
            <th>{t('infrastructure.agentSetup.verifyColHostname')}</th>
            <th>{t('infrastructure.agentSetup.verifyColOS')}</th>
            <th>{t('infrastructure.agentSetup.verifyColAgentVer')}</th>
            <th>{t('infrastructure.agentSetup.verifyColLastSeen')}</th>
          </tr>
        </thead>
        <tbody>
          {devices.map((d) => (
            <tr key={d.id}>
              <td>{renderStatusBadge(d, t)}</td>
              <td className={styles.verifyHostname}>{d.hostname || d.id}</td>
              <td>{[d.os, d.osVersion].filter(Boolean).join(' ')}</td>
              <td className={styles.verifyMono}>{d.agentVersion || '—'}</td>
              <td>{formatLastSeen(d.lastHeartbeat, t)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// Status badge derived from (status, lastHeartbeat). Threshold: <30s
// = online; status=enrolled with no heartbeat yet = waiting; anything
// else = offline / problem. Keeps the per-row icon to a single glyph
// for fast scanning in a list of devices.
function renderStatusBadge(
  d: MyAgentDevice,
  t: (key: string) => string,
): React.ReactElement {
  const heartbeatAgeMs = d.lastHeartbeat
    ? Date.now() - new Date(d.lastHeartbeat).getTime()
    : Infinity;

  if (d.status === 'revoked') {
    return (
      <span className={`${styles.verifyBadge} ${styles.verifyBadgeError}`}>
        ❌ {t('infrastructure.agentSetup.verifyStatusRevoked')}
      </span>
    );
  }
  if (heartbeatAgeMs < 30_000) {
    return (
      <span className={`${styles.verifyBadge} ${styles.verifyBadgeOk}`}>
        ✅ {t('infrastructure.agentSetup.verifyStatusOnline')}
      </span>
    );
  }
  if (d.status === 'enrolled' && !d.lastHeartbeat) {
    return (
      <span className={`${styles.verifyBadge} ${styles.verifyBadgeWait}`}>
        ⏳ {t('infrastructure.agentSetup.verifyStatusWaiting')}
      </span>
    );
  }
  return (
    <span className={`${styles.verifyBadge} ${styles.verifyBadgeError}`}>
      ❌ {t('infrastructure.agentSetup.verifyStatusOffline')}
    </span>
  );
}

// Human-friendly last-seen relative time. Falls back to "—" when
// the agent has never reported a heartbeat. We don't bother with a
// full i18n date-fns formatter here — three coarse buckets cover the
// install-verification use case (just-now / minutes / hours / older).
function formatLastSeen(
  ts: string | null,
  t: (key: string) => string,
): string {
  if (!ts) return '—';
  const ageMs = Date.now() - new Date(ts).getTime();
  if (ageMs < 30_000) return t('infrastructure.agentSetup.verifyLastSeenJustNow');
  if (ageMs < 60 * 60 * 1000) {
    return t('infrastructure.agentSetup.verifyLastSeenMinutesAgo').replace(
      '{{n}}',
      String(Math.floor(ageMs / 60000)),
    );
  }
  if (ageMs < 24 * 60 * 60 * 1000) {
    return t('infrastructure.agentSetup.verifyLastSeenHoursAgo').replace(
      '{{n}}',
      String(Math.floor(ageMs / 3_600_000)),
    );
  }
  return new Date(ts).toLocaleString();
}
