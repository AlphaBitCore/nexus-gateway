import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import {
  Badge, statusToVariant, FormField, Input, Switch, Tooltip, Button, Stack, Card,
} from '@/components/ui';
import type { VirtualKey, VirtualKeyAllowedModelRef, AdminModelsByProvider, Project } from '@/api/types';
import { formatDate } from '@/lib/format';
import styles from '../VirtualKeyDetail.module.css';

/* ── Grouped Model Selector ───────────────────────────────────────────── */

function isRefSelected(selected: VirtualKeyAllowedModelRef[], providerId: string, modelId: string) {
  return selected.some(s => s.providerId === providerId && s.modelId === modelId);
}

function GroupedModelSelect({
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

  const allRefs = useMemo(
    () => groups.flatMap(g => g?.models?.map(m => ({ providerId: g.provider?.id, modelId: m.id }))),
    [groups],
  );
  const q = modelSearch.toLowerCase();

  const filteredGroups = useMemo(() => {
    if (!q) return groups;
    return groups
      .map(g => ({ ...g, models: g?.models?.filter(m => m.name.toLowerCase().includes(q) || m.id.toLowerCase().includes(q)) }))
      .filter(g => g?.models?.length > 0);
  }, [groups, q]);

  return (
    <div className={styles.modelAccessWrapper}>
      <label className={styles.inlineLabel}>
        {t('pages:virtualKeys.modelAccess')}
      </label>
      <Stack direction="horizontal" gap="xs" align="center" className={styles.searchRow}>
        <Input placeholder={t('pages:virtualKeys.searchModels')} value={modelSearch} onChange={e => setModelSearch(e.target.value)}
          className={`${styles.inlineInput} ${styles.searchInputFlex}`} />
        <Button variant="ghost" size="sm" onClick={() => onChange([...allRefs])}>{t('pages:virtualKeys.selectAll')}</Button>
        <Button variant="ghost" size="sm" onClick={() => onChange([])}>{t('pages:virtualKeys.deselectAll')}</Button>
      </Stack>
      <div className={styles.modelSelectPanel}>
        {filteredGroups.length === 0 ? (
          <div className={styles.emptyModelHint}>
            {groups.length === 0 ? t('pages:virtualKeys.noModelsAvailable') : t('pages:virtualKeys.noMatchingModels')}
          </div>
        ) : filteredGroups.map(group => {
          const isCollapsed = collapsed[group.provider?.id] && !q;
          return (
            <div key={group.provider?.id} className={styles.providerGroup}>
              <div
                role="button"
                tabIndex={0}
                onClick={() => setCollapsed(prev => ({ ...prev, [group.provider?.id]: !prev[group.provider?.id] }))}
                onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setCollapsed(prev => ({ ...prev, [group.provider?.id]: !prev[group.provider?.id] })); } }}
                className={styles.modelGroupHeader}>
                <span className={isCollapsed ? styles.collapseArrowClosed : styles.collapseArrowOpen}>&#9660;</span>
                {group.provider?.displayName || group.provider?.name}
                <span className={styles.providerCounter}>({group?.models?.filter(m => isRefSelected(selected, group.provider?.id, m.id)).length}/{group?.models?.length})</span>
              </div>
              {!isCollapsed && group?.models?.map(m => (
                <label key={m.id} className={styles.modelCheckboxRow}>
                  <input type="checkbox" checked={isRefSelected(selected, group.provider?.id, m.id)}
                    onChange={e => {
                      if (e.target.checked) onChange([...selected, { providerId: group.provider?.id, modelId: m.id }]);
                      else onChange(selected.filter(s => !(s.providerId === group.provider?.id && s.modelId === m.id)));
                    }} />
                  {m.name} <span className={styles.modelIdHint}>({m.id})</span>
                </label>
              ))}
            </div>
          );
        })}
      </div>
      <div className={styles.modelAccessSummary}>
        {selected.length === 0 ? t('pages:virtualKeys.allModelsAllowed') : t('pages:virtualKeys.modelsSelected', { count: selected.length })}
      </div>
    </div>
  );
}

/* ── Props ─────────────────────────────────────────────────────────────── */

export interface VirtualKeyInfoTabProps {
  vk: VirtualKey;
  project: Project | undefined;
  modelsData: { data: AdminModelsByProvider[] } | undefined;
  projectsData: { data: Project[] } | undefined;

  // Regen key
  regenConfirming: boolean;
  setRegenConfirming: (v: boolean) => void;
  newKey: string | null;
  keyCopied: boolean;
  regenerateKey: (v: undefined) => void;
  regenerating: boolean;
  copyNewKey: () => void;
  dismissNewKey: () => void;

