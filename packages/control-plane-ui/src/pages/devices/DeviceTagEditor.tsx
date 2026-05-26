// Tag chip input + Save for a single device. PUT /api/admin/agent-devices/:id/tags
// is a full-set replace, so the editor manages the working tag list client-side
// and saves the full array on submit. Used inside the Identity card on
// FleetDeviceDetailPage.

import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { usePermission } from '@/hooks/usePermission';
import { useMutation } from '@/hooks/useMutation';
import { Badge, Button, Input, Stack } from '@/components/ui';
import { devicesApi } from '@/api/services';

interface Props {
  deviceId: string;
  initialTags: string[];
  onSaved?: (tags: string[]) => void;
}

export function DeviceTagEditor({ deviceId, initialTags, onSaved }: Props) {
  const { t } = useTranslation();
  const canEdit = usePermission('agent-devices:update');
  const [tags, setTags] = useState<string[]>(initialTags);
  const [draft, setDraft] = useState('');
  const [dirty, setDirty] = useState(false);

  // Reset working set when the device changes underneath us.
  useEffect(() => {
    setTags(initialTags);
    setDirty(false);
  }, [deviceId, initialTags]);

  const addTag = useCallback(() => {
    const next = draft.trim();
    if (!next) return;
    if (next.length > 64) return;
    if (tags.includes(next)) {
      setDraft('');
      return;
    }
    setTags([...tags, next]);
    setDraft('');
    setDirty(true);
  }, [draft, tags]);

  const removeTag = useCallback(
    (tag: string) => {
      setTags(tags.filter((x) => x !== tag));
      setDirty(true);
    },
    [tags],
  );

  const { mutate: save, loading: saving } = useMutation(
    () => devicesApi.setTags(deviceId, tags),
    {
      successMessage: t('pages:devices.tagsSaved', 'Tags saved'),
      onSuccess: (res) => {
        setDirty(false);
        onSaved?.(res?.tags ?? tags);
      },
    },
  );

  return (
    <Stack gap="sm">
      <div style={{ fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-muted)' }}>
        {t(
          'pages:devices.tagsHelp',
          'Free-form labels. Used by smart-group predicates (tags_contains / tags_contains_all) and filter chips on the Devices list.',
        )}
      </div>
      <Stack direction="horizontal" gap="xs" align="center" style={{ flexWrap: 'wrap' }}>
        {tags.length === 0 && (
          <span style={{ color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-sm)' }}>
            {t('pages:devices.noTags', 'No tags')}
          </span>
        )}
        {tags.map((tag) => (
          <Badge key={tag} variant="outline">
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--g-space-1)' }}>
              {tag}
              {canEdit && (
                <button
                  type="button"
                  onClick={() => removeTag(tag)}
                  aria-label={`remove ${tag}`}
                  style={{
                    background: 'none',
                    border: 0,
                    cursor: 'pointer',
                    padding: 'var(--g-space-0)',
                    color: 'inherit',
                    fontSize: 'var(--g-font-size-sm)',
                  }}
                >
                  ×
                </button>
              )}
            </span>
          </Badge>
        ))}
      </Stack>
      {canEdit && (
        <Stack direction="horizontal" gap="sm">
          <Input
            placeholder={t('pages:devices.addTagPlaceholder', 'add tag…')}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                addTag();
              }
            }}
            style={{ maxWidth: 240 }}
          />
          <Button variant="secondary" onClick={addTag} disabled={!draft.trim()}>
            {t('common:add')}
          </Button>
          <Button onClick={() => save(undefined)} disabled={!dirty || saving}>
            {t('common:save')}
          </Button>
        </Stack>
      )}
    </Stack>
  );
}
