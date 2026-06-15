import type { Dispatch, SetStateAction } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, Stack, Button, Input } from '@/components/ui';
import styles from './SettingsAgentTab.module.css';

interface BypassBundlesCardProps {
  bypassBundles: string[];
  setBypassBundles: Dispatch<SetStateAction<string[]>>;
  bypassInputDraft: string;
  setBypassInputDraft: Dispatch<SetStateAction<string>>;
  setDirty: Dispatch<SetStateAction<boolean>>;
}

/**
 * Exempted bundles (bypassBundles) editor. Mirrors QuicBundlesCard, but the
 * semantics are the opposite and far more consequential for compliance: any
 * app whose SOURCE bundle ID is listed here is passed through by the macOS
 * agent WITHOUT inspection — no TLS bump, no audit row. It exists for trusted
 * tools whose pinned TLS genuinely breaks under inspection (e.g. a developer
 * CLI). Matching is by source bundle, never by host, so the same destination
 * stays inspected from every other app. A prominent warning makes the
 * visibility trade-off impossible to set by accident.
 */
export function BypassBundlesCard({
  bypassBundles,
  setBypassBundles,
  bypassInputDraft,
  setBypassInputDraft,
  setDirty,
}: BypassBundlesCardProps) {
  const { t } = useTranslation();

  const addDraft = () => {
    const v = bypassInputDraft.trim();
    if (v && !bypassBundles.includes(v)) {
      setBypassBundles((prev) => [...prev, v]);
      setDirty(true);
    }
    setBypassInputDraft('');
  };

  return (
    <Card>
      <Stack gap="md">
        <h3 style={{ margin: 'var(--g-space-0)' }}>
          {t('pages:settings.bypassBundles.title', 'Exempted bundles (macOS)')}
        </h3>
        <p className={styles.helpTextSecondary}>
          {t(
            'pages:settings.bypassBundles.desc',
            'macOS bundle IDs of apps the agent will pass through WITHOUT inspection. Use only for a trusted tool whose certificate-pinned TLS genuinely breaks under inspection (e.g. a developer CLI). Matching is by source app, never by destination — so the same host stays inspected from every other app. Leave empty unless you have a specific, reviewed reason.',
          )}
        </p>

        <p className={styles.warningCallout}>
          {t(
            'pages:settings.bypassBundles.warning',
            '⚠ Apps listed here are NOT inspected for compliance. Their AI traffic produces no audit records and is invisible to policy. Every entry is a deliberate, reviewed compliance carve-out — keep this list as short as possible, and remove entries once they are no longer needed.',
          )}
        </p>

        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-2)' }}>
          {bypassBundles.map((b) => (
            <span
              key={b}
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 'var(--g-space-2)',
                padding: 'var(--g-space-1) var(--g-space-3)',
                background: 'var(--color-surface-2)',
                border: '1px solid var(--color-border)',
                borderRadius: 'var(--radius-pill)',
                fontSize: 'var(--font-size-sm)',
                fontFamily: 'var(--g-font-family-mono)',
              }}
            >
              {b}
              <button
                type="button"
                aria-label={t('pages:settings.bypassBundles.remove', 'Remove')}
                onClick={() => {
                  setBypassBundles((prev) => prev.filter((x) => x !== b));
                  setDirty(true);
                }}
                style={{
                  background: 'transparent',
                  border: 'none',
                  cursor: 'pointer',
                  padding: 'var(--g-space-0)',
                  color: 'var(--color-text-muted)',
                  fontSize: 'var(--font-size-md)',
                  lineHeight: 1,
                }}
              >
                ×
              </button>
            </span>
          ))}
          {bypassBundles.length === 0 && (
            <span className={styles.hintTextMuted}>
              {t('pages:settings.bypassBundles.empty', 'No apps exempted — everything is inspected (recommended).')}
            </span>
          )}
        </div>

        <div style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
          <Input
            type="text"
            value={bypassInputDraft}
            onChange={(e) => setBypassInputDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                addDraft();
              }
            }}
            placeholder={t('pages:settings.bypassBundles.placeholder', 'com.example.MyTool — press Enter to add')}
            style={{ flex: 1 }}
          />
          <Button
            variant="secondary"
            onClick={addDraft}
            disabled={!bypassInputDraft.trim()}
          >
            {t('pages:settings.bypassBundles.add', 'Add')}
          </Button>
        </div>
        <p className={styles.helpTextSecondarySmall}>
          {t('pages:settings.bypassBundles.howToFind', 'Find a Mac app\'s bundle ID with: defaults read /Applications/SomeApp.app/Contents/Info.plist CFBundleIdentifier')}
        </p>
      </Stack>
    </Card>
  );
}