  // Edit state
  isEditing: boolean;
  editProjectId: string;
  setEditProjectId: (v: string) => void;
  editSourceApp: string;
  setEditSourceApp: (v: string) => void;
  editEnabled: boolean;
  setEditEnabled: (v: boolean) => void;
  editRateLimitRpm: string;
  setEditRateLimitRpm: (v: string) => void;
  editSelectedModels: VirtualKeyAllowedModelRef[];
  setEditSelectedModels: (v: VirtualKeyAllowedModelRef[]) => void;
  editExpiresAt: string;
  setEditExpiresAt: (v: string) => void;
  editNeverExpires: boolean;
  setEditNeverExpires: (v: boolean) => void;
  updating: boolean;
  handleSave: () => void;
  cancelEditing: () => void;
}

/* ── Component ─────────────────────────────────────────────────────────── */

export function VirtualKeyInfoTab(props: VirtualKeyInfoTabProps) {
  const { t } = useTranslation();
  const {
    vk, project, modelsData, projectsData,
    regenConfirming, setRegenConfirming, newKey, keyCopied,
    regenerateKey, regenerating, copyNewKey, dismissNewKey,
    isEditing,
    editProjectId, setEditProjectId,
    editSourceApp, setEditSourceApp,
    editEnabled, setEditEnabled,
    editRateLimitRpm, setEditRateLimitRpm,
    editSelectedModels, setEditSelectedModels,
    editExpiresAt, setEditExpiresAt,
    editNeverExpires, setEditNeverExpires,
    updating, handleSave, cancelEditing,
  } = props;

  if (!isEditing) {
    return (
      <Stack gap="md">
        <Card>
          <h2 className={styles.widgetTitle}>{t('pages:virtualKeys.virtualKeyInformation')}</h2>
          <div className={styles.kvGrid}>
            <div>
              <div className={styles.kvLabel}>{t('pages:virtualKeys.name')}</div>
              <div className={styles.kvValueBold}>{vk.name}</div>
            </div>
            <div>
              <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:virtualKeys.secretKey')}</span>
                <Tooltip content={t('pages:virtualKeys.secretKeyTooltip')}>
                  <span className={styles.helpIcon}>?</span>
                </Tooltip>
              </Stack>
              <div className={styles.kvValueMono}>
                {vk.keyPrefix ? `${vk.keyPrefix}••••••••` : '--'}
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:virtualKeys.project')}</div>
              <div className={styles.kvValue}>
                {project ? (
                  <Link to={`/iam/projects/${project.id}`} className={styles.link}>
                    {project.name}
                  </Link>
                ) : (
                  <span className={styles.mutedText}>--</span>
                )}
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:virtualKeys.organization')}</div>
              <div className={styles.kvValue}>{project?.organization?.name ?? '--'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:virtualKeys.sourceApp')}</div>
              <div className={styles.kvValue}>{vk.sourceApp ?? '--'}</div>
            </div>
            <div>
              <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:virtualKeys.status')}</span>
                <Tooltip content={t('pages:virtualKeys.statusTooltip')}>
                  <span className={styles.helpIcon}>?</span>
                </Tooltip>
              </Stack>
              <div className={styles.statusBadgeWrap}>
                <Badge variant={statusToVariant(vk.enabled ? 'enabled' : 'disabled')}>{vk.enabled ? t('common:enabled') : t('common:disabled')}</Badge>
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:virtualKeys.expiration')}</div>
              <div className={styles.kvValue}>
                {vk.expiresAt ? formatDate(vk.expiresAt) : t('pages:virtualKeys.never')}
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:virtualKeys.created')}</div>
              <div className={styles.kvValue}>
                {formatDate(vk.createdAt)}
              </div>
            </div>
          </div>

          {/* Regenerate Key -- inline */}
          <div className={styles.regenSection}>
            {!regenConfirming && !newKey && (
              <Button variant="secondary" onClick={() => setRegenConfirming(true)} className={styles.regenBtn}>
                {t('pages:virtualKeys.regenerateSecretKey')}
              </Button>
            )}

            {regenConfirming && (
              <div className={styles.regenConfirm}>
                <div className={styles.regenConfirmTitle}>{t('pages:virtualKeys.regenConfirmTitle')}</div>
                <div className={styles.regenConfirmDesc}>{t('pages:virtualKeys.regenConfirmDesc')}</div>
                <Stack direction="horizontal" gap="xs">
                  <Button onClick={() => regenerateKey(undefined)} loading={regenerating} className={styles.regenConfirmBtn}>
                    {t('pages:virtualKeys.confirmRegenerate')}
                  </Button>
                  <Button variant="secondary" onClick={() => setRegenConfirming(false)}>{t('common:cancel')}</Button>
                </Stack>
              </div>
            )}

            {newKey && (
              <Card className={styles.newKeyCard}>
                <div className={styles.newKeyTitle}>{t('pages:virtualKeys.newKeyGenerated')}</div>
                <div className={styles.newKeyDisplay}>
                  <span>{newKey}</span>
                  <Button variant="secondary" size="sm" onClick={copyNewKey}>
                    {keyCopied ? t('pages:virtualKeys.copied') : t('pages:virtualKeys.copy')}
                  </Button>
                </div>
                <div className={styles.newKeyWarning}>{t('pages:virtualKeys.saveKeyWarning')}</div>
                <Button variant="secondary" size="sm" onClick={dismissNewKey} className={styles.dismissBtn}>{t('pages:virtualKeys.dismiss')}</Button>
              </Card>
            )}
          </div>
        </Card>

        {/* Allowed models */}
        <Card>
          <h2 className={styles.widgetTitle}>{t('pages:virtualKeys.allowedModels')}</h2>
          <div className={styles.allowedModelsSection}>
            {vk.allowedModels && vk.allowedModels.length > 0 ? (
              (() => {
                const idToName = new Map<string, string>();
                const pidToLabel = new Map<string, string>();
                for (const group of (modelsData?.data ?? [])) {
                  pidToLabel.set(group.provider?.id, group.provider?.displayName || group.provider?.name);
                  for (const m of group.models) idToName.set(m.id, m.name);
                }
                const grouped = new Map<string, VirtualKeyAllowedModelRef[]>();
                for (const ref of vk.allowedModels) {
                  const label = pidToLabel.get(ref.providerId) ?? ref.providerId;
                  if (!grouped.has(label)) grouped.set(label, []);
                  grouped.get(label)!.push(ref);
                }
                return (
                  <div>
                    {[...grouped.entries()].map(([provName, refs]) => (
                      <div key={provName} className={styles.allowedModelGroup}>
                        <div className={styles.modelGroupLabel}>
                          {provName}
                        </div>
                        <div className={styles.chipRow}>
                          {refs.map(r => (
                            <span key={`${r.providerId}:${r.modelId}`} className={styles.chip}>
                              {idToName.get(r.modelId) ?? r.modelId}
                            </span>
                          ))}
                        </div>
                      </div>
                    ))}
                  </div>
                );
              })()
            ) : (
              <span className={styles.allModelsHint}>
                {t('pages:virtualKeys.allModelsAllowed')}
              </span>
            )}
          </div>
        </Card>
      </Stack>
    );
  }

  /* ── Edit Mode ───────────────────────────────────────────────────────── */
  return (
    <Card>
      <h2 className={`${styles.widgetTitle} ${styles.editTitle}`}>{t('pages:virtualKeys.editVirtualKey')}</h2>
      <Stack gap="md">
        <div>
          <div className={styles.kvLabel}>{t('pages:virtualKeys.nameReadOnly')}</div>
          <div className={styles.kvValueBold}>{vk.name}</div>
        </div>

        <div>
          <label className={styles.inlineLabel}>{t('pages:virtualKeys.project')}</label>
          <select value={editProjectId} onChange={e => setEditProjectId(e.target.value)}
            className={styles.nativeSelect}>
            <option value="">{t('pages:virtualKeys.none')}</option>
            {(projectsData?.data ?? []).map(p => <option key={p.id} value={p.id}>{p.name}{p.organization ? ` (${p.organization.name})` : ''}</option>)}
          </select>
        </div>

        <FormField label={t('pages:virtualKeys.sourceApp')}>
          <Input name="editSourceApp" value={editSourceApp} onChange={(e) => setEditSourceApp(e.target.value)} placeholder={t('pages:virtualKeys.placeholderSourceApp')} />
        </FormField>

        <GroupedModelSelect groups={modelsData?.data ?? []} selected={editSelectedModels} onChange={setEditSelectedModels} />

        <FormField
          label={t('pages:virtualKeys.rateLimitRpm')}
          helpText={t('pages:virtualKeys.rateLimitHelpText')}
        >
          <Input
            name="editRateLimitRpm"
            value={editRateLimitRpm}
            onChange={(e) => setEditRateLimitRpm(e.target.value)}
            type="number"
            placeholder={t('pages:virtualKeys.placeholderRpm')}
          />
        </FormField>

        <div>
          <label className={styles.inlineLabel}>{t('pages:virtualKeys.expiration')}</label>
          <Stack direction="horizontal" gap="xs" align="center">
            <Input type="date" value={editExpiresAt} onChange={e => setEditExpiresAt(e.target.value)} disabled={editNeverExpires}
              className={`${styles.inlineInput} ${styles.expirationInputFlex}`} />
            <label className={styles.neverExpiresLabel}>
              <input type="checkbox" checked={editNeverExpires} onChange={e => { setEditNeverExpires(e.target.checked); if (e.target.checked) setEditExpiresAt(''); }} />
              {t('pages:virtualKeys.neverExpires')}
            </label>
          </Stack>
        </div>

        <Stack direction="horizontal" gap="sm" align="center">
          <Switch checked={editEnabled} onCheckedChange={setEditEnabled} />
          <Tooltip content={t('pages:virtualKeys.enabledTooltip')}>
            <span>{t('pages:virtualKeys.enabledLabel')}</span>
          </Tooltip>
        </Stack>

        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={cancelEditing}>{t('common:cancel')}</Button>
          <Button onClick={handleSave} loading={updating}>
            {t('pages:virtualKeys.saveChanges')}
          </Button>
        </Stack>
      </Stack>
    </Card>
  );
}
