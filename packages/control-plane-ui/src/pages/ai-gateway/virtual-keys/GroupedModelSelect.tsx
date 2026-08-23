// GroupedModelSelect — the virtual key's model-access picker.
//
// Split from VirtualKeyCreate so the bulk-selection rules live somewhere a
// reader can take in at once. The rule they all serve: a bulk action may only
// touch what the admin can SEE. A button that grants models hidden by the
// search filter makes the key's real permissions differ from the list on screen,
// which is the same defect that made glob refs in an allow list unacceptable.
import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Stack, Tooltip, Input } from '@/components/ui';
import type { AdminModelsByProvider, VirtualKeyAllowedModelRef } from '@/api/types';
import styles from './VirtualKeyCreate.module.css';

function isRefSelected(selected: VirtualKeyAllowedModelRef[], providerId: string, modelId: string) {
  return selected.some(s => s.providerId === providerId && s.modelId === modelId);
}

export function GroupedModelSelect({
  groups,
  selected,
  onChange,
}: {
  groups: AdminModelsByProvider[];
  selected: VirtualKeyAllowedModelRef[];
  onChange: (refs: VirtualKeyAllowedModelRef[]) => void;
}) {
  const { t } = useTranslation();
  const [modelSearch, setModelSearch] = useState('');
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>(() => {
    if (groups.length > 5) {
      const map: Record<string, boolean> = {};
      for (const g of groups) map[g.provider?.id] = true;
      return map;
    }
    return {};
  });

  const q = modelSearch.toLowerCase();

  const filteredGroups = useMemo(() => {
    if (!q) return groups;
    return groups
      .map(g => ({
        ...g,
        models: g?.models?.filter(m => m.name.toLowerCase().includes(q) || m.code.toLowerCase().includes(q)),
      }))
      .filter(g => g?.models?.length > 0);
  }, [groups, q]);

  // Bulk actions operate on what is VISIBLE, and only on that.
  //
  // Select all used to grant every model in the catalogue regardless of the
  // search box: filter to "claude", click it, and the key silently gained every
  // OpenAI and Gemini model too. Deselect all had the mirror problem, clearing
  // selections the filter was hiding. Both made the key's real permissions
  // differ from the list the admin was looking at — the same defect that made
  // glob refs unacceptable, one layer up.
  //
  // Selecting MERGES rather than replaces, so a model chosen before the filter
  // was typed is not dropped by a bulk action aimed at something else.
  const visibleRefs = useMemo(
    () => filteredGroups.flatMap(g => (g?.models ?? []).map(m => ({ providerId: g.provider?.id, modelId: m.id }))),
    [filteredGroups],
  );

  const addRefs = (refs: VirtualKeyAllowedModelRef[]) => {
    const merged = [...selected];
    for (const r of refs) {
      if (!isRefSelected(merged, r.providerId, r.modelId)) merged.push(r);
    }
    onChange(merged);
  };
  const removeRefs = (refs: VirtualKeyAllowedModelRef[]) =>
    onChange(selected.filter(s => !refs.some(r => r.providerId === s.providerId && r.modelId === s.modelId)));

  const handleSelectAll = () => addRefs(visibleRefs);
  const handleDeselectAll = () => removeRefs(visibleRefs);

  // Per-provider bulk selection: the need that would otherwise send an operator
  // looking for a wildcard. It writes the same concrete refs a human would tick,
  // so what the key permits and what the picker shows stay identical.
  const groupRefs = (g: typeof filteredGroups[number]) =>
    (g?.models ?? []).map(m => ({ providerId: g.provider?.id, modelId: m.id }));
  const groupSelectedCount = (g: typeof filteredGroups[number]) =>
    groupRefs(g).filter(r => isRefSelected(selected, r.providerId, r.modelId)).length;

  const toggleCollapse = (providerId: string) =>
    setCollapsed(prev => ({ ...prev, [providerId]: !prev[providerId] }));

  return (
    <div className={styles.modelAccessWrapper}>
      <label className={styles.modelAccessLabel}>
        {t('pages:virtualKeys.modelAccess')}
        <Tooltip content={t('pages:virtualKeys.modelAccessTooltip')}>
          <span role="presentation">&#9432;</span>
        </Tooltip>
      </label>
      <Stack direction="horizontal" gap="xs" className={styles.modelSearchRow}>
        <Input
          placeholder={t('pages:virtualKeys.searchModels')}
          value={modelSearch}
          onChange={e => setModelSearch(e.target.value)}
          className={styles.modelSearchInput}
        />
        <button type="button" onClick={handleSelectAll} className={styles.modelSelectAllBtn}>
          {t('pages:virtualKeys.selectAll')}
        </button>
        <button type="button" onClick={handleDeselectAll} className={styles.modelSelectAllBtn}>
          {t('pages:virtualKeys.deselectAll')}
        </button>
      </Stack>
      <div className={styles.modelListContainer}>
        {filteredGroups.length === 0 ? (
          <div className={styles.emptyModelHint}>
            {groups.length === 0 ? t('pages:virtualKeys.noModelsAvailable') : t('pages:virtualKeys.noMatchingModels')}
          </div>
        ) : (
          filteredGroups.map(group => {
            const isCollapsed = collapsed[group.provider?.id] && !q;
            return (
              <div key={group.provider?.id} className={styles.providerGroupWrapper}>
                <div
                  onClick={() => toggleCollapse(group.provider?.id)}
                  className={styles.providerHeader}
                >
                  <span className={isCollapsed ? styles.collapseArrowClosed : styles.collapseArrowOpen}>&#9660;</span>
                  {group.provider?.displayName || group.provider?.name}
                  <span className={styles.providerCounter}>
                    ({groupSelectedCount(group)}/{group?.models?.length})
                  </span>
                  {/* Selects the models SHOWN for this provider — under a search
                      filter that is the filtered subset, never the whole group,
                      so the button can never grant more than the row displays.
                      stopPropagation because the header itself toggles collapse. */}
                  <button
                    type="button"
                    className={styles.modelSelectAllBtn}
                    onClick={e => {
                      e.stopPropagation();
                      const refs = groupRefs(group);
                      if (groupSelectedCount(group) === refs.length) removeRefs(refs);
                      else addRefs(refs);
                    }}
                  >
                    {groupSelectedCount(group) === (group?.models?.length ?? 0)
                      ? t('pages:virtualKeys.deselectAll')
                      : t('pages:virtualKeys.selectAll')}
                  </button>
                </div>
                {!isCollapsed && group?.models?.map(m => (
                  <label key={m.id} className={styles.modelLabel}>
                    <input
                      type="checkbox"
                      checked={isRefSelected(selected, group.provider?.id, m.id)}
                      onChange={e => {
                        if (e.target.checked) onChange([...selected, { providerId: group.provider?.id, modelId: m.id }]);
                        else onChange(selected.filter(s => !(s.providerId === group.provider?.id && s.modelId === m.id)));
                      }}
                    />
                    {m.name}
                    <span className={styles.modelIdHint}>({m.code})</span>
                  </label>
                ))}
              </div>
            );
          })
        )}
      </div>
      <div className={styles.modelAccessSummary}>
        {selected.length === 0 ? t('pages:virtualKeys.allModelsAllowed') : t('pages:virtualKeys.modelsSelected', { count: selected.length })}
      </div>
    </div>
  );
}
