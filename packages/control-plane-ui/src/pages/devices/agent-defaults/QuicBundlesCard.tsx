import type { Dispatch, SetStateAction } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, Stack, Button, Input } from '@/components/ui';
import styles from './SettingsAgentTab.module.css';

interface QuicBundlesCardProps {
  quicBundles: string[];
  setQuicBundles: Dispatch<SetStateAction<string[]>>;
  quicInputDraft: string;
  setQuicInputDraft: Dispatch<SetStateAction<string>>;
  setDirty: Dispatch<SetStateAction<boolean>>;
}

export function QuicBundlesCard({
  quicBundles,
  setQuicBundles,
  quicInputDraft,
  setQuicInputDraft,
  setDirty,
}: QuicBundlesCardProps) {
  const { t } = useTranslation();

  return (
    <div className={styles.settingsSection}>
      <div className={styles.sectionHeader}>
        <h3 className={styles.sectionTitle}>
          {t('pages:settings.quicFallback.title', 'QUIC fallback bundles (macOS)')}
        </h3>
        <p className={styles.helpTextSecondary}>
          {t('pages:settings.quicFallback.desc', 'macOS bundle IDs of apps whose UDP flows the agent will close to force HTTP/3 → HTTP/2 fallback. Browsers and Electron AI clients prefer h3 to QUIC-friendly endpoints (ChatGPT, Claude.ai, Cloudflare-fronted services); without this list the agent\'s TCP path never sees their requests. Add Chromium-based desktop apps you also want intercepted (Cursor, Claude Desktop, etc.). NEVER add system processes (com.apple.mDNSResponder, dhcpcd, etc.) — that breaks DNS and takes the host network down.')}
        </p>
      </div>

      <Card>
        <Stack gap="md">
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-2)' }}>
          {quicBundles.map((b) => (
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
                aria-label={t('pages:settings.quicFallback.remove', 'Remove')}
                onClick={() => {
                  setQuicBundles((prev) => prev.filter((x) => x !== b));
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
          {quicBundles.length === 0 && (
            <span className={styles.hintTextMuted}>
              {t('pages:settings.quicFallback.empty', 'No bundles configured — agent will not close any UDP flows.')}
            </span>
          )}
        </div>

        <div style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
          <Input
            type="text"
            value={quicInputDraft}
            onChange={(e) => setQuicInputDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                const v = quicInputDraft.trim();
                if (v && !quicBundles.includes(v)) {
                  setQuicBundles((prev) => [...prev, v]);
                  setDirty(true);
                }
                setQuicInputDraft('');
              }
            }}
            placeholder={t('pages:settings.quicFallback.placeholder', 'com.example.MyBrowser — press Enter to add')}
            style={{ flex: 1 }}
          />
          <Button
            variant="secondary"
            onClick={() => {
              const v = quicInputDraft.trim();
              if (v && !quicBundles.includes(v)) {
                setQuicBundles((prev) => [...prev, v]);
                setDirty(true);
              }
              setQuicInputDraft('');
            }}
            disabled={!quicInputDraft.trim()}
          >
            {t('pages:settings.quicFallback.add', 'Add')}
          </Button>
        </div>
        <p className={styles.helpTextSecondarySmall}>
          {t('pages:settings.quicFallback.howToFind', 'Find a Mac app\'s bundle ID with: defaults read /Applications/SomeApp.app/Contents/Info.plist CFBundleIdentifier')}
        </p>
        </Stack>
      </Card>
    </div>
  );
}
