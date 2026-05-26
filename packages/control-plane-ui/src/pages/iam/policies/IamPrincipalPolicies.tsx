import { useState } from 'react';
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useDebouncedValue } from '../../../hooks/useDebouncedValue';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, DataTable, ListFilterToolbar, AlertDialog,
  LoadingSpinner, ErrorBanner, Button, Stack, Card, Select,
} from '@/components/ui';
import type { IamPolicy, IamPolicyAttachment } from '../../../api/types';
import styles from '../_shared/Iam.module.css';

export function IamPrincipalPolicies() {
  const { t } = useTranslation();
  const { type, id } = useParams<{ type: string; id: string }>();
  const [selectedPolicyId, setSelectedPolicyId] = useState('');
  const [detaching, setDetaching] = useState<IamPolicyAttachment | null>(null);
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);

  const {
    data: attachments,
    loading,
    error,
    refetch,
  } = useApi<{ data: IamPolicyAttachment[] }>(
    () => {
      const q = debouncedSearch.trim();
      return iamApi.getPrincipalPolicies(type!, id!, q ? { q } : undefined);
    },
    ['admin', 'iam', 'principal-policies', type, id, debouncedSearch],
    { skip: !type || !id },
  );

  const { data: allPolicies } = useApi<{ data: IamPolicy[] }>(
    () => iamApi.listPolicies(),
    ['admin', 'iam', 'policies', 'list'],
  );

  // Break-glass window. Empty = permanent (default); one of the preset offsets
  // adds an `expiresAt` to the attach call. Engine.loadPolicies filters
  // expired attachments at SQL load time.
  const [expiryPreset, setExpiryPreset] = useState<'' | '1h' | '4h' | '24h'>('');
  const presetToISO = (p: '' | '1h' | '4h' | '24h'): string | undefined => {
    if (!p) return undefined;
    const ms = p === '1h' ? 3600_000 : p === '4h' ? 4 * 3600_000 : 24 * 3600_000;
    return new Date(Date.now() + ms).toISOString();
  };

  const { mutate: attachPolicy, loading: attachLoading } = useMutation(
    (policyId: string) =>
      iamApi.attachPrincipalPolicy(type!, id!, {
        policyId,
        expiresAt: presetToISO(expiryPreset),
      }),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => {
        setSelectedPolicyId('');
        setExpiryPreset('');
      },
      successMessage: 'Policy attached',
    },
  );

  const { mutate: detachPolicy } = useMutation(
    (attachmentId: string) =>
      iamApi.detachPrincipalPolicy(type!, id!, attachmentId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => {
        setDetaching(null);
      },
      successMessage: 'Policy detached',
    },
  );

  if (loading) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const rows = attachments?.data ?? [];

  const attachedPolicyIds = new Set(
    (attachments?.data ?? []).map((a) => a.policyId),
  );
  const availablePolicies = (allPolicies?.data ?? []).filter(
    (p) => !attachedPolicyIds.has(p.id),
  );

  const attachDisabled = !selectedPolicyId || attachLoading;

  return (
    <Stack gap="lg">
      <PageHeader
        title={`Policies for ${type}:${id}`}
        action={
          <Stack direction="horizontal" gap="sm" className={styles.editorToolbarWrap}>
            <Select
              value={selectedPolicyId}
              onValueChange={setSelectedPolicyId}
              options={availablePolicies.map((p) => ({ value: p.id, label: p.name }))}
              placeholder={t('pages:iam.selectPolicy')}
            />
            {/* Break-glass window. Default empty = permanent
                attach (today's behaviour); preset offsets add an
                `expiresAt` to the attach. */}
            <Select
              value={expiryPreset}
              onValueChange={(v) => setExpiryPreset(v as '' | '1h' | '4h' | '24h')}
              options={[
                { value: '', label: t('pages:iam.attachPermanent', 'Permanent') },
                { value: '1h', label: t('pages:iam.attach1h', 'Expires in 1h') },
                { value: '4h', label: t('pages:iam.attach4h', 'Expires in 4h') },
                { value: '24h', label: t('pages:iam.attach24h', 'Expires in 24h') },
              ]}
            />
            <Button
              onClick={() => {
                if (selectedPolicyId) attachPolicy(selectedPolicyId);
              }}
              disabled={attachDisabled}
            >
              {t('pages:iam.attachPolicy')}
            </Button>
          </Stack>
        }
      />

      <ListFilterToolbar
        searchPlaceholder={t('pages:iam.searchAttachedPolicies')}
        searchValue={search}
        onSearchChange={setSearch}
        meta={`${rows.length} attachment${rows.length === 1 ? '' : 's'}`}
      />

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={[
            {
              key: 'name',
              label: t('pages:iam.name'),
              render: (r) => <span>{r.policyName ?? r.name ?? '\u2014'}</span>,
            },
            {
              key: 'source',
              label: t('pages:iam.source'),
              render: (r) => (
                <span className={r.source === 'direct' ? styles.directBadge : styles.groupBadge}>
                  {r.source}
                </span>
              ),
            },
            {
              key: 'groupName',
              label: t('pages:iam.group'),
              render: (r) => <span>{r.groupName ?? '\u2014'}</span>,
            },
            {
              key: 'actions',
              label: t('pages:iam.actions'),
              render: (r) =>
                r.source === 'direct' ? (
                  <Button
                    variant="danger"
                    size="sm"
                    onClick={(e) => {
                      e.stopPropagation();
                      setDetaching(r);
                    }}
                  >
                    {t('pages:iam.detach')}
                  </Button>
                ) : null,
            },
          ]}
          data={rows}
          emptyMessage={t('pages:iam.noPoliciesAttachedToPrincipal')}
        />
      </Card>

      <AlertDialog
        open={!!detaching}
        onOpenChange={(open) => { if (!open) setDetaching(null); }}
        title={t('pages:iam.detachPolicy')}
        description={t('pages:iam.detachPolicyFromPrincipalConfirm', { name: detaching?.policyName ?? detaching?.name })}
        confirmLabel={t('pages:iam.detach')}
        onConfirm={() => { if (detaching) detachPolicy(detaching.id); }}
        variant="danger"
      />
    </Stack>
  );
}
