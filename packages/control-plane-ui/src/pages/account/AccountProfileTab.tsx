import { useState, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useAuth } from '@/auth/context/AuthContext';
import { useApi } from '@/hooks/useApi';
import { api } from '@/api/client';
import { Card, Stack, Button, LoadingSpinner } from '@/components/ui';
import { browserTZ, formatDateTime, setDisplayTZ } from '@/lib/format';
import styles from './Account.module.css';

interface MyProfile {
  id: string;
  displayName: string;
  email?: string | null;
  status: string;
  /** Provisioning origin: "local" | "oidc" | "scim". Non-local = SSO account. */
  source?: string;
  roles?: string[];
  createdAt: string;
  /** IANA TZ name (e.g. "Asia/Shanghai") or null = use browser default. */
  preferredTimezone?: string | null;
}

/**
 * Group every IANA timezone the browser knows about by its region
 * prefix (e.g. "Asia", "Europe") so an admin from anywhere on Earth
 * can find their exact zone. Falls back to a curated short list on
 * older browsers that lack [Intl.supportedValuesOf].
 */
const FALLBACK_TIMEZONES = [
  'UTC',
  'Asia/Shanghai', 'Asia/Tokyo', 'Asia/Singapore', 'Asia/Kolkata', 'Asia/Dubai',
  'Europe/London', 'Europe/Paris', 'Europe/Berlin', 'Europe/Moscow',
  'America/New_York', 'America/Chicago', 'America/Denver',
  'America/Los_Angeles', 'America/Sao_Paulo',
  'Australia/Sydney', 'Pacific/Auckland',
];

interface TimezoneGroup {
  region: string;
  zones: string[];
}

function buildTimezoneGroups(): TimezoneGroup[] {
  // Intl.supportedValuesOf is in modern Chrome/Firefox/Safari since 2022.
  const values = (Intl as unknown as {
    supportedValuesOf?: (key: string) => string[];
  }).supportedValuesOf;
  const all = values ? values('timeZone') : FALLBACK_TIMEZONES;
  const map = new Map<string, string[]>();
  // UTC is its own region for the dropdown so users can find it
  // without scanning the alphabetical list.
  map.set('UTC', ['UTC']);
  for (const tz of all) {
    if (tz === 'UTC') continue;
    const slash = tz.indexOf('/');
    const region = slash === -1 ? 'Other' : tz.slice(0, slash);
    const list = map.get(region);
    if (list) list.push(tz); else map.set(region, [tz]);
  }
  return Array.from(map.entries())
    .sort(([a], [b]) => (a === 'UTC' ? -1 : b === 'UTC' ? 1 : a.localeCompare(b)))
    .map(([region, zones]) => ({ region, zones: zones.slice().sort() }));
}

const TIMEZONE_GROUPS = buildTimezoneGroups();

