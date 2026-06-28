// Two cards bundled in one file so DeviceGroupDetailPage stays readable:
// SmartMembershipCard (predicate editor + tag help) and
// GroupBulkActionsCard (force-refresh / rotate-cert fan-outs). Each is
// self-contained — owns its own state, mutations, and dialogs.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation } from '@/hooks/useMutation';
import {
  Card, Stack, Button, Badge,
} from '@/components/ui';
import { deviceGroupsApi, type BulkActionResponse, type BulkActionResult } from '@/api/services';
import styles from './DeviceGroupDetailPage.module.css';

// Smart Membership

// Predicate examples surfaced via "Insert example" buttons. Each fills the
// textarea so admins have a starting shape they can edit rather than guessing
// field names. The IdP-group example uses the external ID that the IdP
// returns; admins look it up in Settings → Identity Providers → Group mappings.
const EXAMPLE_TAG_OS = `{
  "all": [
    {"field": "os", "op": "eq", "value": "darwin"},
    {"field": "primaryIp", "op": "cidr", "value": "10.32.0.0/16"},
    {"field": "tags", "op": "tags_contains", "value": "finance"}
  ]
}`;

const EXAMPLE_IDP_GROUP = `{
  "all": [
    {"field": "idpGroup", "op": "idp_group_member", "value": "okta-engineering"}
  ]
}`;

const EXAMPLE_RECENT = `{
  "all": [
    {"field": "lastSeenAt", "op": "relative_seconds_within", "value": 3600}
  ]
}`;

interface SmartMembershipCardProps {
  groupId: string;
  /** non-null when the group is already in smart mode */
  currentQuery: unknown | null;
  canUpdate: boolean;
  onSaved: () => void;
}

export function SmartMembershipCard({ groupId, currentQuery, canUpdate, onSaved }: SmartMembershipCardProps) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState(() =>
    currentQuery ? JSON.stringify(currentQuery, null, 2) : ''
  );
  const [previewResult, setPreviewResult] = useState<{ matched: number; sample: string[] } | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);

  const isSmart = currentQuery !== null && currentQuery !== undefined;

  const { mutate: preview, loading: previewing } = useMutation<
    void,
    { matched: number; sample: string[] }
  >(
    async () => {
      setPreviewError(null);
      try {
        const parsed = JSON.parse(draft);
        return await deviceGroupsApi.previewMembership(parsed);
      } catch (e) {
        setPreviewError(e instanceof Error ? e.message : String(e));
        throw e;
      }
    },
    {
      onSuccess: (r) => setPreviewResult(r),
    },
  );

  const { mutate: save } = useMutation(
    async () => {
      try {
        const parsed = JSON.parse(draft);
        return await deviceGroupsApi.setMembershipQuery(groupId, parsed);
      } catch (e) {
        setPreviewError(e instanceof Error ? e.message : String(e));
        throw e;
      }
    },
    {
      successMessage: t('pages:deviceGroups.smartSaved', 'Smart predicate saved'),
      onSuccess: () => onSaved(),
    },
  );

  const { mutate: revertToStatic } = useMutation(
    () => deviceGroupsApi.setMembershipQuery(groupId, null),
    {
      successMessage: t('pages:deviceGroups.smartReverted', 'Reverted to static membership'),
      onSuccess: () => { setDraft(''); setPreviewResult(null); onSaved(); },
    },
  );

  return (
    <Card>
      <Stack gap="md">
        <Stack direction="horizontal" justify="between" align="center">
          <div>
            <div style={{ fontSize: 'var(--g-font-size-base)', fontWeight: 'var(--g-font-weight-semibold)' }}>
              {t('pages:deviceGroups.smartTitle', 'Smart membership')}
            </div>
            <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
              {t(
                'pages:deviceGroups.smartSubtitle',
                'Predicate-driven membership. Non-null query flips the group to smart mode; the Hub recompute job re-evaluates every 60s.',
              )}
            </div>
          </div>
          <Badge variant={isSmart ? 'warning' : 'default'}>
            {isSmart ? t('pages:deviceGroups.modeSmart', 'Smart') : t('pages:deviceGroups.modeStatic', 'Static')}
          </Badge>
        </Stack>

        <textarea
          aria-label={t('pages:deviceGroups.smartPredicateLabel', 'Smart membership predicate')}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder={EXAMPLE_TAG_OS}
          rows={10}
          readOnly={!canUpdate}
          aria-readonly={!canUpdate}
          tabIndex={canUpdate ? undefined : -1}
          style={{
            fontFamily: 'monospace',
            fontSize: 'var(--g-font-size-sm)',
            width: '100%',
            padding: 'var(--g-space-2)',
            borderRadius: 'var(--g-radius-sm)',
            border: '1px solid var(--color-border)',
            background: 'var(--color-bg-subtle)',
            ...(canUpdate ? {} : { pointerEvents: 'none', cursor: 'default' }),
          }}
        />

        {canUpdate && (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-1)', alignItems: 'center' }}>
            <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
              {t('pages:deviceGroups.predicateExamplesLabel', 'Insert example:')}
            </span>
            <Button variant="ghost" size="sm" onClick={() => setDraft(EXAMPLE_TAG_OS)}>
              {t('pages:deviceGroups.predicateExampleTagOs', 'Tag + OS')}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setDraft(EXAMPLE_IDP_GROUP)}>
              {t('pages:deviceGroups.predicateExampleIdpGroup', 'IdP group binding')}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setDraft(EXAMPLE_RECENT)}>
              {t('pages:deviceGroups.predicateExampleRecent', 'Recently active')}
            </Button>
          </div>
        )}

        <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)', lineHeight: 1.5 }}>
          <div>
            <strong>{t('pages:deviceGroups.predicateOperatorsLabel', 'Operators:')}</strong>{' '}
            {t(
              'pages:deviceGroups.predicateOperators',
              'eq, ne, in, nin, prefix, regex, cidr, lt/le/gt/ge (semver-aware), relative_seconds_within, idp_group_member, tags_contains, tags_contains_all.',
            )}
          </div>
          <div>
            <strong>{t('pages:deviceGroups.predicateShapeLabel', 'Top level:')}</strong>{' '}
            {t('pages:deviceGroups.predicateShape', 'exactly one of `all` (AND) or `any` (OR).')}
          </div>
          <div>
            <strong>{t('pages:deviceGroups.predicateIdpHintLabel', 'IdP-group binding:')}</strong>{' '}
            {t(
              'pages:deviceGroups.predicateIdpHint',
              'use `{"field":"idpGroup","op":"idp_group_member","value":"<external-group-id>"}` to fold every device whose bound user is a member of the external IdP group into this device group. The external ID is what your IdP returns in SCIM groups / OIDC `groups` claim — Settings → Identity Providers → Group mappings shows what IDs Nexus currently knows about.',
            )}
          </div>
        </div>

        {previewError && (
          <div style={{ color: 'var(--color-danger)', fontSize: 'var(--g-font-size-sm)' }}>
            {previewError}
          </div>
        )}
        {previewResult && (
          <div style={{ fontSize: 'var(--g-font-size-sm)' }}>
            {t('pages:deviceGroups.previewMatched', '{{count}} devices match', { count: previewResult.matched })}
            {previewResult.sample.length > 0 && (
              <code style={{ display: 'block', marginTop: 'var(--g-space-1-5)', fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
                {previewResult.sample.slice(0, 10).join(', ')}
                {previewResult.sample.length > 10 ? ', …' : ''}
              </code>
            )}
          </div>
        )}

        {canUpdate && (
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button variant="ghost" onClick={() => preview(undefined)} disabled={!draft.trim() || previewing}>
              {t('pages:deviceGroups.previewMembers', 'Preview matches')}
            </Button>
            <Button onClick={() => save(undefined)} disabled={!draft.trim()}>
              {t('pages:deviceGroups.saveSmart', 'Save predicate')}
            </Button>
            {isSmart && (
              <Button variant="danger" onClick={() => revertToStatic(undefined)}>
                {t('pages:deviceGroups.revertStatic', 'Revert to static')}
              </Button>
            )}
          </Stack>
        )}
      </Stack>
    </Card>
  );
}