export function AccountProfileTab() {
  const { t } = useTranslation();
  const { principalType, refreshSession } = useAuth();

  const { data: user, loading, error, refetch } = useApi<MyProfile>(
    () => api.get<MyProfile>('/api/my/profile'),
    ['my', 'profile'],
  );

  const [editing, setEditing] = useState(false);
  const [displayName, setDisplayName] = useState('');
  const [email, setEmail] = useState('');
  const [preferredTimezone, setPreferredTimezone] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');
  const [saveSuccess, setSaveSuccess] = useState('');

  const [changingPassword, setChangingPassword] = useState(false);
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [pwSaving, setPwSaving] = useState(false);
  const [pwError, setPwError] = useState('');
  const [pwSuccess, setPwSuccess] = useState('');

  const startEdit = useCallback(() => {
    if (!user) return;
    setDisplayName(user.displayName ?? '');
    setEmail(user.email ?? '');
    setPreferredTimezone(user.preferredTimezone ?? '');
    setEditing(true);
    setSaveError('');
    setSaveSuccess('');
  }, [user]);

  const cancelEdit = useCallback(() => {
    setEditing(false);
    setSaveError('');
  }, []);

  const saveProfile = useCallback(async () => {
    setSaving(true);
    setSaveError('');
    setSaveSuccess('');
    try {
      // PATCH semantics: empty string clears (server NULLIFs to "use browser default").
      await api.patch('/api/my/profile', {
        username: displayName,
        email: email || undefined,
        preferredTimezone: preferredTimezone, // explicit empty string = clear
      });
      // Apply the new TZ to the running UI so timestamps re-render
      // immediately without waiting for the next page navigation.
      setDisplayTZ(preferredTimezone || null);
      await refreshSession();
      refetch();
      setEditing(false);
      setSaveSuccess(t('pages:account.profileSaved'));
    } catch (err: unknown) {
      setSaveError(err instanceof Error ? err.message : 'Save failed');
    } finally {
      setSaving(false);
    }
  }, [displayName, email, preferredTimezone, refreshSession, refetch, t]);

  const savePassword = useCallback(async () => {
    setPwSaving(true);
    setPwError('');
    setPwSuccess('');
    try {
      await api.patch('/api/my/profile', { currentPassword, newPassword });
      setChangingPassword(false);
      setCurrentPassword('');
      setNewPassword('');
      setPwSuccess(t('pages:account.passwordChanged'));
    } catch (err: unknown) {
      setPwError(err instanceof Error ? err.message : 'Password change failed');
    } finally {
      setPwSaving(false);
    }
  }, [currentPassword, newPassword, t]);

  // Effective TZ shown in the read-only profile card (resolves to
  // browser default when the user hasn't picked one explicitly).
  const effectiveTZ = useMemo(
    () => user?.preferredTimezone || browserTZ(),
    [user?.preferredTimezone],
  );

  if (loading && !user) return <LoadingSpinner />;
  if (error) return <p className={styles.errorText}>{error.message}</p>;
  if (!user) return null;

  const canEdit = principalType === 'admin_user';
  // Federated (SSO) accounts have no local password — sign in is delegated to
  // the external IdP, so the change-password surface is replaced with guidance.
  const isSsoAccount = !!user.source && user.source !== 'local';

  // Use the project-wide TZ-aware formatter for consistent timestamp rendering.
  const fmtDate = (s?: string | null) => formatDateTime(s);

  return (
    <Stack gap="lg">
      <Card>
        <div className={styles.sectionHeader}>
          <h2 className={styles.sectionTitle}>{t('pages:account.profileTitle')}</h2>
          {canEdit && !editing && (
            <Button variant="secondary" onClick={startEdit}>{t('common:edit')}</Button>
          )}
        </div>

        {saveSuccess && <p className={styles.successText}>{saveSuccess}</p>}

        {editing ? (
          <div className={styles.editForm}>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:account.displayName')}</label>
              <input
                className={styles.formInput}
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
              />
            </div>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:account.email')}</label>
              <input
                className={styles.formInput}
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:account.preferredTimezone')}</label>
              <select
                className={styles.formInput}
                value={preferredTimezone}
                onChange={(e) => setPreferredTimezone(e.target.value)}
              >
                <option value="">{t('pages:account.preferredTimezoneBrowser', { tz: browserTZ() })}</option>
                {TIMEZONE_GROUPS.map((g) => (
                  <optgroup key={g.region} label={g.region}>
                    {g.zones.map((tz) => (
                      <option key={tz} value={tz}>{tz}</option>
                    ))}
                  </optgroup>
                ))}
              </select>
            </div>
            {saveError && <p className={styles.errorText}>{saveError}</p>}
            <Stack direction="horizontal" gap="sm">
              <Button onClick={() => void saveProfile()} loading={saving}>{t('common:save')}</Button>
              <Button variant="secondary" onClick={cancelEdit}>{t('common:cancel')}</Button>
            </Stack>
          </div>
        ) : (
          <div className={styles.kvGrid}>
            <div>
              <div className={styles.kvLabel}>{t('pages:account.displayName')}</div>
              <div className={styles.kvValue}>{user.displayName || '--'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:account.email')}</div>
              <div className={styles.kvValue}>{user.email || '--'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:account.role')}</div>
              <div className={styles.kvValue}>
                <span className={styles.roleBadge}>{user.roles?.join(', ') || '--'}</span>
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:account.status')}</div>
              <div className={styles.kvValue}>{user.status}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:account.createdAt')}</div>
              <div className={styles.kvValue}>{fmtDate(user.createdAt)}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:account.preferredTimezone')}</div>
              <div className={styles.kvValue}>
                {effectiveTZ}
                {!user.preferredTimezone && (
                  <span className={styles.helperText}> {t('pages:account.preferredTimezoneAutoSuffix')}</span>
                )}
              </div>
            </div>
          </div>
        )}
      </Card>

      {canEdit && (
        <Card>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle}>{t('pages:account.changePassword')}</h2>
            {!isSsoAccount && !changingPassword && (
              <Button variant="danger" onClick={() => { setChangingPassword(true); setPwError(''); setPwSuccess(''); }}>
                {t('pages:account.changePassword')}
              </Button>
            )}
          </div>

          {isSsoAccount && (
            <p className={styles.helperText}>{t('pages:account.passwordSsoManaged')}</p>
          )}

          {!isSsoAccount && pwSuccess && <p className={styles.successText}>{pwSuccess}</p>}

          {!isSsoAccount && changingPassword && (
            <div className={styles.editForm}>
              <div className={styles.formField}>
                <label className={styles.formLabel}>{t('pages:account.currentPassword')}</label>
                <input
                  className={styles.formInput}
                  type="password"
                  value={currentPassword}
                  onChange={(e) => setCurrentPassword(e.target.value)}
                />
              </div>
              <div className={styles.formField}>
                <label className={styles.formLabel}>{t('pages:account.newPassword')}</label>
                <input
                  className={styles.formInput}
                  type="password"
                  value={newPassword}
                  onChange={(e) => setNewPassword(e.target.value)}
                />
              </div>
              {pwError && <p className={styles.errorText}>{pwError}</p>}
              <Stack direction="horizontal" gap="sm">
                <Button variant="danger" onClick={() => void savePassword()} loading={pwSaving}>{t('common:save')}</Button>
                <Button variant="secondary" onClick={() => { setChangingPassword(false); setPwError(''); }}>{t('common:cancel')}</Button>
              </Stack>
            </div>
          )}
        </Card>
      )}
    </Stack>
  );
}