// Bulk actions

interface GroupBulkActionsCardProps {
  groupId: string;
  canUpdate: boolean;
}

export function GroupBulkActionsCard({ groupId, canUpdate }: GroupBulkActionsCardProps) {
  const { t } = useTranslation();
  const [lastResult, setLastResult] = useState<BulkActionResponse | null>(null);

  const { mutate: forceRefresh, loading: refreshing } = useMutation(
    () => deviceGroupsApi.bulkForceRefresh(groupId),
    {
      onSuccess: (r) => setLastResult(r),
      successMessage: t('pages:deviceGroups.bulkDone', 'Bulk action complete'),
    },
  );

  const { mutate: rotateCert, loading: rotating } = useMutation(
    () => deviceGroupsApi.bulkRotateCert(groupId),
    {
      onSuccess: (r) => setLastResult(r),
      successMessage: t('pages:deviceGroups.bulkDone', 'Bulk action complete'),
    },
  );

  if (!canUpdate) return null;

  return (
    <Card>
      <Stack gap="md">
        <div>
          <div style={{ fontSize: 'var(--g-font-size-base)', fontWeight: 'var(--g-font-weight-semibold)' }}>
            {t('pages:deviceGroups.bulkTitle', 'Bulk actions')}
          </div>
          <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
            {t('pages:deviceGroups.bulkSubtitle', 'Fan out per-device admin actions across every member with bounded parallelism.')}
          </div>
        </div>
        <Stack direction="horizontal" gap="sm">
          <Button className={styles.actionButtonText} variant="secondary" onClick={() => forceRefresh(undefined)} loading={refreshing}>
            {t('pages:deviceGroups.bulkForceRefresh', 'Force config refresh')}
          </Button>
          <Button className={styles.actionButtonText} variant="secondary" onClick={() => rotateCert(undefined)} loading={rotating}>
            {t('pages:deviceGroups.bulkRotateCert', 'Rotate certs')}
          </Button>
        </Stack>
        {lastResult && (
          <div style={{ fontSize: 'var(--g-font-size-sm)' }}>
            <Stack direction="horizontal" gap="sm" align="center">
              <Badge variant={lastResult.failed > 0 ? 'warning' : 'success'}>{lastResult.action}</Badge>
              <span>
                {t('pages:deviceGroups.bulkResult', '{{succeeded}}/{{total}} succeeded', {
                  succeeded: lastResult.succeeded,
                  total: lastResult.total,
                })}
              </span>
            </Stack>
            {lastResult.failed > 0 && (
              <div style={{ marginTop: 'var(--g-space-2)', fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
                {lastResult.results
                  .filter((r: BulkActionResult) => !r.ok)
                  .slice(0, 5)
                  .map((r: BulkActionResult) => (
                    <div key={r.deviceId}>
                      <code>{r.deviceId}</code>: {r.error}
                    </div>
                  ))}
              </div>
            )}
          </div>
        )}
      </Stack>
    </Card>
  );
}
